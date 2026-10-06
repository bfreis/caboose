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
	"sync/atomic"
	"time"

	"github.com/bfreis/caboose/internal/linkdebug"
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
	// DefaultWindow is how much a stream may have in flight, unread, to a
	// side that announced no window of its own in its hello: an agent or
	// a host from before the window was announced.
	DefaultWindow = 256 << 10
	// HostWindow and AgentWindow are what each side's streams receive in
	// flight, unread, and announce in their hello (Message.Window). The
	// host's is the smaller: it holds what the container sends, at most
	// MaxStreams of them. A link to a guest is local, but its round trip
	// through a hypervisor's vsock is not a socketpair's, and a stream
	// moves at most a window per round trip.
	HostWindow  = 1 << 20
	AgentWindow = 4 << 20
	// MaxWindow is the most a peer's announced window is taken as.
	MaxWindow = AgentWindow
	// grantAt is how much a stream reads before granting it back. It is
	// fixed, not half the window, since the peer may be using less than
	// this side's window: DefaultWindow, until it has read this side's
	// hello, or for good when it is older.
	grantAt = DefaultWindow / 2
	// MaxStreams is how many streams may be open at once.
	MaxStreams = 256
	// acceptBacklog is how many opened streams may wait for Accept: as
	// many as may be open, so a stream within MaxStreams is never refused
	// for want of room, however many arrive at once.
	acceptBacklog = MaxStreams
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

	wmu  sync.Mutex // one frame at a time on w
	wbuf []byte     // a frame, written whole: header and payload at once
	// split writes a frame's header and payload apart (linkdebug).
	split bool

	// writing is when the write in progress started, 0 when none is:
	// for WatchStalls, as is lastRead, when the last frame arrived.
	writing  atomic.Int64
	lastRead atomic.Int64

	// recvWindow is what this side's streams take in flight, which its
	// hello announces. sawHello is the readLoop's alone.
	recvWindow int
	sawHello   bool
	// maxStreams is how many streams may be open at once (Limits).
	maxStreams int

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	err     error
	// peerWindow is what the peer's hello announced: what a stream of
	// this side's sends ahead.
	peerWindow int

	accept chan *Stream
	// refused hears of each stream the peer opened that this side reset
	// before Accept returned it (OnRefused).
	refused func(header []byte)
	control chan []byte
	done    chan struct{}
	once    sync.Once
}

// NewSession starts a session over r and w. The host passes opener true,
// and is the only side that may open streams.
func NewSession(r io.Reader, w io.WriteCloser, opener bool) *Session {
	return NewSessionLimited(r, w, opener, Limits{})
}

// Limits bound what the peer can have this side hold: a side that serves
// an untrusted opener (the host's end of a host exec) takes fewer streams,
// and less in flight on each, than a link's own. Zero is the default.
type Limits struct {
	// Window is what this side's streams receive in flight, within
	// DefaultWindow and MaxWindow.
	Window int
	// Streams is the most streams the peer may have open at once; one
	// more is reset unaccepted. At most MaxStreams.
	Streams int
}

// NewSessionLimited is NewSession with lim, set before the first frame is
// read.
func NewSessionLimited(r io.Reader, w io.WriteCloser, opener bool, lim Limits) *Session {
	s := &Session{
		r:          r,
		w:          w,
		wbuf:       make([]byte, headerLen+MaxPayload),
		recvWindow: AgentWindow,
		peerWindow: DefaultWindow,
		streams:    map[uint32]*Stream{},
		accept:     make(chan *Stream, acceptBacklog),
		control:    make(chan []byte, controlBacklog),
		done:       make(chan struct{}),
	}
	if opener {
		s.nextID = 1
		s.recvWindow = HostWindow
	}
	if k := linkdebug.Get(); k.Window > 0 {
		s.recvWindow = min(max(k.Window, DefaultWindow), MaxWindow)
	}
	if lim.Window > 0 {
		s.recvWindow = min(max(lim.Window, DefaultWindow), MaxWindow)
	}
	s.maxStreams = MaxStreams
	if lim.Streams > 0 {
		s.maxStreams = min(lim.Streams, MaxStreams)
	}
	s.split = linkdebug.Get().SplitFrames
	s.lastRead.Store(time.Now().UnixNano())
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
	st := newStream(s, id, header, s.peerWindow)
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

// OnRefused has f called with the header of each stream the peer opens
// that this side resets unaccepted: one over MaxStreams, or one that finds
// no room to wait for Accept. A side waiting for such a stream learns at
// once that it will not come. f runs on the session's reader, so it must
// not block, nor call into the session.
func (s *Session) OnRefused(f func(header []byte)) {
	s.mu.Lock()
	s.refused = f
	s.mu.Unlock()
}

// Window is what this side's streams receive in flight, for its hello.
func (s *Session) Window() uint32 { return uint32(s.recvWindow) }

// PeerWindow is what this side's streams send ahead: the peer's hello's
// window, or DefaultWindow until it arrived (or when it named none).
func (s *Session) PeerWindow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peerWindow
}

