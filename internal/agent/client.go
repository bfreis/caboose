package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

// Main is caboose-agent's command line.
func Main(args []string, stdin io.Reader, stdout io.WriteCloser, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprint(stderr, `usage: caboose-agent COMMAND

  open URL            open an http(s) URL in the host's browser
  notify [-t TITLE] TEXT
                      show a notification on the host
  ports               what listens in the sandbox, and what the host forwards
  link                the sandbox's end of the link (the host's caboose runs it)
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
	case "link":
		if len(rest) != 0 {
			return usage()
		}
		err = RunLink(stdin, stdout, Config{Socket: SocketPath, ProcRoot: "/proc", Interval: time.Second})
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
