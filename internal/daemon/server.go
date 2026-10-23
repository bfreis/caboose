package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
	"github.com/bfreis/caboose/internal/selfupdate"
)

// DefaultIdle is how long the daemon stays once no client is connected and
// no sandbox it serves runs.
const DefaultIdle = 5 * time.Minute

// maxLog is the most daemon.log grows to before it is moved aside.
const maxLog = 10 << 20

// maxConns caps the connections open at once; one more is closed at once.
// A variable for the tests.
var maxConns = 256

// How long the daemon waits for a client's hello, and for a reply to go out.
const (
	helloTimeout = 2 * time.Second
	writeTimeout = 2 * time.Second
)

// ErrRunning is Run's error when another daemon holds the directory's lock.
var ErrRunning = errors.New("a caboose daemon is already running for this directory")

// Links is what the daemon serves the link requests (ensure_link,
// restart_link, stop_link) with, and asks whether a sandbox it serves still
// runs. A daemon given none answers those requests with
// daemonproto.KindUnknownMethod.
type Links interface {
	// Serve handles one link request for env. An error that is a
	// *daemonproto.Error goes to the client as it is; any other is
	// KindInternal.
	Serve(ctx context.Context, method, env string) (*daemonproto.LinkResult, error)
	// Envs is each environment's state, for status.
	Envs() []daemonproto.EnvStatus
	// Busy reports whether something keeps the daemon from exiting idle,
	// such as a running sandbox it serves.
	Busy() bool
	// HostExecs is how many host-exec commands run.
	HostExecs() int
	// Recovering reports whether the links the daemon ran before it
	// started are still being brought back.
	Recovering() bool
	// DrainHostExecs reports true when no host-exec request is in flight,
	// and then refuses every one from then on, for good: both at once, so
	// that none is admitted between finding none and the daemon's exit for
	// a replace. While one is in flight it reports false, and refuses new
	// ones until the next call if hold, or admits them again if not.
	DrainHostExecs(hold bool) bool
	// Close stops every link, killing what they run, and returns once
	// they have stopped.
	Close()
}

// Brokers is what the daemon serves the request broker's methods
// (requests, request, answer, grants, revoke, permissions) with. A daemon
// given none answers them with daemonproto.KindUnknownMethod.
type Brokers interface {
	// Call handles one such request for env. who is the kind of client
	// the connection's hello named (daemonproto.ClientCLI), which the
	// implementation takes who answered from, never anything in params;
	// params are the request's, already checked to be an object and to
	// name a valid env. The result is encoded as the response's; an error
	// that is a *daemonproto.Error goes to the client as it is, any other
	// as KindInternal.
	Call(ctx context.Context, who, method, env string, params json.RawMessage) (any, error)
}

// Checkpoints is what the daemon serves the checkpoints method with: a
// listing of an environment's checkpoints, read from its store. A daemon
// given none answers it with daemonproto.KindUnknownMethod.
type Checkpoints interface {
	// Checkpoints lists env's checkpoints (env already checked to be a
	// valid name). An error that is a *daemonproto.Error goes to the
	// client as it is, any other as KindInternal.
	Checkpoints(ctx context.Context, env string) (*daemonproto.CheckpointsResult, error)
}

// Writable is what the daemon serves the writable method with: the host
// paths the sandboxes reach, read off them when asked. A daemon given
// none answers it with daemonproto.KindUnknownMethod.
type Writable interface {
	// Writable reads, afresh, what every sandbox under CABOOSE_HOME can
	// write and the roots each mounts read-only. An error is an answer it
	// could not make; the client then works it out itself.
	Writable(ctx context.Context) (*daemonproto.WritableResult, error)
}

type noLinks struct{}

func (noLinks) Serve(context.Context, string, string) (*daemonproto.LinkResult, error) {
	return nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "this daemon does not run links")
}
func (noLinks) Envs() []daemonproto.EnvStatus { return []daemonproto.EnvStatus{} }
func (noLinks) Busy() bool                    { return false }
func (noLinks) HostExecs() int                { return 0 }
func (noLinks) DrainHostExecs(bool) bool      { return true }
func (noLinks) Recovering() bool              { return false }
func (noLinks) Close()                        {}