// peerHello takes the window the peer's hello announces, within
// DefaultWindow, which every peer takes, and MaxWindow: 0, from a peer
// that announces none, is DefaultWindow. Streams already open grow to it.
func (s *Session) peerHello(window uint32) {
	w := int(max(min(window, MaxWindow), DefaultWindow))
	s.mu.Lock()
	s.peerWindow = w
	streams := make([]*Stream, 0, len(s.streams))
	for _, st := range s.streams {
		streams = append(streams, st)
	}
	s.mu.Unlock()
	for _, st := range streams {
		st.resize(w)
	}
}

// writeFrame writes a frame in a single write: one packet on a vsock,
// where a header alone would be one of its own.
func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return s.Err()
	default:
	}
	f := s.wbuf[:headerLen+len(payload)]
	f[0] = typ
	binary.BigEndian.PutUint32(f[1:5], id)
	binary.BigEndian.PutUint32(f[5:9], uint32(len(payload)))
	copy(f[headerLen:], payload)
	s.writing.Store(time.Now().UnixNano())
	defer s.writing.Store(0)
	if s.split && len(payload) > 0 {
		if _, err := s.w.Write(f[:headerLen]); err != nil {
			s.fail(err)
			return err
		}
		f = f[headerLen:]
	}
	if _, err := s.w.Write(f); err != nil {
		s.fail(err)
		return err
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
		s.lastRead.Store(time.Now().UnixNano())
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
		if !s.sawHello {
			// The peer's first message is its hello, before any stream:
			// its window counts from the first frame after it.
			s.sawHello = true
			if m, err := Decode(msg); err == nil && m.Type == TypeHello {
				s.peerHello(m.Window)
			}
		}
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
	refused := s.refused
	if len(s.streams) >= s.maxStreams {
		s.mu.Unlock()
		return s.refuse(id, header, refused)
	}
	st := newStream(s, id, append([]byte(nil), header...), s.peerWindow)
	s.streams[id] = st
	s.mu.Unlock()
	select {
	case s.accept <- st:
		return nil
	default:
		// Only when streams reset while waiting still fill the backlog.
		s.forget(id)
		return s.refuse(id, header, refused)
	}
}

// refuse resets the peer's stream id, unaccepted, and says so to refused.
func (s *Session) refuse(id uint32, header []byte, refused func([]byte)) error {
	if refused != nil {
		refused(header)
	}
	return s.writeFrame(frameReset, id, nil)
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
	buf         []byte // received; buf[off:] is not yet read
	off         int
	unacked     int   // read since the last window granted
	credit      int   // bytes this side may still send
	sendWindow  int   // the most credit may be: the peer's window
	remoteEOF   bool  // the peer will send no more
	localClosed bool  // this side will send no more
	err         error // reset, session gone, or Close

	// For WatchStalls: since when a Write has waited for credit, whether
	// that wait was logged, and what moved.
	stalledAt          time.Time
	stallLogged        bool
	sent, recvd        int64
	grantsIn, grantsUp int
}

