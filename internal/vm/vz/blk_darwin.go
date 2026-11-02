package vz

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"

	"github.com/bfreis/caboose/internal/vm"
)

// vmm's own virtio-blk devices, on the framework's custom virtio devices
// (macOS 27): the framework does the transport, the queue and the
// feature negotiation, and hands each request over as a queue element.
// A device's size can change while the VM runs, through its
// device-specific configuration.
//
// The device's queue only takes elements and hands them to workers,
// which serve them on threads of their own and hand them back one at a
// time (inflight). Nothing the device's queue runs waits for a worker,
// and no worker waits for the device's queue.

// The device as PCI and virtio know it.
const (
	blkDeviceID    = 2    // virtio-blk
	blkPCIClass    = 0x01 // mass storage
	blkPCISubclass = 0x00
	blkQueues      = 1
)

// The feature bits the device offers: what its configuration's limits
// need, flush, discard and write zeroes.
const (
	blkFeatureSizeMax     = 1 << 1
	blkFeatureSegMax      = 1 << 2
	blkFeatureFlush       = 1 << 9
	blkFeatureDiscard     = 1 << 13
	blkFeatureWriteZeroes = 1 << 14

	blkFeatures = blkFeatureSizeMax | blkFeatureSegMax | blkFeatureFlush | blkFeatureDiscard | blkFeatureWriteZeroes
)

// blkWorkers is how many requests a device serves at once.
const blkWorkers = 8

// How long a reset, a stop or a pause waits for the requests in flight,
// and how long the guest has to take a new size.
var (
	blkQuiesce   = 5 * time.Second
	blkTellAfter = 5 * time.Second
)

var (
	blkOnce  sync.Once
	blkErr   error
	cfgClass objc.Class // the configuration's delegate
	devClass objc.Class // the device's delegate

	// blkByDelegate finds a device's blkDev from either delegate.
	blkByDelegate sync.Map
)

// blkDev is one of vmm's devices.
type blkDev struct {
	*blkDisk
	index  int
	queue  uintptr        // the device's dispatch queue
	cfgDel objc.ID        // the configuration's delegate
	device atomic.Uintptr // the VZCustomVirtioDevice, once made
	life   *inflight
	work   *workQueue[blkJob]

	notes atomic.Int64
	// held are elements that came while the device was paused, retained,
	// for resume. Only the device's queue touches it.
	held []objc.ID
	// stopped is set once the device stopped: nothing tells its guest of
	// a size after.
	stopped atomic.Bool
}

// blkJob is an element taken from the queue, retained until it is
// handed back.
type blkJob struct {
	el    objc.ID
	epoch uint64
}

func loadBlk() error {
	blkOnce.Do(func() { blkErr = doLoadBlk() })
	return blkErr
}

