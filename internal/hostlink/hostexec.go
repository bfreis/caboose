package hostlink

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
)

// Host exec: with host_exec on, the hello offers OpHostExec, and a client
// of `caboose-agent host` has a command run on this machine, as the user
// the link runs as, in the link's own environment, in the host directory
// of the container one it names. It is a hole in the wall on purpose,
// and the user's to open: nothing the agent sends turns it on, the link's
// Decider decides every request first (trust = hostile refuses them all,
// whatever host_exec says), and what runs is the request's argv alone,
// never through a shell. What the host
// still decides is the rest: no terminal, no other user, no variables, a
// directory under a root the container mounts, a bounded number at once,
// and a command whose client is gone killed, with whatever it started.

const (
	// maxHostExec is the most host commands running at once.
	maxHostExec = 16
	// hostExecSetup is how long a host exec has to say what to run.
	hostExecSetup = 10 * time.Second
	// maxHostExecArgs bounds argv; the request's frame bounds its bytes.
	maxHostExecArgs = 1024
	// hostExecKillWait is how long a command has, after SIGTERM, before
	// SIGKILL.
	hostExecKillWait = 3 * time.Second
	// hostExecLinger is how long the output waits, once the command has
	// exited, for more from what it left running: past it, a background
	// process holding the output open no longer holds up the exit.
	hostExecLinger = time.Second
	// hostExecLogMax is the most lines host exec logs a period
	// (hostExecLogEvery): the sandbox decides how often it runs one.
	hostExecLogMax   = 60
	hostExecLogEvery = time.Minute
)

// HostExec is host_exec's offer: commands run on this machine for the
// sandbox (OpHostExec).
type HostExec struct {
	// Roots are the roots the container mounts: a command runs in the host
	// directory of the container one it names, which must be under one.
	Roots []config.Root
}

// hostExecState is a Host's count of the commands running and what its log
// has said of them this period.
type hostExecState struct {
	running    int
	logFrom    time.Time
	logged     int
	suppressed int
}

// hostExec serves OpHostExec id: a stream for the client's exec protocol,
// or why there is none.
func (h *Host) hostExec(id uint64) error {
	// The policy first, whatever is offered: under trust = hostile it
	// refuses even with host_exec on.
	if o, _ := h.decider().Decide(policy.Request{Kind: policy.KindHostExec}); o.Verdict != broker.Allowed {
		h.execLogf("host exec: refused: %s", clip(o.Reason, 300))
		return errors.New(o.Reason)
	}
	x := h.cfg.HostExec
	if x == nil {
		return errors.New(agentproto.HostExecOff)
	}
	return h.openExec(id, "host exec", func(s *agentproto.Session, req agentproto.ExecRequest, streams map[string]*agentproto.Stream) (int, string, error) {
		return runHostExec(s, x, req, streams, h.cfg.Execs)
	})
}

// execRunner runs the command a client asked for on the exec protocol's
// session s, req and streams its own, and returns its exit status, a
// word on where it ran for the log, and why it did not run at all.
type execRunner func(s *agentproto.Session, req agentproto.ExecRequest, streams map[string]*agentproto.Stream) (code int, where string, err error)

// openExec admits one more command, of the kind what names in the log,
// and opens the stream for request id on which its client speaks the
// exec protocol, served by run; or says why there is none. Host exec and
// aliases share it, and the bound on how many run at once.
func (h *Host) openExec(id uint64, what string, run execRunner) error {
	// Counted from here to the command's end: the daemon exits for a
	// replace only once none is in, and admits none while it holds them
	// off for the running ones to end.
	gate := h.cfg.Gate
	if !gate.enter() {
		h.execLogf("%s: refused: the daemon is being replaced", what)
		return refuse(agentproto.AliasUnavailable, "the host's caboose daemon is being replaced, once the host commands running end, and admits no new one meanwhile; try again in a minute")
	}
	h.mu.Lock()
	if h.closing {
		// Run is waiting for the commands it ran: no more are added.
		h.mu.Unlock()
		gate.leave()
		return refuse(agentproto.AliasUnavailable, "the link is closing")
	}
	if h.execs.running >= maxHostExec {
		h.mu.Unlock()
		gate.leave()
		h.execLogf("%s: refused: already %d running", what, maxHostExec)
		return refuse(agentproto.AliasUnavailable, "already %d commands running on the host; try again once one ends", maxHostExec)
	}
	h.execs.running++
	// Added under h.mu, before closing is set: never once Run waits.
	h.execWG.Add(1)
	h.mu.Unlock()
	done := func() {
		h.mu.Lock()
		h.execs.running--
		h.mu.Unlock()
		gate.leave()
		h.execWG.Done()
	}
	hdr, _ := json.Marshal(agentproto.StreamHeader{Request: id})
	st, err := h.sess.Open(hdr)
	if err != nil {
		done()
		return err
	}
	go func() {
		defer done()
		h.serveExec(st, what, run)
	}()
	return nil
}

