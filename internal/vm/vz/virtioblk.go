package vz

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// A virtio-blk device's requests (virtio 1.3, 5.2), apart from the
// framework: blkDisk serves one blkRequest, whatever carries it, and
// changes its size while requests run.

const sectorSize = 512

// Request types and statuses.
const (
	blkTypeIn          = 0
	blkTypeOut         = 1
	blkTypeFlush       = 4
	blkTypeGetID       = 8
	blkTypeDiscard     = 11
	blkTypeWriteZeroes = 13

	blkStatusOK     = 0
	blkStatusIOErr  = 1
	blkStatusUnsupp = 2
)

// The limits the device says in its configuration. A read or write
// carries at most blkSegMax segments of at most blkSizeMax bytes each,
// so blkMaxIO bounds what one request moves; a discard or write zeroes
// carries one range of at most blkMaxDiscard sectors.
const (
	blkSegMax         = 254
	blkSizeMax        = 64 << 10
	blkMaxIO          = blkSegMax * blkSizeMax
	blkMaxDiscard     = 1 << 22 // sectors: 2 GiB a request
	blkMaxDiscardSegs = 1
)

// blkConfigSize is struct virtio_blk_config's whole size, every field up
// to the zoned characteristics: a device's configuration never changes
// size, so it is the largest from the start.
const blkConfigSize = 96

// blkIDBytes is the most a GET_ID answer holds.
const blkIDBytes = 20

// blkConfig is struct virtio_blk_config for a disk of capacity bytes,
// whose holes are punched in multiples of hole bytes. It says nothing of
// the disk's blocks (no VIRTIO_BLK_F_BLK_SIZE or TOPOLOGY), and the
// guest takes 512-byte ones.
func blkConfig(capacity uint64, hole int64) []byte {
	b := make([]byte, blkConfigSize)
	le := binary.LittleEndian
	le.PutUint64(b[0:], capacity/sectorSize)
	le.PutUint32(b[8:], blkSizeMax)
	le.PutUint32(b[12:], blkSegMax)
	le.PutUint32(b[36:], blkMaxDiscard)
	le.PutUint32(b[40:], blkMaxDiscardSegs)
	// discard_sector_alignment: a discard gives back whole holes only.
	le.PutUint32(b[44:], uint32(hole/sectorSize))
	le.PutUint32(b[48:], blkMaxDiscard)
	le.PutUint32(b[52:], blkMaxDiscardSegs)
	// A write zeroes that may unmap can give blocks back.
	b[56] = 1
	return b
}

// blkRequest is one request's buffers: what the driver wrote (the read
// side, the header first) and what the device answers into (the write
// side, which ends with the status byte).
type blkRequest interface {
	// ReadAvail is how much of the read side is left.
	ReadAvail() int
	// ReadExact copies the next len(p) bytes of the read side.
	ReadExact(p []byte) error
	// ReadRest is the rest of the read side, consumed: buffers that may
	// alias the driver's memory, valid only until the request is done.
	// Their lengths add up to what ReadAvail said, or it fails.
	ReadRest() ([][]byte, error)
	// WriteAvail is how much of the write side is left.
	WriteAvail() int
	// Write appends p to the write side.
	Write(p []byte) error
}

// blkDisk is one disk image behind a device.
type blkDisk struct {
	f      *os.File
	serial string
	// capacity is what the device serves, in bytes, never more than the
	// file holds: no request reaches past it. A request reads it, and
	// does its I/O, holding the disk (holds); a shrink lowers it and then
	// waits for the requests that held the disk before, so once it is
	// done no request still works past the new end.
	capacity atomic.Uint64
	holds    holds
	// resizing makes resizes one at a time.
	resizing sync.Mutex
	// hole is what holes are punched in multiples of: the file system's
	// block.
	hole int64
}

var errBounds = errors.New("past the disk's end")

// blkChunk is what one pread moves into the guest's memory at a time.
const blkChunk = 1 << 20

