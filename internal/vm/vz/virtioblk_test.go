package vz

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeReq is a request over plain buffers: the read side as segments,
// the write side as wcap bytes.
type fakeReq struct {
	read  [][]byte
	wcap  int
	wrote []byte
	// badRest has ReadRest fail, as buffers that do not add up do.
	badRest bool
	// hold, when set, is waited for before ReadRest answers: a request
	// caught before its I/O.
	hold chan struct{}
	// holdWrite, when set, is waited for before the second Write: a
	// read of more than a chunk caught in its I/O, past its bounds check.
	holdWrite chan struct{}
	writes    int
}

func (r *fakeReq) ReadAvail() int {
	n := 0
	for _, s := range r.read {
		n += len(s)
	}
	return n
}

func (r *fakeReq) ReadExact(p []byte) error {
	if r.ReadAvail() < len(p) {
		return errors.New("short")
	}
	for k := 0; k < len(p); {
		c := copy(p[k:], r.read[0])
		k += c
		if r.read[0] = r.read[0][c:]; len(r.read[0]) == 0 {
			r.read = r.read[1:]
		}
	}
	return nil
}

func (r *fakeReq) ReadRest() ([][]byte, error) {
	if r.hold != nil {
		<-r.hold
	}
	if r.badRest {
		return nil, errors.New("the buffers do not add up")
	}
	rest := r.read
	r.read = nil
	return rest, nil
}

func (r *fakeReq) WriteAvail() int { return r.wcap - len(r.wrote) }

func (r *fakeReq) Write(p []byte) error {
	if r.writes++; r.writes == 2 && r.holdWrite != nil {
		<-r.holdWrite
	}
	if len(p) > r.WriteAvail() {
		return errors.New("full")
	}
	r.wrote = append(r.wrote, p...)
	return nil
}

func header(typ uint32, sector uint64) []byte {
	h := make([]byte, 16)
	binary.LittleEndian.PutUint32(h[0:], typ)
	binary.LittleEndian.PutUint64(h[8:], sector)
	return h
}

func seg(sector uint64, num, flags uint32) []byte {
	s := make([]byte, 16)
	binary.LittleEndian.PutUint64(s[0:], sector)
	binary.LittleEndian.PutUint32(s[8:], num)
	binary.LittleEndian.PutUint32(s[12:], flags)
	return s
}

func testDisk(t *testing.T, size int64) *blkDisk {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	d := &blkDisk{f: f, serial: "probe", hole: 4096}
	d.capacity.Store(uint64(size))
	return d
}

func status(r *fakeReq) byte { return r.wrote[len(r.wrote)-1] }

// out is a write of segs at sector.
func out(sector uint64, segs ...[]byte) *fakeReq {
	return &fakeReq{read: append([][]byte{header(blkTypeOut, sector)}, segs...), wcap: 1}
}

// in is a read of n bytes at sector.
func in(sector uint64, n int) *fakeReq {
	return &fakeReq{read: [][]byte{header(blkTypeIn, sector)}, wcap: n + 1}
}

func ranges(typ uint32, segs ...[]byte) *fakeReq {
	return &fakeReq{read: append([][]byte{header(typ, 0)}, segs...), wcap: 1}
}

func fileBytes(t *testing.T, d *blkDisk, off, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := d.f.ReadAt(b, int64(off)); err != nil {
		t.Fatal(err)
	}
	return b
}