// serveExec is the exec protocol's server on st, for one command, which
// run runs. The client is the sandbox's, so the session takes what an
// exec needs and no more: its three streams, each with the least in
// flight.
func (h *Host) serveExec(st *agentproto.Stream, what string, run execRunner) {
	s := agentproto.NewSessionLimited(st, st, false,
		agentproto.Limits{Window: agentproto.DefaultWindow, Streams: agentproto.ExecStreams})
	defer s.Close()
	req, err := agentproto.ReadExec(s, hostExecSetup)
	if err != nil {
		return
	}
	if err := s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
		return
	}
	streams, err := agentproto.AcceptExecStreams(s, req, hostExecSetup)
	if err != nil {
		return
	}
	// Nothing more is asked of a command without a terminal; what else
	// comes is read and dropped, so it cannot fill the session's backlog.
	go func() {
		for range s.Control() {
		}
	}()
	started := time.Now()
	code, dir, err := run(s, req, streams)
	argv := clip(proposal.Printable(strings.Join(req.Argv, " ")), 200)
	if err != nil {
		h.execLogf("%s %s: %d: %s", what, argv, code, clip(proposal.Printable(err.Error()), 300))
	} else {
		h.execLogf("%s %s in %s: exit %d after %v", what, argv, clip(proposal.Printable(dir), 300), code, time.Since(started).Round(time.Millisecond))
	}
	m := agentproto.Message{Type: agentproto.TypeExit, Code: code}
	if err != nil {
		m.Error = err.Error()
	}
	if s.Send(m) != nil {
		return
	}
	// The client ends the session once it has the exit; this side waits
	// for that, or its sends could be cut short.
	select {
	case <-s.Done():
	case <-time.After(hostExecSetup):
	}
}

// defaultLang is the LANG a command gets when the link has none.
const defaultLang = "en_US.UTF-8"

// HostExecEnv is the whole environment of a command run on the host: PATH,
// HOME and LANG as getenv has them, and nothing else. The link's own
// environment (an agent socket, a token, a docker setting) never reaches a
// command the sandbox asked for; env NAME=VALUE CMD sets what one needs.
func HostExecEnv(getenv func(string) string) []string {
	lang := getenv("LANG")
	if lang == "" {
		lang = defaultLang
	}
	return []string{"PATH=" + getenv("PATH"), "HOME=" + getenv("HOME"), "LANG=" + lang}
}

// check is the host directory req runs in, or why it does not run at all.
func (x *HostExec) check(req agentproto.ExecRequest) (string, error) {
	switch {
	case req.TTY:
		return "", errors.New("a command on the host gets no terminal")
	case req.User != "":
		return "", errors.New("a command on the host runs as the user the link runs as, and takes no other")
	case len(req.Env) > 0:
		return "", errors.New("a command on the host gets only PATH, HOME and LANG, and takes no variables (env NAME=VALUE CMD sets one)")
	case len(req.Argv) > maxHostExecArgs:
		return "", fmt.Errorf("over %d arguments", maxHostExecArgs)
	case req.Argv[0] == "":
		return "", errors.New("an empty command")
	}
	for _, a := range req.Argv {
		if strings.ContainsRune(a, 0) {
			return "", errors.New("an argument holds a NUL byte")
		}
	}
	var shared []string
	for _, r := range x.Roots {
		shared = append(shared, r.Container)
	}
	dir, ok := config.HostPath(x.Roots, req.Dir)
	if !ok {
		where := "the link knows of no root the sandbox mounts"
		if len(shared) > 0 {
			where = "those are " + strings.Join(shared, ", ")
		}
		return "", fmt.Errorf("%s is not under a root this machine shares with the sandbox (%s): run it from under one, or pass -C DIR",
			clip(proposal.Printable(req.Dir), 300), where)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is no directory on the host (%s there)", clip(proposal.Printable(req.Dir), 300), dir)
	}
	return dir, nil
}