func doLoadBlk() error {
	for _, c := range []string{"VZCustomVirtioDeviceConfiguration", "VZCustomVirtioDeviceDelegateProvider", "VZVirtioDeviceSpecificConfiguration"} {
		if objc.GetClass(c) == 0 {
			return fmt.Errorf("this macOS has no %s: block devices need macOS 27 or later", c)
		}
	}
	protos := func(name string) []*objc.Protocol {
		if p := objc.GetProtocol(name); p != nil {
			return []*objc.Protocol{p}
		}
		return nil
	}
	var err error
	cfgClass, err = objc.RegisterClass("CabooseBlkConfigDelegate", objc.GetClass("NSObject"),
		protos("VZCustomVirtioDeviceConfigurationDelegate"), nil,
		[]objc.MethodDef{
			{Cmd: sel("customVirtioConfiguration:didCreateDevice:"), Fn: func(self objc.ID, _ objc.SEL, _, device objc.ID) {
				d := lookupBlk(self)
				if d == nil {
					return
				}
				device.Send(sel("retain"))
				del := objc.ID(devClass).Send(sel("new"))
				blkByDelegate.Store(del, d)
				device.Send(sel("setDelegate:"), del)
				d.device.Store(uintptr(device))
				logf("blk%d: device made", d.index)
			}},
		})
	if err != nil {
		return fmt.Errorf("registering the block device's delegate: %v", err)
	}
	devClass, err = objc.RegisterClass("CabooseBlkDeviceDelegate", objc.GetClass("NSObject"),
		protos("VZCustomVirtioDeviceDelegate"), nil,
		[]objc.MethodDef{
			{Cmd: sel("customVirtioDevice:didReceiveNotificationForQueue:"), Fn: func(self objc.ID, _ objc.SEL, _, queue objc.ID) {
				if d := lookupBlk(self); d != nil {
					d.drain(queue)
				}
			}},
			{Cmd: sel("customVirtioDeviceDidAcceptDriverOk:"), Fn: func(self objc.ID, _ objc.SEL, device objc.ID) {
				d := lookupBlk(self)
				if d == nil {
					return
				}
				f := device.Send(sel("negotiatedFeatures"))
				var lo, hi uint32
				if f != 0 {
					lo, hi = objc.Send[uint32](f, sel("subset0")), objc.Send[uint32](f, sel("subset1"))
				}
				size := -1
				if q := device.Send(sel("queueAtIndex:"), uint16(0)); q != 0 {
					size = int(objc.Send[uint16](q, sel("queueSize")))
				}
				d.life.start()
				logf("blk%d: driver ok: negotiated features %#08x_%08x (offered %#x), queue 0 size %d", d.index, hi, lo, blkFeatures, size)
			}},
			{Cmd: sel("customVirtioDeviceWillReset:"), Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
				if d := lookupBlk(self); d != nil {
					was, left := d.end()
					logf("blk%d: reset with %d in flight, %d left unreturned; %s", d.index, was, left, d.stats())
				}
			}},
			{Cmd: sel("customVirtioDeviceWillStop:"), Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
				if d := lookupBlk(self); d != nil {
					d.stopped.Store(true)
					was, left := d.end()
					d.closeWork()
					logf("blk%d: stop with %d in flight, %d left unreturned; %s", d.index, was, left, d.stats())
				}
			}},
			{Cmd: sel("customVirtioDeviceWillPause:"), Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
				if d := lookupBlk(self); d != nil {
					logf("blk%d: pause, %d in flight", d.index, d.life.pause(blkQuiesce))
				}
			}},
			{Cmd: sel("customVirtioDeviceWillResume:"), Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
				if d := lookupBlk(self); d != nil {
					d.life.resume()
					held := d.held
					d.held = nil
					for _, el := range held {
						d.enqueue(el)
					}
					logf("blk%d: resume, %d held", d.index, len(held))
				}
			}},
		})
	if err != nil {
		return fmt.Errorf("registering the block device's delegate: %v", err)
	}
	return nil
}