// Options configure Run.
type Options struct {
	Dir     string // the daemon directory (Dir)
	Version string // this caboose's version
	Dev     bool   // a build from a checkout

	// Idle is how long it stays with no client connected and Links not
	// busy; 0 is DefaultIdle.
	Idle time.Duration
	// Links serves the link requests; nil serves none.
	Links Links
	// Brokers serves the request broker's methods; nil serves none.
	Brokers Brokers
	// Checkpoints serves the checkpoints method; nil serves none.
	Checkpoints Checkpoints
	// Writable serves the writable method; nil serves none.
	Writable Writable
	// Ready, when set, runs once, in a goroutine of its own, as soon as
	// the daemon holds its lock and serves its socket, with its log: what
	// only the one daemon of the directory may do (start the links it ran
	// before) goes there, never before Run.
	Ready func(*log.Logger)
	// Log is where it logs; nil is the directory's daemon.log.
	Log io.Writer
	// LockWait is how long it waits for the directory's lock before
	// returning ErrRunning: a daemon started on demand may meet one that
	// is still cleaning up, or a client probing the lock. 0 waits not at
	// all.
	LockWait time.Duration
	// OnStop, when set, runs once the daemon has stopped and let go of its
	// lock, with what made it stop and its log (still open): what follows
	// a daemon, such as starting the one that replaces it, goes there.
	OnStop func(Stopped, *log.Logger)

	// peerUID reads a connection's peer's user; nil is the system's.
	peerUID func(net.Conn) (uint32, error)
	// Protocol is the protocol its hello claims; 0 is
	// daemonproto.ProtocolVersion. Only tests set it, to play a daemon of
	// another protocol.
	Protocol int
}

// Stopped is what made a daemon stop, for Options.OnStop.
type Stopped struct {
	// Why is the reason, as the log says it.
	Why string
	// Replaced is set when a replace that was not forced made it exit,
	// once no host-exec command ran; Version is the version it named.
	Replaced bool
	Version  string
	// Forced is set when a forced replace made it exit.
	Forced bool
	// Busy is Links.Busy as it was just before the links stopped: a link
	// ran.
	Busy bool
}

// replaceRecheck is how often a daemon being replaced looks again for the
// host-exec commands to have ended. Of every replaceCycle it waits, the
// first replaceHold it admits no new host command, so that short commands
// arriving one after another cannot hold the replace off, while a long one
// (a keep-awake command) keeps host exec refused for a minute at most, and
// only once in a while, not for as long as it runs. Variables for the
// tests.
var (
	replaceRecheck = 2 * time.Second
	replaceHold    = time.Minute
	replaceCycle   = 15 * time.Minute
)

// State is daemon.json: what the running daemon says of itself.
type State struct {
	PID      int       `json:"pid"`
	Version  string    `json:"version"`
	Protocol int       `json:"protocol"`
	Started  time.Time `json:"started"`
	Dev      bool      `json:"dev"`
	Dir      string    `json:"dir"`
}

