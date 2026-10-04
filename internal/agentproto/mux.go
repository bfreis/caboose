// Package agentproto is what the host's link helper and caboose-agent, in
// the container, speak to each other: many streams over one byte stream,
// which is the stdin and stdout of a `docker exec -i`.
//
// Stream 0 carries control messages (Message, one per frame, JSON). Other
// streams are opened by the host alone -- one per TCP connection it
// forwards -- and carry raw bytes. Each stream has its own flow control, so
// a stalled connection never holds up the others or the control messages.
//
// The host reads what the container writes, and the container is not
// trusted: every frame is checked against the limits below, and a peer
// that breaks them is cut off rather than argued with.
package agentproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Frame types.
const (
	frameControl byte = iota // stream 0: one Message
	frameOpen                // a new stream; payload is its header
	frameData                // bytes for a stream
	frameWindow              // payload is a uint32: more bytes the sender may send
	frameClose               // the sender will write no more on this stream
	frameReset               // the stream is gone, both ways
)

const (
	headerLen = 9 // type, stream (uint32), length (uint32)
	// MaxPayload is the most a frame carries; a control message must fit.
	MaxPayload = 64 << 10
	// window is how much a stream may have in flight, unread, per direction.
	window = 256 << 10
	// MaxStreams is how many streams may be open at once.
	MaxStreams = 256
	// acceptBacklog is how many opened streams may wait for Accept.
	acceptBacklog = 16
	// controlBacklog is how many control messages may wait for Control.
	controlBacklog = 64
)

// ErrClosed is returned once the session is closed.
var ErrClosed = errors.New("agentproto: session closed")

// errReset is returned by a stream the peer reset.
var errReset = errors.New("agentproto: stream reset")

// Session is one end of the link.
type Session struct {
	r io.Reader
	w io.WriteCloser

	wmu sync.Mutex // one frame at a time on w

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	err     error

	accept  chan *Stream
	control chan []byte
	done    chan struct{}
	once    sync.Once
}

// NewSession starts a session over r and w. The host passes opener true,
// and is the only side that may open streams.
func NewSession(r io.Reader, w io.WriteCloser, opener bool) *Session {
	s := &Session{
		r:       r,
		w:       w,
		streams: map[uint32]*Stream{},
		accept:  make(chan *Stream, acceptBacklog),
		control: make(chan []byte, controlBacklog),
		done:    make(chan struct{}),
	}
	if opener {
		s.nextID = 1
	}
	go s.readLoop(opener)
	return s
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is why the session ended, once it has.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the session and every stream in it.
func (s *Session) Close() error {
	s.fail(ErrClosed)
	return nil
}

func (s *Session) fail(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		streams := s.streams
		s.streams = map[uint32]*Stream{}
		s.mu.Unlock()
		_ = s.w.Close()
		close(s.done)
		for _, st := range streams {
			st.kill(err)
		}
	})
}

// SendControl sends a control message, already encoded.
func (s *Session) SendControl(msg []byte) error {
	if len(msg) > MaxPayload {
		return fmt.Errorf("agentproto: control message of %d bytes is over %d", len(msg), MaxPayload)
	}
	return s.writeFrame(frameControl, 0, msg)
}

// Control delivers the peer's control messages, in order. It is closed
// when the session ends.
func (s *Session) Control() <-chan []byte { return s.control }

// Open opens a stream with header, which the peer's Accept returns.
func (s *Session) Open(header []byte) (*Stream, error) {
	if len(header) > MaxPayload {
		return nil, fmt.Errorf("agentproto: stream header of %d bytes is over %d", len(header), MaxPayload)
	}
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	if s.nextID == 0 {
		s.mu.Unlock()
		return nil, errors.New("agentproto: this side does not open streams")
	}
	if len(s.streams) >= MaxStreams {
		s.mu.Unlock()
		return nil, fmt.Errorf("agentproto: %d streams already open", MaxStreams)
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id, header)
	s.streams[id] = st
	s.mu.Unlock()
	if err := s.writeFrame(frameOpen, id, header); err != nil {
		s.forget(id)
		return nil, err
	}
	return st, nil
}