func allocated(t *testing.T, d *blkDisk) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(d.f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func fill(d *blkDisk, t *testing.T, c byte) {
	t.Helper()
	st, _ := d.f.Stat()
	if _, err := d.f.WriteAt(bytes.Repeat([]byte{c}, int(st.Size())), 0); err != nil {
		t.Fatal(err)
	}
}

// A write of several segments lands in order, and reads back.
func TestBlkWriteRead(t *testing.T) {
	d := testDisk(t, 8*sectorSize)
	a := bytes.Repeat([]byte("a"), sectorSize)
	b := bytes.Repeat([]byte("b"), 2*sectorSize)
	w := out(3, a, b)
	d.serve(w)
	if status(w) != blkStatusOK {
		t.Fatalf("write status %d", status(w))
	}
	r := in(3, 3*sectorSize)
	d.serve(r)
	if want := append(a, b...); status(r) != blkStatusOK || !bytes.Equal(r.wrote[:len(want)], want) {
		t.Fatalf("read status %d, data equal %v", status(r), bytes.Equal(r.wrote[:len(want)], want))
	}
	// A header and data in one buffer read the same.
	one := &fakeReq{read: [][]byte{append(header(blkTypeOut, 0), a...)}, wcap: 1}
	d.serve(one)
	if status(one) != blkStatusOK || !bytes.Equal(fileBytes(t, d, 0, sectorSize), a) {
		t.Errorf("header and data in one buffer: %d", status(one))
	}
}

func TestBlkBounds(t *testing.T) {
	d := testDisk(t, 8*sectorSize)
	for _, c := range []struct {
		name   string
		sector uint64
		n      int
	}{
		{"last sector and one more", 7, 2 * sectorSize},
		{"past the end", 8, sectorSize},
		{"far past the end", 1 << 60, sectorSize},
		{"wrapping past the end", 1<<64/sectorSize - 1, 2 * sectorSize},
	} {
		w := out(c.sector, make([]byte, c.n))
		d.serve(w)
		r := in(c.sector, c.n)
		d.serve(r)
		if status(w) != blkStatusIOErr || status(r) != blkStatusIOErr || len(r.wrote) != c.n+1 {
			t.Errorf("%s: write %d, read %d (%d bytes)", c.name, status(w), status(r), len(r.wrote))
		}
	}
	if st, _ := d.f.Stat(); st.Size() != 8*sectorSize {
		t.Errorf("the file grew to %d", st.Size())
	}
	ok := in(7, sectorSize)
	d.serve(ok)
	w := out(7, make([]byte, sectorSize))
	d.serve(w)
	if status(ok) != blkStatusOK || status(w) != blkStatusOK {
		t.Errorf("the last sector: read %d, write %d", status(ok), status(w))
	}
}

// Reads and writes are whole sectors, and no more than blkMaxIO.
func TestBlkSizes(t *testing.T) {
	d := testDisk(t, 2*blkMaxIO)
	for name, r := range map[string]*fakeReq{
		"read of half a sector":  in(0, sectorSize/2),
		"write of half a sector": out(0, make([]byte, sectorSize/2)),
		"read over the limit":    in(0, blkMaxIO+sectorSize),
		"write over the limit":   out(0, make([]byte, blkMaxIO), make([]byte, sectorSize)),
	} {
		d.serve(r)
		if status(r) != blkStatusIOErr {
			t.Errorf("%s: %d", name, status(r))
		}
	}
	for name, r := range map[string]*fakeReq{
		"read at the limit":  in(0, blkMaxIO),
		"write at the limit": out(0, make([]byte, blkMaxIO)),
	} {
		d.serve(r)
		if status(r) != blkStatusOK {
			t.Errorf("%s: %d", name, status(r))
		}
	}
}

// A file shorter than the capacity is an error, never zeroes.
func TestBlkShortRead(t *testing.T) {
	d := testDisk(t, 4*sectorSize)
	d.capacity.Store(8 * sectorSize)
	r := in(3, 2*sectorSize)
	d.serve(r)
	if status(r) != blkStatusIOErr {
		t.Errorf("a read past the file's end: %d", status(r))
	}
}

// Malformed requests fail, and still get their status at the end.
func TestBlkMalformed(t *testing.T) {
	d := testDisk(t, 8*sectorSize)
	short := &fakeReq{read: [][]byte{header(blkTypeIn, 0)[:15]}, wcap: sectorSize + 1}
	d.serve(short)
	if len(short.wrote) != sectorSize+1 || status(short) != blkStatusIOErr {
		t.Errorf("a short header: %d bytes, status %d", len(short.wrote), status(short))
	}
	none := &fakeReq{read: [][]byte{header(blkTypeIn, 0)}}
	d.serve(none) // nowhere to answer: nothing to do, nothing to break
	if len(none.wrote) != 0 {
		t.Errorf("no write side, %d bytes written", len(none.wrote))
	}
	bad := out(0, make([]byte, sectorSize))
	bad.badRest = true
	if _, err := d.f.WriteAt(bytes.Repeat([]byte{7}, sectorSize), 0); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, d, 0, sectorSize)
	d.serve(bad)
	if status(bad) != blkStatusIOErr || !bytes.Equal(fileBytes(t, d, 0, sectorSize), before) {
		t.Errorf("buffers that do not add up: %d", status(bad))
	}
}

func TestBlkFlush(t *testing.T) {
	d := testDisk(t, sectorSize)
	r := &fakeReq{read: [][]byte{header(blkTypeFlush, 0)}, wcap: 1}
	d.serve(r)
	if status(r) != blkStatusOK {
		t.Errorf("flush: %d", status(r))
	}
	d.f.Close()
	r = &fakeReq{read: [][]byte{header(blkTypeFlush, 0)}, wcap: 1}
	d.serve(r)
	if status(r) != blkStatusIOErr {
		t.Errorf("flush of a closed file: %d", status(r))
	}
}

func TestBlkGetIDAndUnsupported(t *testing.T) {
	d := testDisk(t, sectorSize)
	r := &fakeReq{read: [][]byte{header(blkTypeGetID, 0)}, wcap: blkIDBytes + 1}
	d.serve(r)
	if status(r) != blkStatusOK || string(bytes.TrimRight(r.wrote[:blkIDBytes], "\x00")) != "probe" {
		t.Errorf("get id: %q", r.wrote)
	}
	u := &fakeReq{read: [][]byte{header(99, 0)}, wcap: 1}
	d.serve(u)
	if status(u) != blkStatusUnsupp {
		t.Errorf("unknown type: %d", status(u))
	}
}