// runHostExec runs req and waits for it, its output copied to its streams;
// err is why it did not run. dir is the host directory it ran in.
func runHostExec(s *agentproto.Session, x *HostExec, req agentproto.ExecRequest, streams map[string]*agentproto.Stream, own *Execs) (code int, dir string, err error) {
	closeAll := func() {
		for _, st := range streams {
			_ = st.CloseWrite()
		}
	}
	if dir, err = x.check(req); err != nil {
		closeAll()
		return agentproto.ExitCannotRun, "", err
	}
	path := req.Argv[0]
	if !strings.Contains(path, "/") {
		// The link's own PATH: the daemon's, which it took from the
		// user's login shell as it started (a detached helper's is the
		// terminal's that started it). One relative to Dir is resolved
		// there by exec.Cmd.
		if path, err = exec.LookPath(path); err != nil {
			closeAll()
			return agentproto.ExitNotFound, dir, fmt.Errorf("%s: not found in the host's PATH", clip(proposal.Printable(req.Argv[0]), 100))
		}
	}
	cmd := &exec.Cmd{Path: path, Args: req.Argv, Dir: dir, Env: HostExecEnv(os.Getenv),
		// Its own group, so a kill reaches what it started too.
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	code, err = runCmd(s, cmd, streams, own, runLimits{})
	return code, dir, err
}

// runLimits are what a command run by runCmd is held to beyond its
// client's life.
type runLimits struct {
	// timeout kills it (its group, SIGTERM then SIGKILL) once it has run
	// that long; 0 for no limit.
	timeout time.Duration
	// noRemains kills whatever is left of its group once the command
	// itself has exited, and waits for that to be gone, so nothing it
	// started outlives it.
	noRemains bool
}

// runCmd starts cmd, which must have Setpgid, and waits for it, its output
// copied to its streams, its stdin from theirs; err is why it did not
// start. The command and its group are killed when the session s ends, and
// as lim says. cmd's Stdin, Stdout and Stderr are runCmd's to set.
func runCmd(s *agentproto.Session, cmd *exec.Cmd, streams map[string]*agentproto.Stream, own *Execs, lim runLimits) (int, error) {
	closeAll := func() {
		for _, st := range streams {
			_ = st.CloseWrite()
		}
	}
	out, errs := streams[agentproto.StreamStdout], streams[agentproto.StreamStderr]
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll()
		return agentproto.ExitCannotRun, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		closeAll()
		return agentproto.ExitCannotRun, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if in := streams[agentproto.StreamStdin]; in != nil {
		// Copied here rather than by exec.Cmd, whose Wait would wait for
		// a stdin that may never end.
		stdin, err := cmd.StdinPipe()
		if err != nil {
			outR.Close()
			outW.Close()
			errR.Close()
			errW.Close()
			closeAll()
			return agentproto.ExitCannotRun, err
		}
		go func() {
			_, _ = io.Copy(stdin, in)
			stdin.Close()
		}()
	}
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		closeAll()
		if errors.Is(err, os.ErrNotExist) {
			return agentproto.ExitNotFound, err
		}
		return agentproto.ExitCannotRun, err
	}
	g := &group{pid: cmd.Process.Pid, done: make(chan struct{})}
	running.track(g, true)
	defer running.track(g, false)
	if own != nil {
		own.track(g, true)
		defer own.track(g, false)
	}
	var deadline <-chan time.Time
	if lim.timeout > 0 {
		t := time.NewTimer(lim.timeout)
		defer t.Stop()
		deadline = t.C
	}
	go func() {
		// The client gone, or the link, or its time up: the command goes
		// too.
		select {
		case <-s.Done():
			g.terminate()
		case <-deadline:
			g.terminate()
		case <-g.done:
		}
	}()
	var copied sync.WaitGroup
	copied.Add(2)
	go func() { defer copied.Done(); drain(out, outR, g.done) }()
	go func() { defer copied.Done(); drain(errs, errR, g.done) }()
	werr := cmd.Wait()
	g.exit()
	if lim.noRemains {
		g.killRemains()
	}
	copied.Wait()
	_ = out.CloseWrite()
	_ = errs.CloseWrite()
	return exitStatus(cmd, werr), nil
}

// drain copies r to dst until r ends, or until the command has exited
// (exited) and nothing more has come for hostExecLinger. A dst that fails
// (the client gone) has the rest dropped, so the command never blocks on
// a full pipe.
func drain(dst io.Writer, r *os.File, exited <-chan struct{}) {
	defer r.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-exited:
			_ = r.SetReadDeadline(time.Now().Add(hostExecLinger))
		case <-stop:
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				dst = io.Discard
			}
			select {
			case <-exited:
				_ = r.SetReadDeadline(time.Now().Add(hostExecLinger))
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

// exitStatus is how cmd ended, as a shell reports it: 128 plus the
// signal that killed it.
func exitStatus(cmd *exec.Cmd, err error) int {
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ps.ExitCode()
	}
	if err != nil {
		return agentproto.ExitCannotRun
	}
	return 0
}