// Accept returns the next stream the peer opened.
func (s *Session) Accept() (*Stream, error) {
	select {
	case st := <-s.accept:
		return st, nil
	case <-s.done:
		return nil, s.Err()
	}
}

func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	var h [headerLen]byte
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:5], id)
	binary.BigEndian.PutUint32(h[5:9], uint32(len(payload)))
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return s.Err()
	default:
	}
	if _, err := s.w.Write(h[:]); err != nil {
		s.fail(err)
		return err
	}
	if len(payload) > 0 {
		if _, err := s.w.Write(payload); err != nil {
			s.fail(err)
			return err
		}
	}
	return nil
}

func (s *Session) readLoop(opener bool) {
	defer close(s.control)
	var h [headerLen]byte
	buf := make([]byte, MaxPayload)
	for {
		if _, err := io.ReadFull(s.r, h[:]); err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			}
			s.fail(err)
			return
		}
		typ, id, n := h[0], binary.BigEndian.Uint32(h[1:5]), binary.BigEndian.Uint32(h[5:9])
		if n > MaxPayload {
			s.fail(fmt.Errorf("agentproto: peer sent a frame of %d bytes", n))
			return
		}
		payload := buf[:n]
		if _, err := io.ReadFull(s.r, payload); err != nil {
			s.fail(err)
			return
		}
		if err := s.handle(typ, id, payload, opener); err != nil {
			s.fail(err)
			return
		}
	}
}

// handle acts on one frame. An error is a protocol violation, which ends
// the session.
func (s *Session) handle(typ byte, id uint32, payload []byte, opener bool) error {
	if typ == frameControl {
		if id != 0 {
			return fmt.Errorf("agentproto: control frame on stream %d", id)
		}
		msg := append([]byte(nil), payload...)
		select {
		case s.control <- msg:
			return nil
		case <-s.done:
			return s.Err()
		}
	}
	if id == 0 {
		return fmt.Errorf("agentproto: frame type %d on stream 0", typ)
	}
	if typ > frameReset {
		return fmt.Errorf("agentproto: unknown frame type %d", typ)
	}
	if typ == frameOpen {
		if opener {
			return errors.New("agentproto: peer opened a stream, which only this side may do")
		}
		return s.accepted(id, payload)
	}
	s.mu.Lock()
	st := s.streams[id]
	s.mu.Unlock()
	if st == nil {
		// A stream this side already dropped: frames may still be in flight.
		return nil
	}
	switch typ {
	case frameData:
		return st.received(payload)
	case frameWindow:
		if len(payload) != 4 {
			return errors.New("agentproto: bad window frame")
		}
		return st.granted(binary.BigEndian.Uint32(payload))
	case frameClose:
		st.remoteClosed()
		return nil
	case frameReset:
		s.forget(id)
		st.kill(errReset)
	}
	return nil
}

func (s *Session) accepted(id uint32, header []byte) error {
	s.mu.Lock()
	if _, dup := s.streams[id]; dup {
		s.mu.Unlock()
		return fmt.Errorf("agentproto: stream %d opened twice", id)
	}
	if len(s.streams) >= MaxStreams {
		s.mu.Unlock()
		return s.writeFrame(frameReset, id, nil)
	}
	st := newStream(s, id, append([]byte(nil), header...))
	s.streams[id] = st
	s.mu.Unlock()
	select {
	case s.accept <- st:
		return nil
	default:
		s.forget(id)
		return s.writeFrame(frameReset, id, nil)
	}
}