func lookupBlk(delegate objc.ID) *blkDev {
	if v, ok := blkByDelegate.Load(delegate); ok {
		return v.(*blkDev)
	}
	return nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s caboose-vmm: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// blkConfigurations are the configurations of m's block devices, and the
// devices they will make. The configurations are the caller's to
// release; on an error nothing is left open.
func blkConfigurations(m vm.Machine) (cfgs []objc.ID, devs []*blkDev, err error) {
	if len(m.BlockDevices) == 0 {
		return nil, nil, nil
	}
	if err := loadBlk(); err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			for _, c := range cfgs {
				c.Send(sel("release"))
			}
			closeBlk(devs)
			freeBlk(devs)
			cfgs, devs = nil, nil
		}
	}()
	for i, b := range m.BlockDevices {
		if len(b.Serial) > blkIDBytes {
			return cfgs, devs, fmt.Errorf("block device %s: its serial %q is over %d bytes", b.Path, b.Serial, blkIDBytes)
		}
		disk, err := openBlkDisk(b.Path, b.Size)
		if err != nil {
			return cfgs, devs, fmt.Errorf("block device %q: %v", b.Serial, err)
		}
		d := &blkDev{blkDisk: disk, index: i, life: newInflight(), work: newWorkQueue[blkJob]()}
		devs = append(devs, d)
		d.serial = b.Serial
		// Past the Mac's page cache: the guest caches what it reads
		// already, as it does on the framework's own disks, which go
		// past it too, and a flush then has only the disk's cache to
		// empty.
		if err := noCache(d.f); err != nil {
			return cfgs, devs, fmt.Errorf("block device %q: F_NOCACHE_EXT on %s: %v", b.Serial, b.Path, err)
		}
		d.queue = dispatchQueueCreate(fmt.Sprintf("caboose.blk%d", i), 0)

		c := class("VZCustomVirtioDeviceConfiguration").Send(sel("new"))
		cfgs = append(cfgs, c)
		c.Send(sel("setDeviceID:"), uint16(blkDeviceID))
		c.Send(sel("setPCIClassID:"), uint8(blkPCIClass))
		c.Send(sel("setPCISubclassID:"), uint8(blkPCISubclass))
		c.Send(sel("setVirtioQueueCount:"), uint16(blkQueues))
		c.Send(sel("optionalFeatures")).Send(sel("setSubset0:"), uint32(blkFeatures))
		spec := specificConfig(d.config(d.capacity.Load()))
		c.Send(sel("setDeviceSpecificConfiguration:"), spec)
		spec.Send(sel("release"))
		del := objc.ID(cfgClass).Send(sel("new"))
		d.cfgDel = del
		blkByDelegate.Store(del, d)
		prov := class("VZCustomVirtioDeviceDelegateProvider").Send(sel("alloc")).Send(sel("initWithDeviceQueue:delegate:"), d.queue, del)
		if prov == 0 {
			return cfgs, devs, errors.New("Virtualization.framework made no provider for a block device")
		}
		c.Send(sel("setProvider:"), prov)
		prov.Send(sel("release"))
		for range blkWorkers {
			go d.worker()
		}
		logf("blk%d: %s, %d bytes, holes of %d, serial %q", i, b.Path, d.capacity.Load(), d.hole, b.Serial)
	}
	return cfgs, devs, nil
}

// closeBlk ends devs' workers and closes their images, for devices no VM
// runs. What a failure lets go of depends on when it happens: before the
// configurations reach the framework, blkConfigurations closes the images
// and frees the delegates and queues too (freeBlk); after (a configuration
// that does not validate, a VM not made or not started), Start and
// configure close the images only, and the delegates and queues, which
// the framework's configuration may still use, are kept until vmm, whose
// start failed, exits; a start that times out
// leaves the images open, since the VM may still start and serve them.
func closeBlk(devs []*blkDev) {
	for _, d := range devs {
		d.closeWork()
		d.f.Close()
	}
}

// freeBlk lets go of what devs' configurations were made with, their
// delegates and dispatch queues, for devices whose configurations never
// reached the framework.
func freeBlk(devs []*blkDev) {
	for _, d := range devs {
		if d.cfgDel != 0 {
			blkByDelegate.Delete(d.cfgDel)
			d.cfgDel.Send(sel("release"))
			d.cfgDel = 0
		}
		if d.queue != 0 {
			dispatchRelease(d.queue)
			d.queue = 0
		}
	}
}

// config is d's struct virtio_blk_config at capacity bytes.
func (d *blkDev) config(capacity uint64) []byte { return blkConfig(capacity, d.hole) }

// specificConfig is b as a device-specific configuration, retained: the
// caller releases it.
func specificConfig(b []byte) objc.ID {
	data := class("NSData").Send(sel("dataWithBytes:length:"), unsafe.Pointer(&b[0]), uint(len(b)))
	return class("VZVirtioDeviceSpecificConfiguration").Send(sel("alloc")).Send(sel("initWithConfigurationData:"), data)
}

