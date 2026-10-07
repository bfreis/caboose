package syncagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/statesync"
)

// Paths are what a sync works on, as the sandbox sees them.
type Paths struct {
	// Home is the sandbox's home; Repo the sync repo it mounts.
	Home, Repo string
	// Exe is the program that answers git's and ssh's prompts: this one,
	// run as `Exe askpass PROMPT` (Askpass).
	Exe string
}

// SandboxPaths are the sandbox's own: its home, and the sync repo where
// the sandbox mounts it, whoever the sync runs as (the agent user, or root
// under vm and some gVisor setups).
func SandboxPaths() (Paths, error) {
	exe, err := os.Executable()
	return Paths{Home: sandboxcfg.ContainerHome, Repo: statesync.ContainerDir, Exe: exe}, err
}

// ghHelper is gh's credential helper, as git config spells it.
const ghHelper = "!gh auth git-credential"

// session is one sync's conversation with the host. Each question is sent
// and its answer taken under mu: a prompt of git's arrives on another
// goroutine (Askpass's server) than a conflict does. Replies are read by a
// goroutine of their own (readReplies), so that the host going away --
// its stdin closing -- is seen at once, not at the next question.
type session struct {
	mu      sync.Mutex
	in      *Reader
	out     io.Writer
	replies chan Reply
	// gone is closed when the host's side ends; quit when the sync is
	// done, and no question is waited on any more.
	gone, quit chan struct{}
}

func (s *session) send(m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Write(s.out, m)
}

// errHostGone is a question the host is no longer there to answer.
var errHostGone = errors.New("the host's caboose went away")

// errUnsent is a question that could not be sent to the host.
type errUnsent struct{ err error }

func (e *errUnsent) Error() string { return "could not ask the host: " + e.err.Error() }

// ask sends m and waits for the host's Reply.
func (s *session) ask(m Message) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := Write(s.out, m); err != nil {
		return Reply{}, &errUnsent{err}
	}
	select {
	case r, ok := <-s.replies:
		if !ok {
			return Reply{}, errHostGone
		}
		return r, nil
	case <-s.quit:
		return Reply{}, errHostGone
	}
}

// readReplies reads the host's replies until its side ends, then cancels
// the sync: nobody is there to see it through.
func (s *session) readReplies(cancel context.CancelFunc) {
	defer func() {
		close(s.replies)
		close(s.gone)
		cancel()
	}()
	for {
		var r Reply
		if err := s.in.Read(&r); err != nil {
			return
		}
		select {
		case s.replies <- r:
		case <-s.quit:
			return
		}
	}
}

func (s *session) fail(kind, format string, args ...any) int {
	_ = s.send(Message{Type: TypeError, Error: &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}})
	return 1
}

// maxMarkers is MaxMarkers, a variable for the tests.
var maxMarkers = MaxMarkers

// lockWait is how long a sync waits for another in the sandbox to finish.
// A variable for the tests.
var lockWait = 5 * time.Second