var (
	chunks = sync.Pool{New: func() any { b := make([]byte, blkChunk); return &b }}
	zeroes [blkChunk]byte
)

// serve does req and writes its status, the last byte of its write side.
func (d *blkDisk) serve(req blkRequest) {
	g := d.holds.enter()
	status := d.do(req)
	d.holds.leave(g)
	// The status is the write side's last byte, whatever was written
	// before it.
	for pad := req.WriteAvail() - 1; pad > 0; pad = req.WriteAvail() - 1 {
		if req.Write(zeroes[:min(pad, len(zeroes))]) != nil {
			return
		}
	}
	if req.WriteAvail() == 1 {
		_ = req.Write([]byte{status})
	}
}

func (d *blkDisk) do(req blkRequest) byte {
	var hdr [16]byte
	if err := req.ReadExact(hdr[:]); err != nil {
		return blkStatusIOErr
	}
	typ := binary.LittleEndian.Uint32(hdr[0:])
	sector := binary.LittleEndian.Uint64(hdr[8:])
	data := req.WriteAvail() - 1
	if data < 0 {
		return blkStatusIOErr
	}
	switch typ {
	case blkTypeIn:
		if data%sectorSize != 0 || data > blkMaxIO {
			return blkStatusIOErr
		}
		off, err := d.span(sector, uint64(data))
		if err != nil {
			return blkStatusIOErr
		}
		if d.read(req, off, data) != nil {
			return blkStatusIOErr
		}
	case blkTypeOut:
		if n := req.ReadAvail(); n%sectorSize != 0 || n > blkMaxIO {
			return blkStatusIOErr
		}
		segs, err := req.ReadRest()
		if err != nil {
			return blkStatusIOErr
		}
		var n uint64
		for _, s := range segs {
			n += uint64(len(s))
		}
		off, err := d.span(sector, n)
		if err != nil {
			return blkStatusIOErr
		}
		if d.write(segs, off) != nil {
			return blkStatusIOErr
		}
	case blkTypeFlush:
		if fullSync(d.f) != nil {
			return blkStatusIOErr
		}
	case blkTypeGetID:
		id := make([]byte, min(data, blkIDBytes))
		copy(id, d.serial)
		if req.Write(id) != nil {
			return blkStatusIOErr
		}
	case blkTypeDiscard, blkTypeWriteZeroes:
		return d.ranges(req, typ)
	default:
		return blkStatusUnsupp
	}
	return blkStatusOK
}