// drain takes every element the queue has, as the framework asks: it
// notifies again only once nextElement said there was none. It runs on
// the device's queue and never waits for a worker.
func (d *blkDev) drain(queue objc.ID) {
	pool := class("NSAutoreleasePool").Send(sel("new"))
	defer pool.Send(sel("drain"))
	d.notes.Add(1)
	for {
		el := queue.Send(sel("nextElement"))
		if el == 0 {
			return
		}
		if j, ok := d.takeElement(el); ok {
			d.work.put(j)
		}
	}
}

// takeElement makes el, which the drain just found, a job when the device
// takes work. A paused device holds it, retained, for resume; one that
// takes no work, before the driver is ready or once reset or stopped,
// leaves it alone, never to be returned.
func (d *blkDev) takeElement(el objc.ID) (blkJob, bool) {
	epoch, st := d.life.take()
	switch st {
	case takeHold:
		el.Send(sel("retain"))
		d.held = append(d.held, el)
		return blkJob{}, false
	case takeNo:
		d.life.drop(1)
		logf("blk%d: an element arrived while the device takes no work; left alone", d.index)
		return blkJob{}, false
	}
	el.Send(sel("retain"))
	return blkJob{el: el, epoch: epoch}, true
}

// enqueue takes el, retained while the device was paused, as the drain
// would: on the device's queue.
func (d *blkDev) enqueue(el objc.ID) {
	j, ok := d.takeElement(el)
	el.Send(sel("release"))
	if ok {
		d.work.put(j)
	}
}

// end ends the epoch (inflight.end), and lets go of what a pause held.
// On the device's queue.
func (d *blkDev) end() (was, left int) {
	was, left = d.life.end(blkQuiesce, func(busy int) {
		logf("blk%d: still waiting for %d requests being served", d.index, busy)
	})
	for _, el := range d.held {
		el.Send(sel("release"))
	}
	d.life.drop(len(d.held))
	d.held = nil
	return was, left
}

// closeWork ends the threads that serve d once they ran out of work.
func (d *blkDev) closeWork() { d.work.close() }

// worker serves elements off the device's queue and hands them back. It
// keeps its thread, as an autorelease pool is drained on the thread that
// made it.
func (d *blkDev) worker() {
	runtime.LockOSThread()
	for {
		j, ok := d.work.get()
		if !ok {
			return
		}
		pool := class("NSAutoreleasePool").Send(sel("new"))
		if d.life.begin(j.epoch) {
			d.serve(vzElement(j.el))
			d.life.finish(j.epoch, func() { j.el.Send(sel("returnToQueue")) })
		}
		j.el.Send(sel("release"))
		pool.Send(sel("drain"))
	}
}

func (d *blkDev) stats() string {
	took, returned, dropped, now, most := d.life.counts()
	return fmt.Sprintf("%d notifications, %d elements, %d returned, %d dropped, %d in flight (at most %d)",
		d.notes.Load(), took, returned, dropped, now, most)
}

// tell has the guest read a new capacity. A timeout is ErrNotTold: the
// update may still land.
func (d *blkDev) tell(capacity uint64) error {
	dev := objc.ID(d.device.Load())
	if dev == 0 {
		return errors.New("the device was not made")
	}
	done := make(chan error, 1)
	block := objc.NewBlock(func(_ objc.Block, err objc.ID) {
		if err != 0 {
			done <- errors.New(describe(err))
			return
		}
		done <- nil
	})
	go onQueue(d.queue, func() {
		// The device's stop runs on this queue too: once it has, the
		// guest is gone, and the update is never made.
		if d.stopped.Load() {
			done <- errors.New("the device stopped")
			return
		}
		pool := class("NSAutoreleasePool").Send(sel("new"))
		spec := specificConfig(d.config(capacity))
		dev.Send(sel("updateDeviceSpecificConfiguration:completionHandler:"), spec, block)
		spec.Send(sel("release"))
		pool.Send(sel("drain"))
	})
	select {
	case err := <-done:
		block.Release()
		return err
	case <-time.After(blkTellAfter):
		// The block stays: the framework may still call it.
		return fmt.Errorf("%w (%v)", ErrNotTold, blkTellAfter)
	}
}

