package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// ExecServer serves the exec port: in a vm guest, what docker exec is in a
// container. Whoever can connect runs commands as the sandbox's user, so
// the port is reached only through the host's vm.sock.
type ExecServer struct {
	// Env is every command's environment, under the request's.
	Env []string
	// Dir is where a command runs when the request names no directory.
	Dir string
	// User is who a command runs as when the request names no user, as
	// "UID" or "UID:GID"; empty is the agent's own. Like the container's
	// user under docker exec, it is looked up again at every exec.
	User string
	// Root is the system whose /etc/passwd and /etc/group say a user's
	// groups; empty is "/".
	Root string
	// Refuse, when set, is why no command runs: the guest has not booted.
	Refuse string
}

// setupTimeout is how long a connection has to say what to run.
const setupTimeout = 10 * time.Second

// ServeConn runs the one command conn asks for, and closes conn.
func (e *ExecServer) ServeConn(conn io.ReadWriteCloser) error {
	s := agentproto.NewSession(conn, conn, false)
	defer s.Close()
	req, err := e.request(s)
	if err != nil {
		return err
	}
	if err := s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
		return err
	}
	streams, err := acceptStreams(s, req)
	if err != nil {
		return err
	}
	code, err := e.run(s, req, streams)
	m := agentproto.Message{Type: agentproto.TypeExit, Code: code}
	if err != nil {
		m.Error = err.Error()
	}
	if serr := s.Send(m); serr != nil {
		return serr
	}
	// The host ends the session once it has the exit; this side waits for
	// that, or its sends could be cut short.
	select {
	case <-s.Done():
	case <-time.After(setupTimeout):
	}
	return nil
}

// request reads the hello and the command.
func (e *ExecServer) request(s *agentproto.Session) (agentproto.ExecRequest, error) {
	timeout := time.After(setupTimeout)
	hello := false
	for {
		select {
		case b, ok := <-s.Control():
			if !ok {
				return agentproto.ExecRequest{}, s.Err()
			}
			m, err := agentproto.Decode(b)
			if err != nil {
				return agentproto.ExecRequest{}, err
			}
			switch {
			case m.Type == agentproto.TypeHello:
				if m.Version != agentproto.Version {
					return agentproto.ExecRequest{}, fmt.Errorf("the host speaks version %d, not %d", m.Version, agentproto.Version)
				}
				hello = true
			case m.Type == agentproto.TypeExec && hello && m.Exec != nil && len(m.Exec.Argv) > 0:
				return *m.Exec, nil
			default:
				return agentproto.ExecRequest{}, fmt.Errorf("unexpected %q before an exec", m.Type)
			}
		case <-timeout:
			return agentproto.ExecRequest{}, errors.New("no exec request")
		}
	}
}

// acceptStreams takes the streams the request needs, by name.
func acceptStreams(s *agentproto.Session, req agentproto.ExecRequest) (map[string]*agentproto.Stream, error) {
	want := map[string]bool{agentproto.StreamStdout: true, agentproto.StreamStderr: true}
	if req.Stdin {
		want[agentproto.StreamStdin] = true
	}
	if req.TTY {
		want = map[string]bool{agentproto.StreamTTY: true}
	}
	got := map[string]*agentproto.Stream{}
	deadline := time.AfterFunc(setupTimeout, func() { s.Close() })
	defer deadline.Stop()
	for len(got) < len(want) {
		st, err := s.Accept()
		if err != nil {
			return nil, err
		}
		var h agentproto.StreamHeader
		if json.Unmarshal(st.Header(), &h) != nil || !want[h.Name] || got[h.Name] != nil {
			st.Close()
			return nil, fmt.Errorf("unexpected stream %q", st.Header())
		}
		got[h.Name] = st
	}
	return got, nil
}

// run starts the command and waits for it; err is why it could not start.
func (e *ExecServer) run(s *agentproto.Session, req agentproto.ExecRequest, streams map[string]*agentproto.Stream) (int, error) {
	cmd, code, err := e.command(req)
	if err != nil {
		for _, st := range streams {
			st.CloseWrite()
		}
		return code, err
	}
	if req.TTY {
		return runTTY(s, cmd, req, streams[agentproto.StreamTTY])
	}
	// Nothing more is asked of a command without a terminal; what else
	// comes is read and dropped, so it cannot fill the session's backlog.
	go func() {
		for range s.Control() {
		}
	}()
	out, errs := streams[agentproto.StreamStdout], streams[agentproto.StreamStderr]
	cmd.Stdout, cmd.Stderr = out, errs
	var stdin io.WriteCloser
	if in := streams[agentproto.StreamStdin]; in != nil {
		// Copied here rather than by exec.Cmd, whose Wait would wait for
		// a stdin that may never end.
		if stdin, err = cmd.StdinPipe(); err != nil {
			return agentproto.ExitCannotRun, err
		}
		go func() {
			_, _ = io.Copy(stdin, in)
			stdin.Close()
		}()
	}
	if err := cmd.Start(); err != nil {
		out.CloseWrite()
		errs.CloseWrite()
		return startFailure(err)
	}
	werr := cmd.Wait()
	out.CloseWrite()
	errs.CloseWrite()
	return exitCode(cmd, werr), nil
}