func TestBlkConfig(t *testing.T) {
	c := blkConfig(3<<30, 16384)
	le := binary.LittleEndian
	if len(c) != blkConfigSize || le.Uint64(c) != 3<<30/sectorSize {
		t.Errorf("config %d bytes, capacity %d", len(c), le.Uint64(c))
	}
	if le.Uint32(c[44:]) != 16384/sectorSize {
		t.Errorf("discard_sector_alignment %d, not the hole's 32 sectors", le.Uint32(c[44:]))
	}
	if le.Uint32(c[8:]) != blkSizeMax || le.Uint32(c[12:]) != blkSegMax || le.Uint32(c[40:]) != blkMaxDiscardSegs || c[56] != 1 {
		t.Errorf("size_max %d seg_max %d max_discard_seg %d write_zeroes_may_unmap %d", le.Uint32(c[8:]), le.Uint32(c[12:]), le.Uint32(c[40:]), c[56])
	}
	// blk_size and the topology say nothing: the device offers neither.
	if !bytes.Equal(c[20:36], make([]byte, 16)) {
		t.Errorf("block sizes said: % x", c[20:36])
	}
}

// A write of segments that cross a chunk, gathered into one buffer a
// chunk at a time, lands as it would segment by segment.
func TestBlkWriteGathered(t *testing.T) {
	const size = 3 * blkChunk
	d := testDisk(t, size)
	segs := [][]byte{
		bytes.Repeat([]byte("a"), sectorSize),
		bytes.Repeat([]byte("b"), blkChunk),
		bytes.Repeat([]byte("c"), 3*sectorSize),
	}
	w := out(5, segs...)
	d.serve(w)
	if status(w) != blkStatusOK {
		t.Fatalf("status %d", status(w))
	}
	want := bytes.Join(segs, nil)
	if got := fileBytes(t, d, 5*sectorSize, len(want)); !bytes.Equal(got, want) {
		t.Errorf("the file holds other bytes")
	}
}

// Discard and write zeroes refuse what is malformed or out of bounds,
// and then touch nothing.
func TestBlkRangesRefused(t *testing.T) {
	const size = 64 << 10
	d := testDisk(t, size)
	fill(d, t, 0xaa)
	for _, typ := range []uint32{blkTypeDiscard, blkTypeWriteZeroes} {
		for _, c := range []struct {
			name   string
			req    *fakeReq
			status byte
		}{
			{"no segment", ranges(typ), blkStatusIOErr},
			{"a partial segment", ranges(typ, seg(0, 8, 0)[:12]), blkStatusIOErr},
			{"two segments", ranges(typ, seg(0, 8, 0), seg(16, 8, 0)), blkStatusIOErr},
			{"past the end", ranges(typ, seg(size/sectorSize-8, 16, 0)), blkStatusIOErr},
			{"far past the end", ranges(typ, seg(1<<60, 8, 0)), blkStatusIOErr},
			{"over the limit", ranges(typ, seg(0, blkMaxDiscard+1, 0)), blkStatusIOErr},
			{"a reserved flag", ranges(typ, seg(0, 8, 2)), blkStatusUnsupp},
		} {
			d.serve(c.req)
			if status(c.req) != c.status {
				t.Errorf("type %d, %s: %d, want %d", typ, c.name, status(c.req), c.status)
			}
		}
	}
	unmap := ranges(blkTypeDiscard, seg(0, 8, blkFlagUnmap))
	d.serve(unmap)
	if status(unmap) != blkStatusUnsupp {
		t.Errorf("a discard with unmap: %d", status(unmap))
	}
	if !bytes.Equal(fileBytes(t, d, 0, size), bytes.Repeat([]byte{0xaa}, size)) {
		t.Error("a refused request changed the disk")
	}
	if st, _ := d.f.Stat(); st.Size() != size {
		t.Errorf("the file is %d bytes", st.Size())
	}
}

// A range is at most blkMaxDiscard sectors.
func TestBlkRangeLimit(t *testing.T) {
	d := testDisk(t, 2*blkMaxDiscard*sectorSize)
	for _, typ := range []uint32{blkTypeDiscard, blkTypeWriteZeroes} {
		over := ranges(typ, seg(0, blkMaxDiscard+1, 0))
		d.serve(over)
		at := ranges(typ, seg(0, blkMaxDiscard, 0))
		d.serve(at)
		if status(over) != blkStatusIOErr || status(at) != blkStatusOK {
			t.Errorf("type %d: over the limit %d, at it %d", typ, status(over), status(at))
		}
	}
}