// ReadState reads the daemon directory's daemon.json.
func ReadState(dir string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

type server struct {
	opts    Options
	links   Links
	log     *log.Logger
	ln      *net.UnixListener
	own     uint32
	peerUID func(net.Conn) (uint32, error)
	started time.Time
	ctx     context.Context // ends when the daemon stops
	cancel  context.CancelFunc

	mu       sync.Mutex
	conns    map[net.Conn]bool // true once past the hello
	stopping bool              // set once it stops: no connection is taken after
	wg       sync.WaitGroup

	stopOnce sync.Once
	stopped  chan struct{}
	why      string

	// Under mu: a replace not forced waits for the host-exec commands
	// (replacing), for the version it named; what made the daemon stop.
	replacing      bool
	replaceVersion string
	result         Stopped

	tableOnce sync.Once
	table     map[string]*method // the method table, by name (lookup)

}

// Run is the daemon: it takes the directory's lock (ErrRunning when another
// daemon holds it), listens on its socket, and serves until ctx ends, a
// client sends stop, or it has been idle for Options.Idle. It removes its
// socket and daemon.json before it lets go of the lock; then runs
// Options.OnStop.
func Run(ctx context.Context, opts Options) error {
	st, lg, closeLog, err := run(ctx, opts)
	if closeLog != nil {
		defer closeLog()
	}
	if err == nil && opts.OnStop != nil {
		opts.OnStop(st, lg)
	}
	return err
}

// run is Run up to letting go of the lock. It leaves the log open, for
// OnStop: closeLog closes it.
func run(ctx context.Context, opts Options) (st Stopped, lg *log.Logger, closeLog func(), err error) {
	dir := opts.Dir
	if dir == "" {
		return st, nil, nil, errors.New("no daemon directory")
	}
	if err := CheckSocket(dir); err != nil {
		return st, nil, nil, err
	}
	if err := prepareDir(dir); err != nil {
		return st, nil, nil, err
	}
	lk, err := tryLock(filepath.Join(dir, LockFile))
	if err != nil && opts.LockWait > 0 && errors.Is(err, syscall.EWOULDBLOCK) {
		lk, err = lockWait(ctx, filepath.Join(dir, LockFile), opts.LockWait)
	}
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return st, nil, nil, ErrRunning
		}
		return st, nil, nil, fmt.Errorf("the daemon's lock: %w", err)
	}
	defer unlock(lk)

	w := opts.Log
	if w == nil {
		l, err := cappedlog.Open(filepath.Join(dir, LogFile), maxLog, "caboose daemon")
		if err != nil {
			return st, nil, nil, fmt.Errorf("the daemon's log: %w", err)
		}
		closeLog = func() { l.Close() }
		defer func() {
			if err != nil {
				closeLog()
				closeLog = nil
			}
		}()
		w = l
	}
	s := &server{
		opts:    opts,
		links:   opts.Links,
		log:     log.New(w, "", log.LstdFlags),
		own:     ownUID(),
		peerUID: opts.peerUID,
		started: time.Now(),
		conns:   map[net.Conn]bool{},
		stopped: make(chan struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if s.links == nil {
		s.links = noLinks{}
	}
	if s.peerUID == nil {
		s.peerUID = peerUID
	}
	if s.opts.Idle <= 0 {
		s.opts.Idle = DefaultIdle
	}

	// Holding the lock, any socket left is a dead daemon's.
	lg = s.log
	sock := Socket(dir)
	if err := saferm.Remove(dir, sock); err != nil {
		return st, lg, closeLog, fmt.Errorf("removing the last daemon's socket: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return st, lg, closeLog, fmt.Errorf("the daemon's socket: %w", err)
	}
	ln.SetUnlinkOnClose(false) // removed below, through saferm
	s.ln = ln
	// After cleanup, which records whether the links were busy.
	defer func() { st = s.stopResult() }()
	defer s.cleanup()
	if err := os.Chmod(sock, 0o600); err != nil {
		return st, lg, closeLog, fmt.Errorf("the daemon's socket: %w", err)
	}
	if err := s.writeState(); err != nil {
		return st, lg, closeLog, fmt.Errorf("the daemon's state: %w", err)
	}
	s.log.Printf("caboose daemon %s started (pid %d, protocol %d, idle exit after %v)", opts.Version, os.Getpid(), daemonproto.ProtocolVersion, s.opts.Idle)

	go s.accept()
	go s.idle()
	if opts.Ready != nil {
		go opts.Ready(s.log)
	}
	select {
	case <-ctx.Done():
		s.stop("signalled")
	case <-s.stopped:
	}
	s.log.Printf("caboose daemon stopping: %s", s.why)
	return st, lg, closeLog, nil
}

// prepareDir makes the daemon directory, 0700, and refuses one that is not
// a directory of the user's own (a symlink included).
func prepareDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != ownUID() {
		return fmt.Errorf("%s belongs to uid %d, not this user", dir, st.Uid)
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

func (s *server) writeState() error {
	b, err := json.MarshalIndent(State{
		PID: os.Getpid(), Version: s.opts.Version, Protocol: s.protocol(),
		Started: s.started, Dev: s.opts.Dev, Dir: s.opts.Dir,
	}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.opts.Dir, "."+StateFile+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(s.opts.Dir, StateFile))
	}
	if err != nil {
		_ = saferm.Remove(s.opts.Dir, tmp)
	}
	return err
}

// stopResult is what made the daemon stop.
func (s *server) stopResult() Stopped {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.result
	r.Why = s.why
	return r
}

// stop ends the daemon, once.
func (s *server) stop(why string) {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.stopOnce.Do(func() {
		s.why = why
		close(s.stopped)
	})
}

// cleanup closes the socket and every connection, removes the socket and
// daemon.json, stops the links, and waits for the connections' goroutines:
// all before Run lets go of the lock, so a daemon that starts next never
// has its files removed by this one.
func (s *server) cleanup() {
	s.ln.Close()
	for _, f := range []string{SocketFile, StateFile} {
		if err := saferm.Remove(s.opts.Dir, filepath.Join(s.opts.Dir, f)); err != nil {
			s.log.Printf("removing %s: %v", f, err)
		}
	}
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.cancel()
	busy := s.links.Busy()
	s.mu.Lock()
	s.result.Busy = busy
	s.mu.Unlock()
	s.links.Close()
	s.wg.Wait()
	s.log.Printf("caboose daemon stopped")
}

func (s *server) accept() {
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			select {
			case <-s.stopped:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				s.stop("its socket closed")
				return
			}
			s.log.Printf("accepting a connection: %v", err)
			time.Sleep(50 * time.Millisecond) // out of descriptors: don't spin
			continue
		}
		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			c.Close()
			return
		}
		if len(s.conns) >= maxConns {
			s.mu.Unlock()
			c.Close()
			s.log.Printf("refused a connection: %d are open already", maxConns)
			continue
		}
		s.conns[c] = false
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(c)
	}
}

