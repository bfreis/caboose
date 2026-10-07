package agent

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/syncagent"
)

// ErrNoLink is a command run while no host is linked.
var ErrNoLink = errors.New("no host is linked to this sandbox right now (the host's caboose link helper is not running)")

// ask sends one request to the link on socket and returns its answer.
func ask(socket string, m agentproto.Message) (agentproto.Message, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return agentproto.Message{}, ErrNoLink
	}
	defer c.Close()
	b, err := json.Marshal(m)
	if err != nil {
		return agentproto.Message{}, err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return agentproto.Message{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(c, agentproto.MaxPayload)).ReadBytes('\n')
	if err != nil {
		return agentproto.Message{}, fmt.Errorf("the link gave no answer: %w", err)
	}
	var r agentproto.Message
	if err := json.Unmarshal(line, &r); err != nil {
		return r, err
	}
	if !r.OK {
		msg := r.Error
		if msg == "" {
			msg = "refused"
		}
		return r, errors.New(msg)
	}
	return r, nil
}

// Open asks the host to open url in its browser.
func Open(socket, url string) error {
	_, err := ask(socket, agentproto.Message{Op: agentproto.OpOpen, URL: url})
	return err
}

// Notify asks the host to show a notification.
func Notify(socket, title, text string) error {
	_, err := ask(socket, agentproto.Message{Op: agentproto.OpNotify, Title: title, Text: text})
	return err
}

// Ports writes what listens here and what the host forwards of it.
func Ports(socket string, w io.Writer) error {
	r, err := ask(socket, agentproto.Message{Op: OpStatus})
	if err != nil {
		return err
	}
	fwd := map[int]agentproto.Forward{}
	for _, f := range r.Forwards {
		fwd[f.Port] = f
	}
	if len(r.Ports) == 0 {
		fmt.Fprintln(w, "nothing is listening on a TCP port in the sandbox")
		return nil
	}
	for _, p := range r.Ports {
		f, ok := fwd[p]
		switch {
		case ok && f.HostPort != 0:
			fmt.Fprintf(w, "%-6d forwarded to the host's localhost:%d\n", p, f.HostPort)
		case ok:
			fmt.Fprintf(w, "%-6d not forwarded: %s\n", p, f.Reason)
		default:
			fmt.Fprintf(w, "%-6d not forwarded yet\n", p)
		}
	}
	return nil
}

// ErrNoProxy is a connect while nothing serves the outbound proxy.
var ErrNoProxy = errors.New("caboose's outbound proxy is off: no host is linked to this sandbox right now, " +
	"or egress is off in the vm profile (config.toml on the host), or the sandbox is not under isolation vm")

// connectTimeout bounds the proxy's answer to a CONNECT: above its own
// wait for the host.
const connectTimeout = 30 * time.Second

// Connect makes a TCP connection to host:port through the outbound proxy
// at proxy, and copies stdin to it and it to stdout until the far end
// closes: ssh's ProxyCommand (`caboose-agent connect %h %p`).
func Connect(proxy, host string, port int, stdin io.Reader, stdout io.Writer) error {
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return ErrNoProxy
	}
	defer c.Close()
	tc := c.(*net.TCPConn)
	target := net.JoinHostPort(host, strconv.Itoa(port))
	_ = tc.SetDeadline(time.Now().Add(connectTimeout))
	if _, err := fmt.Fprintf(tc, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return fmt.Errorf("caboose's outbound proxy at %s: %v", proxy, err)
	}
	lr := &io.LimitedReader{R: tc, N: 64 << 10}
	br := bufio.NewReader(lr)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return fmt.Errorf("caboose's outbound proxy at %s gave no answer to CONNECT %s: %v", proxy, target, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(strings.TrimPrefix(string(b), "caboose: "))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("cannot connect to %s through the host (%d): %s", target, resp.StatusCode, printable(msg))
	}
	_ = tc.SetDeadline(time.Time{})
	lr.N = math.MaxInt64
	go func() {
		_, _ = io.Copy(tc, stdin)
		_ = tc.CloseWrite()
	}()
	if _, err := io.Copy(stdout, br); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("the connection to %s through the host broke: %v", target, err)
	}
	return nil
}