// group is a running host command's process group. It is signalled only
// until its leader is reaped, after which the ID may be another's.
type group struct {
	mu     sync.Mutex
	pid    int
	exited bool
	done   chan struct{}
}

func (g *group) signal(sig syscall.Signal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.exited {
		_ = syscall.Kill(-g.pid, sig)
	}
}

// killRemains, once the leader is reaped, ends what is left of its group:
// SIGTERM, then SIGKILL hostExecKillWait later, and returns once no
// process of the group is left, or a while after the SIGKILL. A group
// whose ID is still in use cannot have it handed to another process, so
// signalling it is safe while any member lives; with none left, the only
// risk is a new process given this very ID as its own group's between the
// reap and now, which takes the system's whole PID range going round.
// A process that left the group (setsid, setpgid) is out of its reach.
func (g *group) killRemains() {
	alive := func() bool { return syscall.Kill(-g.pid, 0) == nil }
	if !alive() {
		return
	}
	_ = syscall.Kill(-g.pid, syscall.SIGTERM)
	for _, wait := range []time.Duration{hostExecKillWait, remainsGoneWait} {
		end := time.Now().Add(wait)
		for alive() && time.Now().Before(end) {
			time.Sleep(20 * time.Millisecond)
		}
		if !alive() {
			return
		}
		_ = syscall.Kill(-g.pid, syscall.SIGKILL)
	}
}

// remainsGoneWait is how long killRemains waits, after the SIGKILL, for
// the group to be gone.
const remainsGoneWait = 2 * time.Second

// exit records that the leader is reaped.
func (g *group) exit() {
	g.mu.Lock()
	g.exited = true
	g.mu.Unlock()
	close(g.done)
}

// terminate sends the group SIGTERM, then SIGKILL if its leader is still
// there hostExecKillWait later, and returns once it is gone.
func (g *group) terminate() {
	g.signal(syscall.SIGTERM)
	select {
	case <-g.done:
		return
	case <-time.After(hostExecKillWait):
	}
	g.signal(syscall.SIGKILL)
	<-g.done
}

// Execs are host commands running: one link's, or every link's in the
// process (KillHostExecs). A link that keeps its own (Config.Execs) can
// kill and count its commands without touching another link's.
type Execs struct {
	mu     sync.Mutex
	groups map[*group]bool
}

// NewExecs is an empty set.
func NewExecs() *Execs { return &Execs{groups: map[*group]bool{}} }

func (e *Execs) track(g *group, on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if on {
		e.groups[g] = true
	} else {
		delete(e.groups, g)
	}
}

// Running is how many commands of the set are running.
func (e *Execs) Running() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.groups)
}

// Kill terminates every command of the set still running, and returns once
// they are gone.
func (e *Execs) Kill() {
	e.mu.Lock()
	gs := make([]*group, 0, len(e.groups))
	for g := range e.groups {
		gs = append(gs, g)
	}
	e.mu.Unlock()
	var wg sync.WaitGroup
	for _, g := range gs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.terminate()
		}()
	}
	wg.Wait()
}

// running are the host commands running in this process, every link's.
var running = NewExecs()

// KillHostExecs terminates every host command still running in this
// process, every link's, and returns once they are gone: the process's
// last act, at a signal, since its sessions' ends do it only while the
// process lives.
func KillHostExecs() { running.Kill() }

// HostExecsRunning is how many host commands run in this process, every
// link's.
func HostExecsRunning() int { return running.Running() }

// execLogf logs a line of host exec's, at most hostExecLogMax a period.
func (h *Host) execLogf(format string, args ...any) {
	if h.cfg.Log == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, now := &h.execs, time.Now()
	if e.logFrom.IsZero() || now.Sub(e.logFrom) >= hostExecLogEvery {
		if e.suppressed > 0 {
			h.cfg.Log.Printf("host exec: ... and %d more lines not logged (at most %d a minute)", e.suppressed, hostExecLogMax)
		}
		e.logFrom, e.logged, e.suppressed = now, 0, 0
	}
	if e.logged >= hostExecLogMax {
		e.suppressed++
		return
	}
	e.logged++
	h.cfg.Log.Printf(format, args...)
}