// idle stops the daemon once no client has been connected, and the links
// not been busy, for Options.Idle. A connection still in its hello
// counts: the decision and the stopping flag are taken under s.mu, which
// accept holds to take a connection, so no client is let in to find the
// daemon gone.
func (s *server) idle() {
	every := min(max(s.opts.Idle/10, 10*time.Millisecond), time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-s.stopped:
			return
		case now := <-t.C:
			busy := s.links.Busy()
			s.mu.Lock()
			switch {
			case busy || len(s.conns) > 0:
				last = now
				s.mu.Unlock()
			case now.Sub(last) >= s.opts.Idle:
				s.stopping = true
				s.mu.Unlock()
				s.stop(fmt.Sprintf("idle for %v", s.opts.Idle))
				return
			default:
				s.mu.Unlock()
			}
		}
	}
}

func (s *server) serve(c *net.UnixConn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()
	uid, err := s.peerUID(c)
	if err == nil {
		err = checkPeer(uid, s.own)
	}
	if err != nil {
		s.log.Printf("refused a connection: %v", err)
		return
	}
	conn := daemonproto.NewConn(c)
	proto := s.protocol()
	hello, cmp, err := daemonproto.ServerHandshakeProtocol(conn, s.opts.Version, proto, helloTimeout)
	if err != nil {
		s.log.Printf("a client's hello: %v", err)
		return
	}
	c0 := caller{Kind: hello.Client, hello: hello, cmp: cmp}
	s.mu.Lock()
	stopping := s.stopping
	s.conns[c] = true
	s.mu.Unlock()
	if stopping {
		return
	}
	for {
		f, err := conn.Read(0)
		if err != nil {
			if errors.Is(err, daemonproto.ErrBadFrame) || errors.Is(err, daemonproto.ErrLineTooLong) {
				_ = conn.Write(daemonproto.Failure(0, daemonproto.NewError(daemonproto.KindBadRequest, "%v", err)), writeTimeout)
			}
			return
		}
		if f.Type != daemonproto.TypeRequest {
			_ = conn.Write(daemonproto.Failure(0, daemonproto.NewError(daemonproto.KindBadRequest, "expected a request, got a %s", f.Type)), writeTimeout)
			return
		}
		resp, then := s.handle(f, c0)
		err = conn.Write(resp, writeTimeout)
		// What was decided is done even when the answer did not go out:
		// a drained replace has closed host exec for good.
		if then != nil {
			then()
		}
		if err != nil {
			return
		}
	}
}