// WaitProxy waits, for at most wait, until the outbound proxy at proxy
// accepts a connection: what the entrypoint and the builder do before
// their first download, since the proxy serves only once the host's link
// is up, which may come just after them.
func WaitProxy(proxy string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		c, err := net.DialTimeout("tcp", proxy, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (waited %v)", ErrNoProxy, wait)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// hostAnswerWait bounds the link's answer to a host exec: above its own
// waits for the host's hello, answer and stream.
const hostAnswerWait = 30 * time.Second

// HostExec runs argv on the host, through the link on socket, in the host
// directory of dir (a path in the sandbox, under a root), with stdio, and
// returns its exit status. A command that never ran is ExitCannotRun and
// why: the host refused it, or nothing answered. Done, ctx closes the
// connection, which ends the command on the host.
func HostExec(ctx context.Context, socket string, argv []string, dir string, stdio agentproto.ExecIO) (int, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return agentproto.ExitCannotRun, ErrNoLink
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	b, _ := json.Marshal(agentproto.Message{Op: agentproto.OpHostExec})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return agentproto.ExitCannotRun, err
	}
	_ = c.SetReadDeadline(time.Now().Add(hostAnswerWait))
	line, err := readLine(c, agentproto.MaxPayload)
	if err != nil {
		if ctx.Err() != nil {
			return agentproto.ExitCannotRun, ctx.Err()
		}
		return agentproto.ExitCannotRun, fmt.Errorf("the link gave no answer: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	var r agentproto.Message
	if err := json.Unmarshal(line, &r); err != nil {
		return agentproto.ExitCannotRun, err
	}
	if !r.OK {
		return agentproto.ExitCannotRun, errors.New(cmp.Or(r.Error, "refused"))
	}
	req := agentproto.ExecRequest{Argv: argv, Dir: dir, Stdin: true}
	code, err := agentproto.Exec(c, req, stdio)
	if err != nil {
		if ctx.Err() != nil {
			return agentproto.ExitCannotRun, ctx.Err()
		}
		return agentproto.ExitCannotRun, fmt.Errorf("the connection to the host broke: %w", err)
	}
	return code, nil
}

// readLine reads up to a newline a byte at a time, so nothing past it is
// taken from r: what follows is another protocol's.
func readLine(r io.Reader, max int) ([]byte, error) {
	var line []byte
	var b [1]byte
	for len(line) < max {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return line, nil
			}
			line = append(line, b[0])
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("an answer over %d bytes", max)
}

// hostCommand is `caboose-agent host`: the command's exit status, or 126
// when it never ran, 130 or 143 when this was interrupted or terminated.
func hostCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, usage func() int) int {
	dir := ""
flags:
	for len(args) > 0 {
		switch {
		case args[0] == "--":
			args = args[1:]
			break flags
		case args[0] == "-C" && len(args) >= 2:
			dir, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "-"):
			return usage()
		default:
			break flags
		}
	}
	if len(args) == 0 {
		return usage()
	}
	cwd, err := os.Getwd()
	if err != nil && (dir == "" || !filepath.IsAbs(dir)) {
		fmt.Fprintf(stderr, "caboose-agent: host: no current directory to run in (%v): pass -C DIR\n", err)
		return agentproto.ExitCannotRun
	}
	switch {
	case dir == "":
		dir = cwd
	case !filepath.IsAbs(dir):
		dir = filepath.Join(cwd, dir)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := HostExec(ctx, SocketPath, args, filepath.Clean(dir),
			agentproto.ExecIO{Stdin: stdin, Stdout: stdout, Stderr: stderr})
		done <- result{code, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			fmt.Fprintf(stderr, "caboose-agent: host: %v\n", r.err)
		}
		return r.code
	case sig := <-sigs:
		// Closing the connection ends the command on the host.
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		if sig == syscall.SIGTERM {
			return 128 + int(syscall.SIGTERM)
		}
		return 128 + int(syscall.SIGINT)
	}
}

// maxProxyWait bounds wait-proxy's SECONDS.
const maxProxyWait = 600

// Main is caboose-agent's command line.
func Main(args []string, stdin io.Reader, stdout io.WriteCloser, stderr io.Writer) int {
	usage := func() int {
		_, _ = io.WriteString(stderr, `usage: caboose-agent COMMAND

  open URL            open an http(s) URL in the host's browser
  notify [-t TITLE] TEXT
                      show a notification on the host
  ports               what listens in the sandbox, and what the host forwards
  connect HOST PORT   a TCP connection made by the host, on stdin and stdout:
                      ssh's ProxyCommand (caboose-agent connect %h %p), under vm
  host [-C DIR] [--] CMD [ARG...]
                      run CMD on the host, as the user who runs caboose, in
                      the host's directory for DIR (by default the current
                      one, which must be under a root); no shell and no
                      terminal: stdin, stdout, stderr and the exit status
                      pass through. Only when host_exec is on, on the host
  wait-proxy [SECONDS]
                      wait until the outbound proxy serves (30 seconds at most
                      by default), under vm
  link                the sandbox's end of the link (the host's caboose runs it)
  sync run|status     caboose sync, run in the sandbox (the host's caboose runs it)
  askpass PROMPT      answers git's and ssh's prompts during a sync
  guest               a vm guest's agent: its control, exec and link ports (the init runs it)
`)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "open":
		if len(rest) != 1 {
			return usage()
		}
		err = Open(SocketPath, rest[0])
	case "notify":
		title := "caboose"
		if len(rest) >= 2 && rest[0] == "-t" {
			title, rest = rest[1], rest[2:]
		}
		if len(rest) == 0 {
			return usage()
		}
		err = Notify(SocketPath, title, strings.Join(rest, " "))
	case "ports":
		if len(rest) != 0 {
			return usage()
		}
		err = Ports(SocketPath, stdout)
	case "connect":
		if len(rest) != 2 {
			return usage()
		}
		port, perr := strconv.Atoi(rest[1])
		if perr != nil || !agentproto.ValidPort(port) {
			fmt.Fprintf(stderr, "caboose-agent: connect: %q is not a port\n", printable(rest[1]))
			return 2
		}
		err = Connect(agentproto.EgressListen, rest[0], port, stdin, stdout)
	case "host":
		return hostCommand(rest, stdin, stdout, stderr, usage)
	case "wait-proxy":
		secs := 30
		if len(rest) > 1 {
			return usage()
		}
		if len(rest) == 1 {
			n, perr := strconv.Atoi(rest[0])
			if perr != nil || n < 0 || n > maxProxyWait {
				fmt.Fprintf(stderr, "caboose-agent: wait-proxy: %q is not a number of seconds up to %d\n", printable(rest[0]), maxProxyWait)
				return 2
			}
			secs = n
		}
		err = WaitProxy(agentproto.EgressListen, time.Duration(secs)*time.Second)
	case "sync":
		if len(rest) != 1 {
			return usage()
		}
		p, perr := syncagent.SandboxPaths()
		if perr != nil {
			fmt.Fprintf(stderr, "caboose-agent: sync: %v\n", perr)
			return 1
		}
		return syncagent.Serve(rest[0], stdin, stdout, stderr, p)
	case "askpass":
		return syncagent.Askpass(rest, stdout, stderr)
	case "link":
		if len(rest) != 0 {
			return usage()
		}
		err = RunLink(stdin, stdout, Config{Socket: SocketPath, ProcRoot: "/proc", Interval: time.Second})
	case "guest":
		if len(rest) != 0 {
			return usage()
		}
		err = RunGuest(stderr)
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		return usage()
	}
	if err != nil {
		fmt.Fprintf(stderr, "caboose-agent: %v\n", err)
		return 1
	}
	return 0
}