// A discard gives back the whole blocks it covers, and leaves the rest.
func TestBlkDiscard(t *testing.T) {
	const size = 64 << 10
	d := testDisk(t, size)
	fill(d, t, 0xaa)
	before := allocated(t, d)
	// Sectors 4 ... 36: blocks 1 ... 3 whole, half of 0 and of 4.
	r := ranges(blkTypeDiscard, seg(4, 32, 0))
	d.serve(r)
	if status(r) != blkStatusOK {
		t.Fatalf("discard: %d", status(r))
	}
	got := fileBytes(t, d, 0, size)
	if !bytes.Equal(got[int(d.hole):4*int(d.hole)], make([]byte, 3*int(d.hole))) {
		t.Error("the whole blocks still hold data")
	}
	if !bytes.Equal(got[:int(d.hole)], bytes.Repeat([]byte{0xaa}, int(d.hole))) || !bytes.Equal(got[4*int(d.hole):], bytes.Repeat([]byte{0xaa}, size-4*int(d.hole))) {
		t.Error("a discard reached past its whole blocks")
	}
	if after := allocated(t, d); after > before-3*d.hole {
		t.Errorf("allocated %d -> %d", before, after)
	}
}

// A write zeroes reads as zeroes over all of its range, holes in the
// middle, the edges written.
func TestBlkWriteZeroes(t *testing.T) {
	const size = 64 << 10
	for _, flags := range []uint32{0, blkFlagUnmap} {
		d := testDisk(t, size)
		fill(d, t, 0xaa)
		before := allocated(t, d)
		for _, s := range [][2]uint64{{4, 32}, {49, 2}} { // 2 KiB ... 18 KiB, and inside one block
			r := ranges(blkTypeWriteZeroes, seg(s[0], uint32(s[1]), flags))
			d.serve(r)
			if status(r) != blkStatusOK {
				t.Fatalf("write zeroes %v: %d", s, status(r))
			}
		}
		want := bytes.Repeat([]byte{0xaa}, size)
		copy(want[4*sectorSize:36*sectorSize], make([]byte, 32*sectorSize))
		copy(want[49*sectorSize:51*sectorSize], make([]byte, 2*sectorSize))
		if !bytes.Equal(fileBytes(t, d, 0, size), want) {
			t.Errorf("flags %d: the disk is not zero just where asked", flags)
		}
		if after := allocated(t, d); after > before-3*d.hole {
			t.Errorf("flags %d: allocated %d -> %d", flags, before, after)
		}
	}
}

// Each request checks the capacity it finds.
func TestBlkCapacityChange(t *testing.T) {
	d := testDisk(t, 8*sectorSize)
	r := in(6, sectorSize)
	d.serve(r)
	d.capacity.Store(4 * sectorSize)
	r2 := in(6, sectorSize)
	d.serve(r2)
	if status(r) != blkStatusOK || status(r2) != blkStatusIOErr {
		t.Errorf("before %d, after %d", status(r), status(r2))
	}
}

// Growing: the file, then the capacity, then the guest; a guest that
// surely did not hear keeps the old size.
func TestBlkResizeGrow(t *testing.T) {
	d := testDisk(t, 8*sectorSize)
	var told []uint64
	tell := func(c uint64) error {
		st, _ := d.f.Stat()
		if d.capacity.Load() != c || uint64(st.Size()) < c {
			t.Errorf("told %d with the capacity at %d, the file at %d", c, d.capacity.Load(), st.Size())
		}
		told = append(told, c)
		return nil
	}
	if err := d.resize(16*sectorSize+100, tell); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.f.Stat(); st.Size() != 16*sectorSize || d.capacity.Load() != 16*sectorSize || len(told) != 1 {
		t.Errorf("file %d, capacity %d, told %v", st.Size(), d.capacity.Load(), told)
	}
	if err := d.resize(32*sectorSize, func(uint64) error { return errors.New("refused") }); err == nil || d.capacity.Load() != 16*sectorSize {
		t.Errorf("a refused update: %v, capacity %d", err, d.capacity.Load())
	}
	if err := d.resize(32*sectorSize, func(uint64) error { return ErrNotTold }); !errors.Is(err, ErrNotTold) || d.capacity.Load() != 32*sectorSize {
		t.Errorf("an update that may land: %v, capacity %d", err, d.capacity.Load())
	}
}

