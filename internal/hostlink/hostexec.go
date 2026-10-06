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
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// Host exec: with host_exec on, the hello offers OpHostExec, and a client
// of `caboose-agent host` has a command run on this machine, as the user
// the link runs as, in the link's own environment, in the host directory
// of the container one it names. It is a hole in the wall on purpose,
// and the user's to open: nothing the agent sends turns it on, and what
// runs is the request's argv alone, never through a shell. What the host
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
	x := h.cfg.HostExec
	if x == nil {
		return errors.New(agentproto.HostExecOff)
	}
	h.mu.Lock()
	if h.execs.running >= maxHostExec {
		h.mu.Unlock()
		h.execLogf("host exec: refused: already %d running", maxHostExec)
		return fmt.Errorf("already %d commands running on the host; try again once one ends", maxHostExec)
	}
	h.execs.running++
	h.mu.Unlock()
	done := func() {
		h.mu.Lock()
		h.execs.running--
		h.mu.Unlock()
	}
	hdr, _ := json.Marshal(agentproto.StreamHeader{Request: id})
	st, err := h.sess.Open(hdr)
	if err != nil {
		done()
		return err
	}
	go func() {
		defer done()
		h.serveExec(st, x)
	}()
	return nil
}

// serveExec is the exec protocol's server on st, for one command. The
// client is the sandbox's, so the session takes what an exec needs and no
// more: its three streams, each with the least in flight.
func (h *Host) serveExec(st *agentproto.Stream, x *HostExec) {
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
	code, dir, err := runHostExec(s, x, req, streams)
	what := clip(proposal.Printable(strings.Join(req.Argv, " ")), 200)
	if err != nil {
		h.execLogf("host exec %s: %d: %s", what, code, clip(proposal.Printable(err.Error()), 300))
	} else {
		h.execLogf("host exec %s in %s: exit %d after %v", what, clip(proposal.Printable(dir), 300), code, time.Since(started).Round(time.Millisecond))
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

// check is the host directory req runs in, or why it does not run at all.
func (x *HostExec) check(req agentproto.ExecRequest) (string, error) {
	switch {
	case req.TTY:
		return "", errors.New("a command on the host gets no terminal")
	case req.User != "":
		return "", errors.New("a command on the host runs as the user the link runs as, and takes no other")
	case len(req.Env) > 0:
		return "", errors.New("a command on the host runs in the link's own environment, and takes no variables (env NAME=VALUE CMD sets one)")
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
func runHostExec(s *agentproto.Session, x *HostExec, req agentproto.ExecRequest, streams map[string]*agentproto.Stream) (code int, dir string, err error) {
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
		// The link's own PATH: the user's, from the terminal that
		// started it. One relative to Dir is resolved there by exec.Cmd.
		if path, err = exec.LookPath(path); err != nil {
			closeAll()
			return agentproto.ExitNotFound, dir, fmt.Errorf("%s: not found in the host's PATH", clip(proposal.Printable(req.Argv[0]), 100))
		}
	}
	cmd := &exec.Cmd{Path: path, Args: req.Argv, Dir: dir,
		// Its own group, so a kill reaches what it started too.
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}

	out, errs := streams[agentproto.StreamStdout], streams[agentproto.StreamStderr]
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll()
		return agentproto.ExitCannotRun, dir, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		closeAll()
		return agentproto.ExitCannotRun, dir, err
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
			return agentproto.ExitCannotRun, dir, err
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
			return agentproto.ExitNotFound, dir, err
		}
		return agentproto.ExitCannotRun, dir, err
	}
	g := &group{pid: cmd.Process.Pid, done: make(chan struct{})}
	track(g, true)
	defer track(g, false)
	go func() {
		// The client gone, or the link: the command goes too.
		select {
		case <-s.Done():
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
	copied.Wait()
	_ = out.CloseWrite()
	_ = errs.CloseWrite()
	return exitStatus(cmd, werr), dir, nil
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

// running are the host commands running in this process, every session's.
var running = struct {
	sync.Mutex
	groups map[*group]bool
}{groups: map[*group]bool{}}

func track(g *group, on bool) {
	running.Lock()
	defer running.Unlock()
	if on {
		running.groups[g] = true
	} else {
		delete(running.groups, g)
	}
}

// KillHostExecs terminates every host command still running in this
// process, and returns once they are gone: the link's last act, at a
// signal, since its sessions' ends do it only while the process lives.
func KillHostExecs() {
	running.Lock()
	gs := make([]*group, 0, len(running.groups))
	for g := range running.groups {
		gs = append(gs, g)
	}
	running.Unlock()
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