// lockRepo takes the sandbox's own sync lock, in repo's .git: the host's
// lock is held only while its caboose runs, and a sync whose host went
// away finishes here on its own.
func lockRepo(repo string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(repo, ".git", "caboose-sync.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if (err != syscall.EWOULDBLOCK && err != syscall.EINTR) || time.Now().After(deadline) {
			f.Close()
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Serve is caboose-agent sync OP: it says hello, reads the host's Request
// from in, runs op (OpRun or OpStatus) on p, and writes what it needs
// answered and its result to out. git's output goes to stderr. The exit
// status is 0 for a sync (or status) that went through. When in ends
// before the sync does -- the host's caboose interrupted, or gone -- the
// sync stops: a command reaching the remote is killed, a merge waiting on
// an answer is undone, and an apply under way is finished first.
func Serve(op string, in io.Reader, out, stderr io.Writer, p Paths) int {
	if op != OpRun && op != OpStatus {
		fmt.Fprintf(stderr, "caboose-agent: sync: %q is not run or status\n", op)
		return 2
	}
	ses := &session{in: NewReader(in, MaxLine), out: out, replies: make(chan Reply),
		gone: make(chan struct{}), quit: make(chan struct{})}
	var once sync.Once
	done := func() { once.Do(func() { close(ses.quit) }) }
	defer done()
	if err := ses.send(Message{Type: TypeHello, Version: Version}); err != nil {
		fmt.Fprintf(stderr, "caboose-agent: sync: %v\n", err)
		return 1
	}
	var req Request
	if err := ses.in.Read(&req); err != nil {
		fmt.Fprintf(stderr, "caboose-agent: sync: reading the request: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ses.readReplies(cancel)
	if req.Version != Version {
		return ses.fail(KindVersion, "the host speaks sync protocol %d, the sandbox %d", req.Version, Version)
	}
	if req.Mounted == nil {
		return ses.fail(KindSetup, "the request names no mounts")
	}
	unlock, err := lockRepo(p.Repo)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ses.fail(KindBusy, "another sync is still running in the sandbox (one whose caboose was interrupted, finishing on its own); try again in a moment")
	}
	if err != nil {
		return ses.fail(KindSetup, "locking the sync repo: %v", err)
	}
	defer unlock()
	s, err := newSyncer(&req, p)
	if err != nil {
		return ses.fail(KindSetup, "%v", err)
	}
	s.Ctx = ctx
	if req.ShowGit {
		s.Stderr = stderr
	}
	stop, err := serveAskpass(s, p, func(a Ask) (string, bool) {
		if !req.Interactive {
			return "", false
		}
		r, err := ses.ask(Message{Type: TypeAsk, Ask: &a})
		if err != nil || r.Abort || r.Answer == nil {
			return "", false
		}
		return *r.Answer, true
	})
	if err != nil {
		return ses.fail(KindSetup, "answering git's prompts: %v", err)
	}
	defer func() {
		done() // a prompt still waiting on the host is not waited for
		stop()
	}()
	if req.Interactive {
		s.Resolve = func(c statesync.Conflict) (statesync.Resolution, error) {
			return resolve(ses, c)
		}
	}
	if op == OpStatus {
		return status(ses, s)
	}
	return run(ses, s, req.Remote)
}

// resolve asks the host to settle c.
func resolve(ses *session, c statesync.Conflict) (statesync.Resolution, error) {
	m := &Conflict{Path: c.Path, JSON: c.JSON, Keys: c.Keys, Note: c.Note, OursDeleted: c.Ours == nil, TheirsDeleted: c.Theirs == nil}
	if data, err := c.Markers(); err != nil {
		m.MarkersErr = err.Error()
	} else if len(data) > maxMarkers {
		m.MarkersErr = fmt.Sprintf("too large to edit from here (%d MiB); keep a side, or settle it in the sandbox", len(data)>>20)
	} else {
		m.Markers = data
	}
	r, err := ses.ask(Message{Type: TypeConflict, Conflict: m})
	var unsent *errUnsent
	switch {
	case errors.As(err, &unsent):
		return statesync.Resolution{}, fmt.Errorf("%s changed on both sides, and %v", c.Path, err)
	case err != nil || r.Abort:
		return statesync.Resolution{}, statesync.ErrAborted
	case r.Take == TakeOurs:
		return c.Take(statesync.Ours), nil
	case r.Take == TakeTheirs:
		return c.Take(statesync.Theirs), nil
	case r.Resolution != nil:
		return *r.Resolution, nil
	}
	return statesync.Resolution{}, statesync.ErrAborted
}

// newSyncer is the Syncer req asks for, on p, with git set up as the
// sync's is: the sandbox's global git config never read (it can arrive by
// sync from another machine, and an url.*.insteadOf, a core.sshCommand or
// a credential helper in it would redirect the sync's own push), gh's
// credential helper for an HTTPS remote when gh is installed, and the
// remote's SSH host key kept in the sync repo, which outlives the sandbox.
func newSyncer(req *Request, p Paths) (*statesync.Syncer, error) {
	c, err := sandboxcfg.Parse(req.Sandbox)
	if err != nil {
		return nil, fmt.Errorf("the sandbox config sent does not parse: %v", err)
	}
	sb := &datadir.Sandbox{Config: c, Data: req.Sandbox}
	if req.SandboxErr != "" {
		sb.Err = errors.New(req.SandboxErr)
	}
	s := &statesync.Syncer{
		Home: p.Home, Repo: p.Repo, Host: req.Host,
		Sandbox: sb, Roots: req.Roots, Defaults: req.Defaults, Mounted: req.Mounted,
		Budget: time.Duration(req.Budget) * time.Second,
	}
	s.Env = []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + p.Home}
	if !req.Interactive {
		s.Env = append(s.Env, "GIT_TERMINAL_PROMPT=0")
	}
	// An ssh command the sandbox's environment sets is the user's to
	// keep; git would take GIT_SSH_COMMAND over it.
	if os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		ssh := "ssh -o 'UserKnownHostsFile=" + p.Repo + "/.git/known_hosts ~/.ssh/known_hosts'"
		if req.Auto {
			// No host key, passphrase or password prompt: fail instead.
			ssh += " -o BatchMode=yes"
		}
		s.Env = append(s.Env, "GIT_SSH_COMMAND="+ssh)
	}
	// Docker Desktop's file sharing reports a mount's root as owned by
	// root for a moment after the host changes something under it, and
	// under vm and gVisor the sync may run as root: git would refuse the
	// repo as "dubious ownership". The check guards against a repo of
	// another user's, and this one is the sandbox's to write anyway. Only
	// on the command line, which is protected config.
	s.GitArgs = []string{"-c", "safe.directory=" + p.Repo}
	if _, err := exec.LookPath("gh"); err == nil {
		// The empty one first: it clears any helper the image's system
		// config names, so gh's is the only one asked.
		s.GitArgs = append(s.GitArgs, "-c", "credential.helper=", "-c", "credential.helper="+ghHelper)
	}
	return s, nil
}

// run is OpRun: the repo set up, the remote set when asked, and one sync.
func run(ses *session, s *statesync.Syncer, remote string) int {
	if err := s.Init(); err != nil {
		return ses.fail(KindSetup, "setting up the sync repo: %v", err)
	}
	if remote != "" {
		if err := s.SetRemote(remote); err != nil {
			return ses.fail(KindSetup, "setting the sync remote: %v", err)
		}
	}
	if s.Remote() == "" {
		return ses.fail(KindNoRemote, "%v", statesync.ErrNoRemote)
	}
	r, err := s.Sync()
	if r != nil {
		if err := ses.send(Message{Type: TypeReport, Report: r}); err != nil {
			return 1
		}
	}
	if err != nil {
		return failed(ses, err)
	}
	return 0
}

// failed sends err as the Error of its kind.
func failed(ses *session, err error) int {
	e := &Error{Kind: KindSync, Message: err.Error()}
	var se *statesync.SecretsError
	switch {
	case errors.As(err, &se):
		e.Kind, e.Paths = KindSecrets, se.Paths
	case errors.Is(err, statesync.ErrAborted):
		e.Kind = KindAborted
	case errors.Is(err, statesync.ErrNoRemote):
		e.Kind = KindNoRemote
	case errors.Is(err, statesync.ErrSandboxConfig):
		e.Kind = KindSandboxConfig
	case errors.Is(err, statesync.ErrNoAnswer):
		e.Kind = KindNoAnswer
	case errors.Is(err, statesync.ErrStopped):
		e.Kind = KindStopped
	}
	_ = ses.send(Message{Type: TypeError, Error: e})
	return 1
}

// status is OpStatus: what would be sent, the repo's state, and what a
// fetch brings. It writes nothing but what the fetch does.
func status(ses *session, s *statesync.Syncer) int {
	if s.Remote() == "" {
		return ses.fail(KindNoRemote, "%v", statesync.ErrNoRemote)
	}
	st, err := PendingStatus(s)
	if err != nil {
		return failed(ses, err)
	}
	st.Unmounted = s.Unmounted()
	st.Remote = true
	st.Dirty, _ = s.Dirty()
	st.Unsent, _ = s.Unsent()
	if err := s.Fetch(); err != nil {
		st.FetchErr = err.Error()
	} else {
		if st.Divergence, err = s.Divergence(); err != nil {
			st.DivergenceErr = err.Error()
		}
		if st.Taken, st.Others, err = s.Incoming(); err != nil {
			return failed(ses, err)
		}
	}
	if err := ses.send(Message{Type: TypeStatus, Status: st}); err != nil {
		return 1
	}
	return 0
}