func newStream(s *Session, id uint32, header []byte, peerWindow int) *Stream {
	st := &Stream{s: s, id: id, header: header, credit: peerWindow, sendWindow: peerWindow}
	st.cond = sync.NewCond(&st.mu)
	return st
}

// resize grows the stream's send window to w, the peer's, keeping what
// it has in flight.
func (st *Stream) resize(w int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if w > st.sendWindow {
		st.credit += w - st.sendWindow
		st.sendWindow = w
		st.cond.Broadcast()
	}
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
	if len(st.buf)-st.off+st.unacked+len(p) > st.s.recvWindow {
		return fmt.Errorf("agentproto: stream %d overran its window", st.id)
	}
	if st.err == nil {
		if st.off > 0 && len(st.buf)+len(p) > cap(st.buf) {
			// Reuse what was read rather than grow.
			n := copy(st.buf, st.buf[st.off:])
			st.buf, st.off = st.buf[:n], 0
		}
		st.buf = append(st.buf, p...)
	}
	st.recvd += int64(len(p))
	st.cond.Broadcast()
	return nil
}

func (st *Stream) granted(n uint32) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if int64(st.credit)+int64(n) > int64(st.sendWindow) {
		return fmt.Errorf("agentproto: stream %d granted more than its window", st.id)
	}
	st.credit += int(n)
	st.grantsIn++
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
	for len(st.buf) == st.off && !st.remoteEOF && st.err == nil {
		st.cond.Wait()
	}
	if len(st.buf) == st.off {
		err := st.err
		st.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	n := copy(p, st.buf[st.off:])
	st.off += n
	if st.off == len(st.buf) {
		st.buf, st.off = st.buf[:0], 0
	}
	st.unacked += n
	var grant int
	if st.unacked >= grantAt && !st.remoteEOF {
		grant, st.unacked = st.unacked, 0
		st.grantsUp++
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
		if st.credit == 0 && st.err == nil && !st.localClosed {
			st.stalledAt = time.Now()
			for st.credit == 0 && st.err == nil && !st.localClosed {
				st.cond.Wait()
			}
			st.stalledAt, st.stallLogged = time.Time{}, false
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
		st.sent += int64(n)
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
	st.buf, st.off = nil, 0
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

// WatchStalls calls logf, until the session ends, once for each time a
// stream has had bytes to send but no credit for longer than after, and
// once for each write to the peer that has been blocked that long: a
// stalled link says which of the two it is, and how far each stream got.
func (s *Session) WatchStalls(logf func(format string, args ...any), after time.Duration) {
	go func() {
		t := time.NewTicker(max(after/5, 10*time.Millisecond))
		defer t.Stop()
		var writeLogged int64
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
			}
			now := time.Now()
			ago := func(ns int64) time.Duration { return now.Sub(time.Unix(0, ns)).Round(time.Millisecond) }
			if w := s.writing.Load(); w != 0 && w != writeLogged && ago(w) > after {
				writeLogged = w
				logf("link: a write to the peer has been blocked for %v (last frame from the peer %v ago)", ago(w), ago(s.lastRead.Load()))
			}
			s.mu.Lock()
			streams := make([]*Stream, 0, len(s.streams))
			for _, st := range s.streams {
				streams = append(streams, st)
			}
			peer := s.peerWindow
			s.mu.Unlock()
			for _, st := range streams {
				st.mu.Lock()
				if st.stalledAt.IsZero() || st.stallLogged || now.Sub(st.stalledAt) <= after {
					st.mu.Unlock()
					continue
				}
				st.stallLogged = true
				line := fmt.Sprintf("link: stream %d has waited %v for credit: sent %d (window %d, peer's %d), %d grants in; received %d, %d unread, %d read not yet granted, %d grants out; last frame from the peer %v ago",
					st.id, now.Sub(st.stalledAt).Round(time.Millisecond), st.sent, st.sendWindow, peer, st.grantsIn,
					st.recvd, len(st.buf)-st.off, st.unacked, st.grantsUp, ago(s.lastRead.Load()))
				st.mu.Unlock()
				logf("%s", line)
			}
		}
	}()
}