// replace is the replace method: see daemonproto.ReplaceParams. It returns
// the answer, or the refusal, and what to do once it is sent.
func (s *server) replace(p daemonproto.ReplaceParams, hello *daemonproto.Frame, cmp daemonproto.Compat) (*daemonproto.ReplaceResult, *daemonproto.Error, func()) {
	who := fmt.Sprintf("caboose %s (%s, protocol %d)", daemonproto.Text(hello.Version, 64), daemonproto.Text(hello.Client, 16), hello.Protocol)
	want := daemonproto.Text(p.Version, 64)
	if p.Force {
		if cmp != daemonproto.CompatClientNewer {
			s.log.Printf("replace: refused a forced replace from %s, which speaks this daemon's protocol", who)
			return nil, daemonproto.NewError(daemonproto.KindBadRequest, "force is only for a caboose of a newer protocol; use caboose daemon restart"), nil
		}
		n := s.links.HostExecs()
		s.log.Printf("replace: %s asks to replace this daemon now (as %s), killing %d host command(s)", who, want, n)
		s.mu.Lock()
		s.result.Forced = true
		s.mu.Unlock()
		return &daemonproto.ReplaceResult{Exiting: true, HostExecs: n}, nil, func() {
			s.stop(fmt.Sprintf("replaced by a caboose of protocol %d", hello.Protocol))
		}
	}
	if selfupdate.Valid(s.opts.Version) && selfupdate.Valid(p.Version) && selfupdate.Compare(s.opts.Version, p.Version) >= 0 {
		s.log.Printf("replace: %s asks for %s; this daemon is %s already: nothing to do", who, want, s.opts.Version)
		return &daemonproto.ReplaceResult{Current: true}, nil, nil
	}
	s.mu.Lock()
	waiting := s.replacing
	s.replacing, s.replaceVersion = true, p.Version
	s.mu.Unlock()
	if s.drain(true) {
		s.log.Printf("replace: %s asks for %s; no host command runs: exiting", who, want)
		return &daemonproto.ReplaceResult{Exiting: true}, nil, func() { s.stopReplaced() }
	}
	n := s.links.HostExecs()
	s.log.Printf("replace: %s asks for %s; waiting for the host commands to end (%d running), admitting no new one for %v", who, want, n, replaceHold)
	if !waiting {
		go s.awaitDrain(replaceRecheck, time.Now())
	}
	return &daemonproto.ReplaceResult{HostExecs: n}, nil, nil
}

// drain closes host exec for good, and reports true, when no host-exec
// request is in flight; else it holds new ones off, or admits them again,
// as hold says.
func (s *server) drain(hold bool) bool { return s.links.DrainHostExecs(hold) }

// holding is whether a replace that began waiting at since holds new host
// commands off at now: the first replaceHold of every replaceCycle.
func holding(since, now time.Time) bool {
	return now.Sub(since)%replaceCycle < replaceHold
}

// awaitDrain stops the daemon, for a replace that began waiting at since,
// once no host-exec command runs, looking every every.
func (s *server) awaitDrain(every time.Duration, since time.Time) {
	t := time.NewTicker(every)
	defer t.Stop()
	held := true
	for {
		select {
		case <-s.stopped:
			return
		case now := <-t.C:
			hold := holding(since, now)
			if s.drain(hold) {
				s.log.Printf("replace: the host commands have ended: exiting")
				s.stopReplaced()
				return
			}
			if hold != held {
				if hold {
					s.log.Printf("replace: %d host command(s) still run: admitting no new one for %v", s.links.HostExecs(), replaceHold)
				} else {
					s.log.Printf("replace: %d host command(s) still run: admitting new ones again, and waiting", s.links.HostExecs())
				}
				held = hold
			}
		}
	}
}

// stopReplaced stops the daemon for a replace that was not forced, once
// host exec is drained.
func (s *server) stopReplaced() {
	s.mu.Lock()
	v := s.replaceVersion
	if !s.result.Forced {
		s.result.Replaced, s.result.Version = true, v
	}
	s.mu.Unlock()
	why := "replaced"
	if v != "" {
		why = "replaced by caboose " + daemonproto.Text(v, 64)
	}
	s.stop(why)
}

// protocol is the daemon protocol this daemon speaks: Options.Protocol, or
// this build's.
func (s *server) protocol() int {
	if s.opts.Protocol != 0 {
		return s.opts.Protocol
	}
	return daemonproto.ProtocolVersion
}

func (s *server) status() *daemonproto.StatusResult {
	envs := s.links.Envs()
	if envs == nil {
		envs = []daemonproto.EnvStatus{}
	}
	s.mu.Lock()
	replacing := s.replacing
	s.mu.Unlock()
	info := daemonproto.DaemonInfo{Replacing: replacing,
		PID: os.Getpid(), Version: s.opts.Version, Protocol: s.protocol(),
		Started: s.started, Dev: s.opts.Dev, Dir: s.opts.Dir, HostExecs: s.links.HostExecs(),
		Recovering: s.links.Recovering(),
	}
	return &daemonproto.StatusResult{Daemon: info, Envs: envs}
}
