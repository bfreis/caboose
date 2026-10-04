package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
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
	"or egress_proxy is off (config.toml on the host), or the sandbox is not under isolation vm")

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
  wait-proxy [SECONDS]
                      wait until the outbound proxy serves (30 seconds at most
                      by default), under vm
  link                the sandbox's end of the link (the host's caboose runs it)
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