// read moves n bytes at off into req's write side, a chunk at a time.
// A read that comes short, the file ending before the capacity does, is
// an error, never zeroes.
func (d *blkDisk) read(req blkRequest, off int64, n int) error {
	bp := chunks.Get().(*[]byte)
	defer chunks.Put(bp)
	buf := *bp
	for n > 0 {
		k := min(n, len(buf))
		got, err := d.f.ReadAt(buf[:k], off)
		if got < k {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if err := req.Write(buf[:k]); err != nil {
			return err
		}
		off, n = off+int64(k), n-k
	}
	return nil
}

// write writes segs, a request's data, at off. Segments are mostly a
// page of the guest's each, and the image is written past the Mac's page
// cache (noCache), where a pwrite of part of a page reads that page
// first: so they are copied into one buffer, a chunk at a time, and each
// chunk written with one pwrite.
func (d *blkDisk) write(segs [][]byte, off int64) error {
	if len(segs) == 1 {
		_, err := d.f.WriteAt(segs[0], off)
		return err
	}
	return d.writeGathered(segs, off)
}

// writeGathered writes segs at off a chunk at a time, each chunk copied
// whole into one buffer first.
func (d *blkDisk) writeGathered(segs [][]byte, off int64) error {
	bp := chunks.Get().(*[]byte)
	defer chunks.Put(bp)
	buf := (*bp)[:0]
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		_, err := d.f.WriteAt(buf, off)
		off += int64(len(buf))
		buf = buf[:0]
		return err
	}
	for _, s := range segs {
		for len(s) > 0 {
			k := min(len(s), cap(buf)-len(buf))
			buf, s = append(buf, s[:k]...), s[k:]
			if len(buf) == cap(buf) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

// The flags of a discard or write zeroes range.
const (
	blkFlagUnmap = 1
)

// ranges does a discard or a write zeroes: segments of struct
// virtio_blk_discard_write_zeroes, copied out of the driver's memory
// before any is looked at.
func (d *blkDisk) ranges(req blkRequest, typ uint32) byte {
	n := req.ReadAvail()
	if n == 0 || n%16 != 0 || n/16 > blkMaxDiscardSegs {
		return blkStatusIOErr
	}
	var raw [16 * blkMaxDiscardSegs]byte
	if req.ReadExact(raw[:n]) != nil {
		return blkStatusIOErr
	}
	le := binary.LittleEndian
	// Every segment is checked before any is done.
	for s := raw[:n]; len(s) > 0; s = s[16:] {
		num, flags := le.Uint32(s[8:]), le.Uint32(s[12:])
		if flags&^blkFlagUnmap != 0 || (typ == blkTypeDiscard && flags&blkFlagUnmap != 0) {
			return blkStatusUnsupp
		}
		if num > blkMaxDiscard {
			return blkStatusIOErr
		}
		if _, err := d.span(le.Uint64(s[0:]), uint64(num)*sectorSize); err != nil {
			return blkStatusIOErr
		}
	}
	for s := raw[:n]; len(s) > 0; s = s[16:] {
		off := int64(le.Uint64(s[0:]) * sectorSize)
		size := int64(le.Uint32(s[8:])) * sectorSize
		var err error
		if typ == blkTypeDiscard {
			err = d.discard(off, size)
		} else {
			err = d.zero(off, size)
		}
		if err != nil {
			return blkStatusIOErr
		}
	}
	return blkStatusOK
}

// aligned is the part of n bytes at off made of whole blocks of the
// file system, as [start, end); end <= start when there is none.
func (d *blkDisk) aligned(off, n int64) (start, end int64) {
	start = (off + d.hole - 1) / d.hole * d.hole
	end = (off + n) / d.hole * d.hole
	return start, end
}

// discard gives back the whole blocks of n bytes at off. What it leaves
// at the edges keeps its data, which a discard allows.
func (d *blkDisk) discard(off, n int64) error {
	if start, end := d.aligned(off, n); end > start {
		return punchHole(d.f, start, end-start)
	}
	return nil
}

// zero makes n bytes at off read as zeroes: the whole blocks become a
// hole, the edges are written. A volume is sparse, so a hole is what the
// device has for zeroes whether or not the driver allows an unmap.
func (d *blkDisk) zero(off, n int64) error {
	start, end := d.aligned(off, n)
	if end <= start {
		return d.writeZeroes(off, n)
	}
	if err := d.writeZeroes(off, start-off); err != nil {
		return err
	}
	if err := d.writeZeroes(end, off+n-end); err != nil {
		return err
	}
	return punchHole(d.f, start, end-start)
}

func (d *blkDisk) writeZeroes(off, n int64) error {
	for n > 0 {
		k := min(n, int64(len(zeroes)))
		if _, err := d.f.WriteAt(zeroes[:k], off); err != nil {
			return err
		}
		off, n = off+k, n-k
	}
	return nil
}

// span is the file offset of n bytes at sector, when they fit the disk.
func (d *blkDisk) span(sector, n uint64) (int64, error) {
	c := d.capacity.Load()
	if sector > c/sectorSize {
		return 0, errBounds
	}
	off := sector * sectorSize
	if n > c-off {
		return 0, errBounds
	}
	return int64(off), nil
}

// ErrNotTold is a guest that may or may not have heard of a new size:
// telling it timed out.
var ErrNotTold = errors.New("the guest did not confirm the new size in time")

// FileError is a resize that left the disk's file at a size other than
// the one it meant to: Size bytes, while the disk serves Capacity. Applied
// says whether Capacity is the new size (a shrink the guest heard of, or
// may have, whose file could not be cut: the file is longer than the
// disk, and only wastes space) or the old one (a grow the guest surely
// did not hear of, whose file could not go back: the file is longer than
// the disk, its end unused).
type FileError struct {
	Capacity, Size int64
	Applied        bool
	Err            error // what kept the file from its size
}

func (e *FileError) Error() string {
	if e.Applied {
		return fmt.Sprintf("the disk is %d bytes, but its file stays at %d bytes: %v", e.Capacity, e.Size, e.Err)
	}
	return fmt.Sprintf("the disk stays at %d bytes, but its file stays at %d bytes: %v", e.Capacity, e.Size, e.Err)
}

func (e *FileError) Unwrap() error { return e.Err }

// ResizeApplied says whether a resize that returned err left the disk at
// the new size: nil, ErrNotTold, or a FileError that says so. A caller
// records the new size as the disk's when it is true, the old one
// otherwise; a FileError (errors.As) says the file is not that size.
func ResizeApplied(err error) bool {
	var fe *FileError
	return err == nil || errors.Is(err, ErrNotTold) || errors.As(err, &fe) && fe.Applied
}

// drainWait bounds how long a shrink waits for the requests in flight.
var drainWait = 10 * time.Second

// undoDrained, for tests only, is called when a refused grow has drained
// the requests that saw the larger capacity, before the file is cut back.
var undoDrained func()

// truncate sets a disk's file's size: the tests' way to make it fail.
var truncate = (*os.File).Truncate

// resize makes the disk size bytes, rounded down to a sector, and tells
// the guest through tell. A size under one sector is refused.
//
// Growing, the file grows first and then the capacity, so the guest
// never hears of a size the device refuses. Shrinking, the capacity drops
// first and the requests that may still work past the new end finish,
// then the guest hears, and only then does the file lose its end: the
// guest has given that space up beforehand (its file system shrunk or
// unmounted).
//
// What the error says of the disk (ResizeApplied tells the first two
// from the rest):
//   - nil: capacity and file are the new size, and the guest knows it.
//   - ErrNotTold: the capacity is the new size, and the guest may or may
//     not have heard. The file is the new size too (a grown one at
//     least), unless a FileError is joined to it.
//   - a FileError with Applied: a shrink the guest heard of (joined with
//     ErrNotTold when it may not have), whose file could not be cut: the
//     capacity is the new size, the file longer.
//   - a FileError without Applied, wrapped with the guest's refusal: a
//     grow the guest surely did not hear of, whose file could not go back
//     (the requests that saw the larger capacity did not finish in
//     drainWait, or the truncate failed): the capacity is the old size,
//     the file the new one.
//   - any other: capacity and file are as they were, and the guest never
//     heard of the new size.
func (d *blkDisk) resize(size uint64, tell func(capacity uint64) error) error {
	if size < sectorSize {
		return fmt.Errorf("a size of %d bytes is less than one sector (%d)", size, sectorSize)
	}
	d.resizing.Lock()
	defer d.resizing.Unlock()
	size = size / sectorSize * sectorSize
	old := d.capacity.Load()
	if size == old {
		return nil
	}
	st, err := d.f.Stat()
	if err != nil {
		return err
	}
	was := st.Size()
	if size > old {
		if uint64(was) < size {
			if err := truncate(d.f, int64(size)); err != nil {
				return err
			}
		}
		d.capacity.Store(size)
		err = tell(size)
		if err == nil || errors.Is(err, ErrNotTold) {
			return err
		}
		// The file goes back to its size once no request that saw the
		// larger capacity still works.
		d.capacity.Store(old)
		if uint64(was) < size {
			left := func(why error) error {
				return fmt.Errorf("%w; %w", err, &FileError{Capacity: int64(old), Size: int64(size), Err: why})
			}
			if !d.holds.drain(time.Now().Add(drainWait)) {
				return left(fmt.Errorf("the requests in flight did not finish in %v", drainWait))
			}
			if undoDrained != nil {
				undoDrained()
			}
			if terr := truncate(d.f, was); terr != nil {
				return left(terr)
			}
		}
		return err
	}
	d.capacity.Store(size)
	if !d.holds.drain(time.Now().Add(drainWait)) {
		d.capacity.Store(old)
		return fmt.Errorf("the requests in flight did not finish in %v", drainWait)
	}
	err = tell(size)
	if err != nil && !errors.Is(err, ErrNotTold) {
		d.capacity.Store(old)
		return err
	}
	if terr := truncate(d.f, int64(size)); terr != nil {
		return errors.Join(err, &FileError{Capacity: int64(size), Size: was, Applied: true, Err: terr})
	}
	return err
}

// holds counts the requests that hold a disk, in two generations, so a
// shrink can wait for every request that may have seen the old capacity
// while new requests, which see the new one, never wait for anything.
//
// A request counts itself before it reads the capacity, and a shrink
// stores the capacity before it drains: a request the drain does not
// count, counting itself after the drain looked, reads the new capacity.
type holds struct {
	gen atomic.Uint64
	n   [2]atomic.Int64
}

// enter holds the disk, and says the generation to leave.
func (h *holds) enter() uint64 {
	g := h.gen.Load()
	h.n[g&1].Add(1)
	return g
}

func (h *holds) leave(g uint64) { h.n[g&1].Add(-1) }

// drain waits until deadline for every request that held the disk when
// it was called to leave, and says whether they did. Drains are one at a
// time (blkDisk.resizing).
func (h *holds) drain(deadline time.Time) bool {
	g := h.gen.Load()
	// The requests left over by an earlier drain that gave up are in the
	// slot new requests take next: they go first.
	if !h.empty(&h.n[(g+1)&1], deadline) {
		return false
	}
	h.gen.Store(g + 1)
	return h.empty(&h.n[g&1], deadline)
}

// empty waits until deadline for n to be 0.
func (h *holds) empty(n *atomic.Int64, deadline time.Time) bool {
	for pause := 50 * time.Microsecond; n.Load() != 0; pause = min(2*pause, time.Millisecond) {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(pause)
	}
	return true
}

// openBlkDisk opens the image at path for a device, which only it uses
// from then on: a disk that some other process has open through a lock
// is refused. A size above 0 is what the device serves, the file grown
// to it when it is smaller (a larger file's end is left alone, unused);
// 0 serves the file's size.
func openBlkDisk(path string, size int64) (*blkDisk, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	d, err := newBlkDisk(f, path, size)
	if err != nil {
		f.Close()
		return nil, err
	}
	return d, nil
}

func newBlkDisk(f *os.File, path string, size int64) (*blkDisk, error) {
	if err := lockFile(f); err != nil {
		if errors.Is(err, errLocked) {
			return nil, fmt.Errorf("the disk image %s is in use by another process", path)
		}
		return nil, fmt.Errorf("locking the disk image %s: %v", path, err)
	}
	hole, err := fsBlock(f)
	if err != nil {
		return nil, fmt.Errorf("the disk image %s: its file system's block: %v", path, err)
	}
	if hole < sectorSize || hole > blkChunk || hole&(hole-1) != 0 {
		return nil, fmt.Errorf("the disk image %s: its file system's block, %d bytes, is not a power of two from %d to %d", path, hole, sectorSize, blkChunk)
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fmt.Errorf("the disk image %s: a size of %d bytes", path, size)
	}
	size = size / sectorSize * sectorSize
	if size == 0 {
		size = st.Size() / sectorSize * sectorSize
	} else if st.Size() < size {
		if err := f.Truncate(size); err != nil {
			return nil, err
		}
	}
	d := &blkDisk{f: f, hole: hole}
	d.capacity.Store(uint64(size))
	return d, nil
}