// ResizeDisk makes block device i size bytes (rounded down to a sector)
// while the VM runs, and has the guest read the new size; one resize runs
// at a time. Growing, the file grows first; shrinking, the guest must
// have given the end up already (its file system shrunk, or unmounted),
// and the file is cut once the guest knows. A size under one sector is
// refused. What the error says of the disk is blkDisk.resize's: when
// ResizeApplied(err), the disk has the new size (the guest may not have
// heard, with ErrNotTold), and a caller records it as the disk's;
// otherwise it keeps the old size and the guest never heard of the new
// one. Either way, a FileError (errors.As) says the file was left at
// another size than the disk's, and which.
func (r *Runner) ResizeDisk(i int, size int64) error {
	r.resizing.Lock()
	defer r.resizing.Unlock()
	if i < 0 || i >= len(r.blk) {
		return fmt.Errorf("there is no block device %d", i)
	}
	if size < sectorSize {
		return fmt.Errorf("block device %d: a size of %d bytes, less than one sector (%d)", i, size, sectorSize)
	}
	d := r.blk[i]
	t0, old := time.Now(), d.capacity.Load()
	err := d.resize(uint64(size), d.tell)
	logf("blk%d: capacity %d -> %d bytes in %v: %v; %s", d.index, old, d.capacity.Load(), time.Since(t0), err, d.stats())
	return err
}

// DiskSize is block device i's capacity in bytes: what the guest may use
// of it.
func (r *Runner) DiskSize(i int) (int64, error) {
	if i < 0 || i >= len(r.blk) {
		return 0, fmt.Errorf("there is no block device %d", i)
	}
	return int64(r.blk[i].capacity.Load()), nil
}

// vzElement is a VZVirtioQueueElement as a blkRequest.
type vzElement objc.ID

func (e vzElement) ReadAvail() int {
	return int(objc.Send[uint](objc.ID(e), sel("readBuffersAvailableByteCount")))
}

func (e vzElement) ReadExact(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	var nserr objc.ID
	if !objc.Send[bool](objc.ID(e), sel("readBytesIntoBuffer:exactLength:error:"), unsafe.Pointer(&p[0]), uint(len(p)), unsafe.Pointer(&nserr)) {
		return errors.New(describe(nserr))
	}
	return nil
}

func (e vzElement) ReadRest() ([][]byte, error) {
	want := e.ReadAvail()
	arr := objc.ID(e).Send(sel("readBuffers"))
	n := objc.Send[uint](arr, sel("count"))
	out := make([][]byte, 0, n)
	total := 0
	for i := range n {
		data := arr.Send(sel("objectAtIndex:"), i)
		l := objc.Send[uint](data, sel("length"))
		if l == 0 {
			continue
		}
		total += int(l)
		out = append(out, unsafe.Slice((*byte)(objc.Send[unsafe.Pointer](data, sel("bytes"))), l))
	}
	if total != want {
		return nil, fmt.Errorf("the read buffers hold %d bytes, not the %d left", total, want)
	}
	return out, nil
}

func (e vzElement) WriteAvail() int {
	return int(objc.Send[uint](objc.ID(e), sel("writeBuffersAvailableByteCount")))
}

func (e vzElement) Write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	var nserr objc.ID
	if !objc.Send[bool](objc.ID(e), sel("writeBuffer:exactLength:error:"), unsafe.Pointer(&p[0]), uint(len(p)), unsafe.Pointer(&nserr)) {
		return errors.New(describe(nserr))
	}
	return nil
}