// command is req as a process not yet started, or the status and reason
// it cannot be.
func (e *ExecServer) command(req agentproto.ExecRequest) (*exec.Cmd, int, error) {
	if e.Refuse != "" {
		return nil, agentproto.ExitCannotRun, errors.New(e.Refuse)
	}
	env := append(append([]string{}, e.Env...), req.Env...)
	path, err := lookPath(req.Argv[0], getenv(env, "PATH"))
	if err != nil {
		return nil, agentproto.ExitNotFound, err
	}
	cmd := &exec.Cmd{Path: path, Args: req.Argv, Env: env, Dir: req.Dir}
	if cmd.Dir == "" {
		cmd.Dir = e.Dir
	}
	u, root := req.User, e.Root
	if u == "" {
		u = e.User
	}
	if root == "" {
		root = "/"
	}
	cred, err := userCred(root, u)
	if err != nil {
		return nil, agentproto.ExitCannotRun, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	return cmd, 0, nil
}

// runTTY runs cmd on a new terminal, spliced to st.
func runTTY(s *agentproto.Session, cmd *exec.Cmd, req agentproto.ExecRequest, st *agentproto.Stream) (int, error) {
	master, slave, err := openPTY()
	if err != nil {
		st.CloseWrite()
		return agentproto.ExitCannotRun, err
	}
	defer master.Close()
	if req.Rows > 0 && req.Cols > 0 {
		setSize(master, req.Rows, req.Cols)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr.Setsid, cmd.SysProcAttr.Setctty, cmd.SysProcAttr.Ctty = true, true, 0
	err = cmd.Start()
	slave.Close()
	if err != nil {
		st.CloseWrite()
		return startFailure(err)
	}
	go func() { _, _ = io.Copy(master, st) }()
	go func() {
		for b := range s.Control() {
			if m, err := agentproto.Decode(b); err == nil && m.Type == agentproto.TypeResize && m.Rows > 0 && m.Cols > 0 {
				setSize(master, m.Rows, m.Cols)
			}
		}
	}()
	copied := make(chan struct{})
	go func() {
		// Ends with EIO once no process has the terminal open.
		_, _ = io.Copy(st, master)
		close(copied)
	}()
	werr := cmd.Wait()
	// Whatever the command wrote last may still be in the terminal; a
	// process it left behind holding it open does not keep the exec.
	select {
	case <-copied:
	case <-time.After(250 * time.Millisecond):
		_ = master.SetReadDeadline(time.Now())
		<-copied
	}
	st.CloseWrite()
	return exitCode(cmd, werr), nil
}

// openPTY opens a new terminal pair through /dev/ptmx.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n int
	err = control(master, func(fd int) error {
		var err error
		if n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN); err != nil {
			return err
		}
		return unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)
	})
	if err == nil {
		slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("opening a terminal: %w", err)
	}
	return master, slave, nil
}

func setSize(f *os.File, rows, cols uint16) {
	_ = control(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
}

// control runs fn on f's descriptor. Not f.Fd(), which would make it
// blocking, and no longer end a read at a deadline; and this fails safely
// once f is closed.
func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// startFailure is the status of a command that did not start: 127 when
// there is no such file, 126 otherwise, as a shell has it.
func startFailure(err error) (int, error) {
	if errors.Is(err, os.ErrNotExist) {
		return agentproto.ExitNotFound, err
	}
	return agentproto.ExitCannotRun, err
}

// exitCode is how cmd ended, as a shell reports it: 128 plus the signal
// that killed it.
func exitCode(cmd *exec.Cmd, err error) int {
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

// lookPath finds file on path, the command's own PATH rather than the
// agent's.
func lookPath(file, path string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, file)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: executable file not found in $PATH", file)
}

// getenv is the last value env gives name, as exec.Cmd uses it.
func getenv(env []string, name string) string {
	v := ""
	for _, e := range env {
		if k, val, ok := strings.Cut(e, "="); ok && k == name {
			v = val
		}
	}
	return v
}

// parseUser reads "UID" or "UID:GID"; a user alone gets its UID as GID,
// as docker exec -u 0 gets root's group, unless /etc/passwd gives it
// another (userCred, which also gives it its groups).
func parseUser(u string) (*syscall.Credential, error) {
	us, gs, hasGID := strings.Cut(u, ":")
	uid, err := strconv.ParseUint(us, 10, 32)
	gid := uid
	if err == nil && hasGID {
		gid, err = strconv.ParseUint(gs, 10, 32)
	}
	if err != nil {
		return nil, fmt.Errorf("user %q is not UID or UID:GID", u)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, nil
}