// Shrinking: the capacity drops, the requests working past it finish,
// the guest hears, then the file is cut; an error leaves the file whole.
func TestBlkResizeShrink(t *testing.T) {
	const mib = 1 << 20
	d := testDisk(t, 8*mib)
	fill(d, t, 0x5a)
	// A read of 2 MiB at 5 MiB, caught between its chunks.
	held := in(5*mib/sectorSize, 2*mib)
	held.holdWrite = make(chan struct{})
	served := make(chan struct{})
	go func() { d.serve(held); close(served) }()
	waitHeld(t, d)
	var told atomic.Bool
	resized := make(chan error, 1)
	go func() {
		resized <- d.resize(4*mib, func(c uint64) error {
			select {
			case <-served:
			default:
				t.Error("the guest was told before a request in flight finished")
			}
			if st, _ := d.f.Stat(); d.capacity.Load() != c || st.Size() != 8*mib {
				t.Errorf("told %d with the capacity at %d, the file at %d", c, d.capacity.Load(), st.Size())
			}
			told.Store(true)
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	if told.Load() {
		t.Fatal("told before the request finished")
	}
	// A new request does not wait for the shrink, and is refused.
	if r := out(6*mib/sectorSize, make([]byte, sectorSize)); timedServe(d, r) > 20*time.Millisecond || status(r) != blkStatusIOErr {
		t.Errorf("a request past the new end during the shrink: %d", status(r))
	}
	close(held.holdWrite)
	if err := <-resized; err != nil {
		t.Fatal(err)
	}
	<-served
	if status(held) != blkStatusOK || !bytes.Equal(held.wrote[:2*mib], bytes.Repeat([]byte{0x5a}, 2*mib)) {
		t.Errorf("the request in flight: %d", status(held))
	}
	if st, _ := d.f.Stat(); st.Size() != 4*mib {
		t.Errorf("the file is %d bytes", st.Size())
	}
	d.f.Truncate(8 * sectorSize)
	d.capacity.Store(8 * sectorSize)

	// A guest that surely did not hear keeps the disk as it was.
	if err := d.resize(4*sectorSize, func(uint64) error { return errors.New("refused") }); err == nil {
		t.Error("a refused shrink succeeded")
	}
	if st, _ := d.f.Stat(); st.Size() != 8*sectorSize || d.capacity.Load() != 8*sectorSize {
		t.Errorf("after a refused shrink: file %d, capacity %d", st.Size(), d.capacity.Load())
	}
	// One that may have heard has the new size, the file cut too: it gave
	// the end up before asking.
	if err := d.resize(4*sectorSize, func(uint64) error { return ErrNotTold }); !errors.Is(err, ErrNotTold) {
		t.Errorf("a shrink the guest may not know of: %v", err)
	}
	if st, _ := d.f.Stat(); st.Size() != 4*sectorSize || d.capacity.Load() != 4*sectorSize {
		t.Errorf("after a shrink the guest may not know of: file %d, capacity %d", st.Size(), d.capacity.Load())
	}
}

// A request that never ends bounds the shrink's wait: the guest is not
// told and the file stays.
func TestBlkResizeShrinkStuck(t *testing.T) {
	old := drainWait
	drainWait = 30 * time.Millisecond
	t.Cleanup(func() { drainWait = old })
	d := testDisk(t, 16*sectorSize)
	held := out(12, make([]byte, sectorSize))
	held.hold = make(chan struct{})
	defer close(held.hold)
	go d.serve(held)
	waitHeld(t, d)
	err := d.resize(8*sectorSize, func(uint64) error { t.Error("told"); return nil })
	if st, _ := d.f.Stat(); err == nil || st.Size() != 16*sectorSize {
		t.Errorf("%v, file %d", err, st.Size())
	}
}

// Under writes everywhere, shrinks and grows never let one land past the
// end the device has.
func TestBlkResizeUnderLoad(t *testing.T) {
	const most = 256
	d := testDisk(t, most*sectorSize)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 1))
			buf := bytes.Repeat([]byte{byte(w)}, sectorSize)
			for {
				select {
				case <-stop:
					return
				default:
				}
				d.serve(out(rng.Uint64N(most), buf))
			}
		}()
	}
	sizes := []uint64{64, 200, 8, 128, 1, 256, 32}
	for i := range 60 {
		size := sizes[i%len(sizes)] * sectorSize
		if err := d.resize(size, func(uint64) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if st, _ := d.f.Stat(); uint64(st.Size()) != size {
			t.Fatalf("after resizing to %d the file is %d", size, st.Size())
		}
	}
	if err := d.resize(16*sectorSize, func(uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()
	if st, _ := d.f.Stat(); st.Size() != 16*sectorSize {
		t.Errorf("a write landed past the end: the file is %d", st.Size())
	}
}

// waitHeld waits until a request holds d.
func waitHeld(t *testing.T, d *blkDisk) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); d.holds.n[0].Load()+d.holds.n[1].Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no request holds the disk")
		}
	}
}

// timedServe serves r and says how long it took.
func timedServe(d *blkDisk, r *fakeReq) time.Duration {
	t0 := time.Now()
	d.serve(r)
	return time.Since(t0)
}

