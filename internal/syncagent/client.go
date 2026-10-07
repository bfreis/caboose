package syncagent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/statesync"
)

// Handler answers, on the host, what the sync in the sandbox asks, and
// bounds the run.
type Handler struct {
	// Conflict settles a conflict; nil aborts the sync.
	Conflict func(*Conflict) Reply
	// Ask answers a prompt; nil refuses it. At most MaxAsks are put to it
	// in one run; the rest are refused.
	Ask func(*Ask) Reply
	// Timeout, when set, bounds the whole run: the sync is told to stop
	// (its stdin closed), and the command killed a moment later.
	Timeout time.Duration
	// Interrupted is called when an interrupt (SIGINT, SIGTERM, SIGHUP)
	// came during the run, the sync was told to stop, and Conflict or Ask
	// still holds the run up (waiting on a terminal): it is to end the
	// process. Without it the run ends once they return.
	Interrupted func()
}

// MaxAsks bounds the prompts one run puts to Handler.Ask: git asks for a
// username and a password, ssh about a host key and a passphrase, and a
// retry or two of those; any more is the sandbox asking for the asking's
// sake.
const MaxAsks = 10

// Result is what a sync or a status ended with: Report (a sync, also one
// that failed on its way), Status, and Err when it did not go through.
type Result struct {
	Report *statesync.Report
	Status *Status
	Err    *Error
}

var (
	// ErrOldAgent is a sandbox whose caboose-agent cannot run the sync
	// this caboose asks for: an image from an older (or newer) caboose.
	ErrOldAgent = errors.New("the sandbox's caboose-agent does not run this caboose's sync")
	// ErrInterrupted is a run interrupted on the host: the sync in the
	// sandbox was told to stop, and does, finishing an apply under way.
	ErrInterrupted = errors.New("interrupted; the sync in the sandbox was told to stop")
	// ErrTimedOut is a run that took longer than Handler.Timeout.
	ErrTimedOut = errors.New("the sync in the sandbox did not finish in time; it was told to stop")
)

// stderrKept bounds what Run keeps of the command's stderr, to say why it
// failed.
const stderrKept = 16 << 10

// stopGrace is how long a sync told to stop has before its command is
// killed. A variable for the tests.
var stopGrace = 3 * time.Second

// Run runs cmd -- caboose-agent sync OP in the sandbox -- sends it req, and
// answers what it asks with h, until it ends. Its stderr (git's output)
// goes to show, when set, once the agent has said hello (an old agent's
// usage text is no use to anyone), and is otherwise only kept to say what
// went wrong. cmd's stdin and stdout are Run's, and it runs in a process
// group of its own: an interrupt at the terminal is Run's to pass on, by
// closing the sync's stdin, which stops it in the sandbox whatever the
// command between (docker exec, a VM's exec) makes of a signal.
func Run(cmd *exec.Cmd, req Request, h Handler, show io.Writer) (*Result, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	g := &gate{show: show, kept: &tail{max: stderrKept}}
	cmd.Stderr = g
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigc)
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var (
		once   sync.Once
		mu     sync.Mutex
		reason error
	)
	stop := func(why error) {
		once.Do(func() {
			mu.Lock()
			reason = why
			mu.Unlock()
			stdin.Close()
			time.AfterFunc(stopGrace, func() { _ = cmd.Process.Kill() })
		})
	}
	var timeout <-chan time.Time
	if h.Timeout > 0 {
		t := time.NewTimer(h.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	conversed := make(chan struct{})
	go func() {
		select {
		case <-sigc:
			stop(ErrInterrupted)
			select {
			case <-conversed:
			case <-time.After(stopGrace + time.Second):
				if h.Interrupted != nil {
					h.Interrupted()
				}
			}
		case <-timeout:
			stop(fmt.Errorf("%w (%s)", ErrTimedOut, h.Timeout))
		case <-conversed:
		}
	}()
	res, convErr := converse(NewReader(stdout, MaxLine), stdin, req, h, g.open)
	close(conversed)
	stdin.Close()
	if convErr != nil {
		_ = cmd.Process.Kill()
	}
	// The rest unread, so a command that writes on does not block.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	mu.Lock()
	stopped := reason
	mu.Unlock()
	switch {
	case stopped != nil:
		return nil, stopped
	case errors.Is(convErr, errNoHello):
		if strings.Contains(g.kept.String(), "usage: caboose-agent") {
			return nil, ErrOldAgent
		}
		return nil, fmt.Errorf("the sync did not start in the sandbox: %s", why(g.kept.String(), waitErr))
	case convErr != nil:
		return nil, convErr
	case res.Report == nil && res.Status == nil && res.Err == nil:
		return nil, fmt.Errorf("the sync in the sandbox ended with no result: %s", why(g.kept.String(), waitErr))
	}
	return res, nil
}

var errNoHello = errors.New("no hello")

// converse is Run's side of the protocol; hello is called once the agent
// has said it.
func converse(in *Reader, out io.Writer, req Request, h Handler, hello func()) (*Result, error) {
	var m0 Message
	if err := in.Read(&m0); err != nil || m0.Type != TypeHello {
		return nil, errNoHello
	}
	if m0.Version != Version {
		return nil, fmt.Errorf("%w (it speaks protocol %d, this caboose %d)", ErrOldAgent, m0.Version, Version)
	}
	hello()
	req.Version = Version
	if err := Write(out, req); err != nil {
		return nil, fmt.Errorf("sending the sync its request: %w", err)
	}
	res := &Result{}
	asks := 0
	for {
		var m Message
		err := in.Read(&m)
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the sync in the sandbox: %w", err)
		}
		var reply Reply
		switch m.Type {
		case TypeConflict:
			reply = Reply{Abort: true}
			if m.Conflict != nil && h.Conflict != nil {
				reply = h.Conflict(m.Conflict)
			}
		case TypeAsk:
			reply = Reply{Abort: true}
			if asks++; m.Ask != nil && h.Ask != nil && asks <= MaxAsks {
				reply = h.Ask(m.Ask)
			}
		case TypeReport:
			res.Report = m.Report
			continue
		case TypeStatus:
			res.Status = m.Status
			continue
		case TypeError:
			res.Err = m.Error
			continue
		default:
			return nil, fmt.Errorf("the sync in the sandbox sent a message of type %q", m.Type)
		}
		if err := Write(out, reply); err != nil {
			return nil, fmt.Errorf("answering the sync in the sandbox: %w", err)
		}
	}
}

// why is what a command that failed said last, or how it ended.
func why(stderr string, err error) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	if err != nil {
		return err.Error()
	}
	return "it said nothing"
}

// gate is the command's stderr: all of it kept, and shown from open on,
// with what came before.
type gate struct {
	mu     sync.Mutex
	show   io.Writer
	kept   *tail
	opened bool
	early  bytes.Buffer
}

func (g *gate) Write(p []byte) (int, error) {
	g.kept.Write(p)
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.show == nil:
	case g.opened:
		_, _ = g.show.Write(p)
	case g.early.Len() < stderrKept:
		g.early.Write(p)
	}
	return len(p), nil
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.show != nil && !g.opened {
		_, _ = g.show.Write(g.early.Bytes())
	}
	g.opened = true
}

// tail is an io.Writer that keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if extra := t.buf.Len() - t.max; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