func (s *Session) forget(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

// Stream is one stream of a session: an io.ReadWriteCloser with a half
// close.
type Stream struct {
	s      *Session
	id     uint32
	header []byte

	mu          sync.Mutex
	cond        *sync.Cond
	buf         []byte // received, not yet read
	unacked     int    // read since the last window granted
	credit      int    // bytes this side may still send
	remoteEOF   bool   // the peer will send no more
	localClosed bool   // this side will send no more
	err         error  // reset, session gone, or Close
}

func newStream(s *Session, id uint32, header []byte) *Stream {
	st := &Stream{s: s, id: id, header: header, credit: window}
	st.cond = sync.NewCond(&st.mu)
	return st
}

// Header is what the opener sent with the stream.
func (st *Stream) Header() []byte { return st.header }

func (st *Stream) received(p []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.remoteEOF {
		return fmt.Errorf("agentproto: data on stream %d after its close", st.id)
	}
	// Unread plus read-but-not-yet-granted is what the peer has in flight.
	if len(st.buf)+st.unacked+len(p) > window {
		return fmt.Errorf("agentproto: stream %d overran its window", st.id)
	}
	if st.err == nil {
		st.buf = append(st.buf, p...)
	}
	st.cond.Broadcast()
	return nil
}

func (st *Stream) granted(n uint32) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if n > window || st.credit+int(n) > window {
		return fmt.Errorf("agentproto: stream %d granted more than its window", st.id)
	}
	st.credit += int(n)
	st.cond.Broadcast()
	return nil
}

func (st *Stream) remoteClosed() {
	st.mu.Lock()
	st.remoteEOF = true
	done := st.localClosed
	st.cond.Broadcast()
	st.mu.Unlock()
	if done {
		st.s.forget(st.id)
	}
}

func (st *Stream) kill(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.cond.Broadcast()
	st.mu.Unlock()
}

// Read reads what the peer sent, and io.EOF once it closed its side.
func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	for len(st.buf) == 0 && !st.remoteEOF && st.err == nil {
		st.cond.Wait()
	}
	if len(st.buf) == 0 {
		err := st.err
		st.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	n := copy(p, st.buf)
	st.buf = st.buf[n:]
	st.unacked += n
	var grant int
	if st.unacked >= window/2 && !st.remoteEOF {
		grant, st.unacked = st.unacked, 0
	}
	st.mu.Unlock()
	if grant > 0 {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(grant))
		_ = st.s.writeFrame(frameWindow, st.id, b[:])
	}
	return n, nil
}

// Write sends p, waiting for the peer to read when the window is full.
func (st *Stream) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		st.mu.Lock()
		for st.credit == 0 && st.err == nil && !st.localClosed {
			st.cond.Wait()
		}
		if st.err != nil || st.localClosed {
			err := st.err
			st.mu.Unlock()
			if err == nil {
				err = io.ErrClosedPipe
			}
			return written, err
		}
		n := min(len(p)-written, st.credit, MaxPayload)
		st.credit -= n
		st.mu.Unlock()
		if err := st.s.writeFrame(frameData, st.id, p[written:written+n]); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

// CloseWrite tells the peer this side will send no more; reading goes on.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.localClosed || st.err != nil {
		st.mu.Unlock()
		return nil
	}
	st.localClosed = true
	done := st.remoteEOF
	st.cond.Broadcast()
	st.mu.Unlock()
	err := st.s.writeFrame(frameClose, st.id, nil)
	if done {
		st.s.forget(st.id)
	}
	return err
}

// Close ends the stream both ways. When the peer has not finished sending,
// it is told to stop (a reset).
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.err != nil {
		st.mu.Unlock()
		return nil
	}
	clean := st.localClosed && st.remoteEOF
	st.err = io.ErrClosedPipe
	st.buf = nil
	st.cond.Broadcast()
	st.mu.Unlock()
	st.s.forget(st.id)
	if clean {
		return nil
	}
	return st.s.writeFrame(frameReset, st.id, nil)
}

// HalfCloser is a connection whose write side closes alone: a Stream, or a
// *net.TCPConn.
type HalfCloser interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// Splice copies both ways between a and b until both are done, passing
// each side's end of writing on as a half close, then closes both. An error
// either way (a reset, a broken connection) closes both at once.
func Splice(a, b HalfCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	pass := func(dst, src HalfCloser) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil {
			a.Close()
			b.Close()
			return
		}
		_ = dst.CloseWrite()
	}
	go pass(a, b)
	go pass(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}