// A shrink that gives up on a stuck request stores nothing later: a grow
// after it keeps its size once the request ends, and no request waits
// for the shrink, neither while it waits nor after.
func TestBlkResizeShrinkStuckThenGrow(t *testing.T) {
	old := drainWait
	drainWait = 50 * time.Millisecond
	t.Cleanup(func() { drainWait = old })
	d := testDisk(t, 16*sectorSize)
	held := out(2, make([]byte, sectorSize))
	held.hold = make(chan struct{})
	served := make(chan struct{})
	go func() { d.serve(held); close(served) }()
	waitHeld(t, d)

	shrunk := make(chan error, 1)
	go func() { shrunk <- d.resize(8*sectorSize, func(uint64) error { t.Error("told"); return nil }) }()
	time.Sleep(10 * time.Millisecond)
	// While the shrink waits, a request goes through at once.
	if took := timedServe(d, in(1, sectorSize)); took > 20*time.Millisecond {
		t.Errorf("a request waited %v for a shrink", took)
	}
	if err := <-shrunk; err == nil {
		t.Fatal("a shrink with a request stuck succeeded")
	}
	if c := d.capacity.Load(); c != 16*sectorSize {
		t.Errorf("after a shrink that gave up the capacity is %d", c)
	}
	if err := d.resize(32*sectorSize, func(uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if took := timedServe(d, in(20, sectorSize)); took > 20*time.Millisecond {
		t.Errorf("a request waited %v after the shrink gave up", took)
	}
	close(held.hold)
	<-served
	time.Sleep(20 * time.Millisecond)
	if c := d.capacity.Load(); c != 32*sectorSize {
		t.Errorf("the grow was undone: capacity %d", c)
	}
	if st, _ := d.f.Stat(); st.Size() != 32*sectorSize {
		t.Errorf("the file is %d bytes", st.Size())
	}
}

// A request left over by a shrink that gave up holds up the next shrink
// too: it saw the capacity from before both.
func TestBlkResizeShrinkStraggler(t *testing.T) {
	old := drainWait
	drainWait = 30 * time.Millisecond
	t.Cleanup(func() { drainWait = old })
	d := testDisk(t, 16*sectorSize)
	held := out(12, make([]byte, sectorSize))
	held.hold = make(chan struct{})
	served := make(chan struct{})
	go func() { d.serve(held); close(served) }()
	waitHeld(t, d)
	tell := func(uint64) error { return nil }
	if err := d.resize(8*sectorSize, tell); err == nil {
		t.Fatal("the first shrink succeeded")
	}
	if err := d.resize(8*sectorSize, tell); err == nil {
		t.Fatal("the second shrink succeeded with a request of before both still out")
	}
	close(held.hold)
	<-served
	if err := d.resize(8*sectorSize, tell); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.f.Stat(); st.Size() != 8*sectorSize {
		t.Errorf("the file is %d bytes", st.Size())
	}
}

// A grow the guest surely did not hear of leaves the file as it was; one
// it may have heard of keeps it grown.
func TestBlkResizeGrowRestoresFile(t *testing.T) {
	d := testDisk(t, 8*sectorSize)
	if err := d.resize(32*sectorSize, func(uint64) error { return errors.New("refused") }); err == nil {
		t.Fatal("a refused grow succeeded")
	}
	if st, _ := d.f.Stat(); st.Size() != 8*sectorSize || d.capacity.Load() != 8*sectorSize {
		t.Errorf("after a refused grow: file %d, capacity %d", st.Size(), d.capacity.Load())
	}
	// A file larger than the capacity already stays as large.
	if err := d.f.Truncate(64 * sectorSize); err != nil {
		t.Fatal(err)
	}
	if err := d.resize(32*sectorSize, func(uint64) error { return errors.New("refused") }); err == nil {
		t.Fatal("a refused grow succeeded")
	}
	if st, _ := d.f.Stat(); st.Size() != 64*sectorSize {
		t.Errorf("a refused grow cut a file larger than both sizes to %d", st.Size())
	}
	if err := d.resize(32*sectorSize, func(uint64) error { return ErrNotTold }); !errors.Is(err, ErrNotTold) {
		t.Fatal(err)
	}
	if d.capacity.Load() != 32*sectorSize {
		t.Errorf("a grow that may have landed: capacity %d", d.capacity.Load())
	}
}

// A grow the guest refused waits, before it cuts the file back, for the
// requests that saw the larger capacity.
func TestBlkResizeGrowRestoreWaits(t *testing.T) {
	const mib = 1 << 20
	d := testDisk(t, 4*mib)
	var held *fakeReq
	served := make(chan struct{})
	err := d.resize(8*mib, func(uint64) error {
		held = in(5*mib/sectorSize, 2*mib)
		held.holdWrite = make(chan struct{})
		go func() { d.serve(held); close(served) }()
		waitHeld(t, d)
		go func() { time.Sleep(30 * time.Millisecond); close(held.holdWrite) }()
		return errors.New("refused")
	})
	if err == nil {
		t.Fatal("a refused grow succeeded")
	}
	select {
	case <-served:
	default:
		t.Error("the grow was undone while a request that saw it was still served")
	}
	if status(held) != blkStatusOK {
		t.Errorf("the request that saw the grow: %d", status(held))
	}
	if st, _ := d.f.Stat(); st.Size() != 4*mib {
		t.Errorf("the file is %d bytes after a refused grow", st.Size())
	}
}

// A refused grow gives the capacity back before it drains: a request that
// comes once the drain is done, before the file is cut back, is refused
// past the old end, as the drain did not wait for it.
func TestBlkResizeGrowRestoreBeforeDrain(t *testing.T) {
	const mib = 1 << 20
	d := testDisk(t, 4*mib)
	var late *fakeReq
	undoDrained = func() {
		late = in(5*mib/sectorSize, sectorSize)
		d.serve(late)
	}
	t.Cleanup(func() { undoDrained = nil })
	if err := d.resize(8*mib, func(uint64) error { return errors.New("refused") }); err == nil {
		t.Fatal("a refused grow succeeded")
	}
	if late == nil {
		t.Fatal("the grow was not undone")
	}
	if status(late) != blkStatusIOErr {
		t.Errorf("a request past the old end, after the drain: %d", status(late))
	}
	if st, _ := d.f.Stat(); st.Size() != 4*mib {
		t.Errorf("the file is %d bytes after a refused grow", st.Size())
	}
}

// New requests never wait for a drain, and a drain waits for each one
// that held the disk before it.
func TestHolds(t *testing.T) {
	var h holds
	a := h.enter()
	drained := make(chan bool, 1)
	go func() { drained <- h.drain(time.Now().Add(5 * time.Second)) }()
	for h.gen.Load() == a {
		time.Sleep(time.Millisecond)
	}
	for range 100 {
		h.leave(h.enter())
	}
	select {
	case <-drained:
		t.Fatal("the drain did not wait")
	case <-time.After(10 * time.Millisecond):
	}
	h.leave(a)
	if !<-drained {
		t.Error("the drain gave up")
	}
	if h.drain(time.Now()) != true {
		t.Error("a drain with nothing held gave up")
	}
}

// An image is the device's alone: a second open of it is refused while
// the first holds it, and the size asked for grows a smaller file.
func TestOpenBlkDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, make([]byte, 8*sectorSize+100), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := openBlkDisk(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.capacity.Load() != 8*sectorSize {
		t.Errorf("capacity %d", d.capacity.Load())
	}
	want, err := fsBlock(d.f)
	if err != nil || d.hole != want {
		t.Errorf("holes of %d, the file system's block %d (%v)", d.hole, want, err)
	}
	if _, err := openBlkDisk(path, 0); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("a second open: %v", err)
	}
	d.f.Close()
	d, err = openBlkDisk(path, 32*sectorSize+1)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := d.f.Stat(); st.Size() != 32*sectorSize || d.capacity.Load() != 32*sectorSize {
		t.Errorf("sized at boot: file %d, capacity %d", st.Size(), d.capacity.Load())
	}
	d.f.Close()
	d, err = openBlkDisk(path, 16*sectorSize)
	if err != nil {
		t.Fatal(err)
	}
	defer d.f.Close()
	if st, _ := d.f.Stat(); st.Size() != 32*sectorSize || d.capacity.Load() != 16*sectorSize {
		t.Errorf("a smaller size: file %d, capacity %d", st.Size(), d.capacity.Load())
	}
	if _, err := openBlkDisk(filepath.Join(t.TempDir(), "none.img"), 0); err == nil {
		t.Error("a missing image opened")
	}
}

// Holes are punched in the file system's blocks, whatever their size: a
// discard smaller than one punches nothing.
func TestBlkDiscardBlock(t *testing.T) {
	const size = 64 << 10
	d := testDisk(t, size)
	d.hole = 16384
	fill(d, t, 0xaa)
	// 8 KiB at 4 KiB: whole 4 KiB blocks, but no whole 16 KiB one.
	r := ranges(blkTypeDiscard, seg(8, 16, 0))
	d.serve(r)
	if status(r) != blkStatusOK || !bytes.Equal(fileBytes(t, d, 0, size), bytes.Repeat([]byte{0xaa}, size)) {
		t.Errorf("a discard under one block: %d, data changed", status(r))
	}
	// 16 KiB ... 48 KiB: two whole blocks.
	r = ranges(blkTypeDiscard, seg(24, 80, 0))
	d.serve(r)
	got := fileBytes(t, d, 0, size)
	if !bytes.Equal(got[16384:49152], make([]byte, 32768)) || !bytes.Equal(got[:16384], bytes.Repeat([]byte{0xaa}, 16384)) || !bytes.Equal(got[49152:], bytes.Repeat([]byte{0xaa}, size-49152)) {
		t.Error("the discard did not give back just its whole blocks")
	}
}

// failTruncate has resize's truncates to size fail, for the test.
func failTruncate(t *testing.T, size int64) {
	t.Helper()
	truncate = func(f *os.File, n int64) error {
		if n == size {
			return errors.New("no space to truncate")
		}
		return f.Truncate(n)
	}
	t.Cleanup(func() { truncate = (*os.File).Truncate })
}

// resizeOutcome is what a resize's error says, and what the disk is.
type resizeOutcome struct {
	applied  bool
	file     *FileError // nil when there is none
	capacity uint64
	size     int64 // the file's
}

func outcome(t *testing.T, d *blkDisk, err error) resizeOutcome {
	t.Helper()
	o := resizeOutcome{applied: ResizeApplied(err), capacity: d.capacity.Load()}
	var fe *FileError
	if errors.As(err, &fe) {
		o.file = fe
	}
	st, serr := d.f.Stat()
	if serr != nil {
		t.Fatal(serr)
	}
	o.size = st.Size()
	return o
}

// Each way a resize ends says whether the disk has the new size, and
// whether its file is the disk's size, and the disk is what it says.
func TestBlkResizeOutcomes(t *testing.T) {
	const s = sectorSize
	refused := errors.New("refused")
	t.Run("ok", func(t *testing.T) {
		d := testDisk(t, 8*s)
		o := outcome(t, d, d.resize(16*s, func(uint64) error { return nil }))
		if !o.applied || o.file != nil || o.capacity != 16*s || o.size != 16*s {
			t.Errorf("%+v", o)
		}
	})
	t.Run("not told", func(t *testing.T) {
		d := testDisk(t, 8*s)
		err := d.resize(16*s, func(uint64) error { return ErrNotTold })
		o := outcome(t, d, err)
		if !errors.Is(err, ErrNotTold) || !o.applied || o.file != nil || o.capacity != 16*s || o.size != 16*s {
			t.Errorf("%v: %+v", err, o)
		}
	})
	t.Run("refused", func(t *testing.T) {
		d := testDisk(t, 8*s)
		err := d.resize(16*s, func(uint64) error { return refused })
		o := outcome(t, d, err)
		if !errors.Is(err, refused) || o.applied || o.file != nil || o.capacity != 8*s || o.size != 8*s {
			t.Errorf("%v: %+v", err, o)
		}
	})
	t.Run("shrunk, the file not cut", func(t *testing.T) {
		d := testDisk(t, 8*s)
		failTruncate(t, 4*s)
		err := d.resize(4*s, func(uint64) error { return nil })
		o := outcome(t, d, err)
		if !o.applied || o.file == nil || !o.file.Applied || o.file.Capacity != 4*s || o.file.Size != 8*s || o.capacity != 4*s || o.size != 8*s ||
			!strings.Contains(err.Error(), "the disk is 2048 bytes, but its file stays at 4096 bytes: no space to truncate") {
			t.Errorf("%v: %+v %+v", err, o, o.file)
		}
	})
	t.Run("shrunk, not told, the file not cut", func(t *testing.T) {
		d := testDisk(t, 8*s)
		failTruncate(t, 4*s)
		err := d.resize(4*s, func(uint64) error { return ErrNotTold })
		o := outcome(t, d, err)
		if !errors.Is(err, ErrNotTold) || !o.applied || o.file == nil || !o.file.Applied || o.capacity != 4*s || o.size != 8*s {
			t.Errorf("%v: %+v", err, o)
		}
	})
	t.Run("a refused grow, the file not cut back", func(t *testing.T) {
		d := testDisk(t, 8*s)
		failTruncate(t, 8*s)
		err := d.resize(16*s, func(uint64) error { return refused })
		o := outcome(t, d, err)
		if !errors.Is(err, refused) || o.applied || o.file == nil || o.file.Applied || o.file.Capacity != 8*s || o.file.Size != 16*s ||
			o.capacity != 8*s || o.size != 16*s || !strings.Contains(err.Error(), "the disk stays at 4096 bytes, but its file stays at 8192 bytes: no space to truncate") {
			t.Errorf("%v: %+v %+v", err, o, o.file)
		}
	})
	t.Run("a refused grow, a request stuck", func(t *testing.T) {
		old := drainWait
		drainWait = 30 * time.Millisecond
		t.Cleanup(func() { drainWait = old })
		d := testDisk(t, 8*s)
		held := out(12, make([]byte, s))
		held.hold = make(chan struct{})
		defer close(held.hold)
		err := d.resize(16*s, func(uint64) error {
			go d.serve(held)
			waitHeld(t, d)
			return refused
		})
		o := outcome(t, d, err)
		if !errors.Is(err, refused) || o.applied || o.file == nil || o.file.Applied || o.capacity != 8*s || o.size != 16*s ||
			!strings.Contains(err.Error(), "did not finish") {
			t.Errorf("%v: %+v", err, o)
		}
	})
	t.Run("less than a sector", func(t *testing.T) {
		d := testDisk(t, 8*s)
		for _, n := range []uint64{0, 1, s - 1} {
			err := d.resize(n, func(uint64) error { t.Error("told"); return nil })
			if o := outcome(t, d, err); err == nil || o.applied || o.capacity != 8*s || o.size != 8*s {
				t.Errorf("%d bytes: %v, %+v", n, err, o)
			}
		}
		if err := d.resize(s, func(uint64) error { return nil }); err != nil || d.capacity.Load() != s {
			t.Errorf("one sector: %v, capacity %d", err, d.capacity.Load())
		}
	})
}
