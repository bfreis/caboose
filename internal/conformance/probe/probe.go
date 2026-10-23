// Package probe is the conformance suite's probe: a small program the
// suite builds outside the image, places into the writable part of its own
// throwaway sandbox, and runs there to send the link the messages the
// sandbox's own tools never send (an answer, a grant, a config change, an
// unknown request, an oversize line) and, given a socket, to try the
// daemon's protocol. It prints what came back, one JSON line per message;
// whether a control held is decided by the suite, from the host.
//
// It refuses to run unless its gate passes: /work holds exactly the suite's
// two roots, named from the nonce it is given, and each root's
// .conformance-root holds exactly that nonce. It writes, removes and
// executes nothing, reads nothing but /work's listing and those two files,
// and dials only the sockets it is given on its command line: it has no
// default socket. In its mount mode (mount.go) it makes, instead, the one
// mount or unmount call it is told, on one of the suite's own mount
// points, or, in its userns mode, enters a user namespace and attempts a
// remount, an unmount and a witness write; both must be refused. The
// mount and userns calls, and the one witness write, are made only in
// mount_linux.go.
package probe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// Exit statuses.
const (
	ExitHeld    = 0 // every message sent was refused as it must be
	ExitNotHeld = 1 // at least one was not: a control did not hold
	ExitUsage   = 2 // bad command line; nothing was checked or sent
	ExitGate    = 3 // the gate refused; nothing was sent
	ExitError   = 4 // something could not be tried (the positive control failed, no link): nothing is proven
)

// Outcomes of one message.
const (
	// Refused: the host answered with a refusal.
	Refused = "refused"
	// Accepted: the peer answered ok.
	Accepted = "accepted"
	// Closed: the peer closed the connection without an answer.
	Closed = "closed"
	// Foreign: the peer answered the daemon's hello in another protocol,
	// its first byte no JSON object's: not the daemon itself, though a
	// wrapper could relay to it, so the verdict is the caller's to
	// confirm (D2 takes it only under vm, for a socket the guest binds
	// itself, as its dockerd's). Any answer that opens as JSON, even one
	// the daemon's protocol refuses, stays NotHeld.
	Foreign = "foreign"
	// Unreachable: no connection could be made.
	Unreachable = "unreachable"
	// NoHost: the sandbox's end of the link answered for itself that the
	// host did not (down, gone, silent): nothing was asked of the host.
	NoHost = "no-host"
	// Failed: the probe could not tell (a bad answer, a timeout, a write
	// that broke): nothing is proven.
	Failed = "error"
	// RateLimited: the host refused the request for coming too fast,
	// before judging it: nothing is proven.
	RateLimited = "rate-limited"
	// Skipped: not sent, since the positive control failed.
	Skipped = "skipped"
)

// Verdicts.
const (
	Held     = "held"     // refused as the control owes
	NotHeld  = "not-held" // the control did not hold
	Unproven = "unproven" // nothing could be told: the suite reports ERROR
)

// Message names, the fixed list. Control is the positive control, sent
// before the others; DaemonHello goes to the daemon socket, the rest to the
// link socket.
const (
	Control     = "status"
	DaemonHello = "daemon-hello"
	// DaemonNoHello goes to the daemon socket too, with a request where
	// the hello must come first: the daemon refuses it and logs "a
	// client's hello" for every such connection that reaches it, which is
	// what the suite's witness on the host counts.
	DaemonNoHello = "daemon-no-hello"
	Oversize      = "oversize"
)

// linkMessages are the link requests, in the order they are sent: each an
// op the host must refuse.
type linkMessage struct {
	name string
	msg  agentproto.Message
}

var linkMessages = []linkMessage{
	{"answer", agentproto.Message{Op: "answer", Title: "caboose-conformance", Text: "approve"}},
	{"approve", agentproto.Message{Op: "approve", Text: "yes"}},
	{"grant", agentproto.Message{Op: "grant", Text: "all"}},
	{"config", agentproto.Message{Op: "config", Text: "[link]\nhost_exec = true"}},
	{"trust", agentproto.Message{Op: "trust", Text: "fallible"}},
	{"daemon", agentproto.Message{Op: "daemon", Text: "stop"}},
	{"unknown", agentproto.Message{Op: "caboose-conformance-unknown"}},
}

// Messages returns every message name the probe knows, in the order it
// sends them, the positive control first.
func Messages() []string {
	out := []string{Control}
	for _, m := range linkMessages {
		out = append(out, m.name)
	}
	return append(out, Oversize, DaemonHello, DaemonNoHello)
}

// toDaemon reports whether the message goes to --daemon's socket.
func toDaemon(name string) bool { return name == DaemonHello || name == DaemonNoHello }

// noHost are the answers the sandbox's end of the link gives for itself
// when it could not ask the host (internal/agent's link.go): a refusal in
// form, but not the host's.
var noHost = []string{"the host link is down", "the host link went down", "the host did not answer", "bad request"}

// rateLimited is the host's refusal of a request that came too fast
// (internal/hostlink's host.go): it says nothing of the op.
const rateLimited = "too many requests"

// Config is one run.
type Config struct {
	// Work is the directory the gate lists: /work in a sandbox.
	Work string
	// Nonce is the suite's, 32 hex digits.
	Nonce string
	// LinkSocket is the link socket's path; required.
	LinkSocket string
	// DaemonSocket is a socket to try the daemon's protocol on; "" sends no
	// DaemonHello.
	DaemonSocket string
	// Only, when set, are the messages to send (the control is always
	// sent first when any link message is).
	Only []string
	// Timeout bounds each message.
	Timeout time.Duration
	// Interval is the wait before each request that reaches the host, so
	// that its rate limit (a burst, then one every two seconds) does not
	// answer in the op's place.
	Interval time.Duration
}

// Outcome is what one message got.
type Outcome struct {
	Message string `json:"message"`
	Outcome string `json:"outcome"`
	// Verdict is what the outcome says of the control: Held, NotHeld or
	// Unproven.
	Verdict string `json:"verdict"`
	// Detail is what the peer said, cut short and printable.
	Detail string `json:"detail,omitempty"`
}

var (
	nonceRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// RootMarker is the file in each root that holds the nonce.
	rootMarker = ".conformance-root"
	// readFlags open the gate's files: read only, never through a link.
	readFlags = os.O_RDONLY | syscall.O_NOFOLLOW
)

// maxMarker bounds a root's marker: the nonce and a newline.
const maxMarker = 64

// Gate checks that the probe is in the suite's sandbox: work lists exactly
// conf-<nonce8>-ro and conf-<nonce8>-dev, real directories, the second
// with a part/ directory, and each root's .conformance-root is a regular
// file holding exactly the nonce and a newline.
func Gate(work, nonce string) error {
	if !nonceRe.MatchString(nonce) {
		return errors.New("the nonce is not 32 lowercase hex digits")
	}
	if !filepath.IsAbs(work) || filepath.Clean(work) != work {
		return fmt.Errorf("%q is not a clean absolute path", work)
	}
	if err := isDir(work); err != nil {
		return err
	}
	ro, dev := "conf-"+nonce[:8]+"-ro", "conf-"+nonce[:8]+"-dev"
	ents, err := os.ReadDir(work)
	if err != nil {
		return fmt.Errorf("listing %s: %v", work, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{dev, ro}) { // ReadDir sorts
		return fmt.Errorf("%s lists %d entries, not exactly %s and %s", work, len(names), ro, dev)
	}
	for _, r := range []string{ro, dev} {
		root := filepath.Join(work, r)
		if err := isDir(root); err != nil {
			return err
		}
		if err := checkMarker(filepath.Join(root, rootMarker), nonce); err != nil {
			return err
		}
	}
	return isDir(filepath.Join(work, dev, "part"))
}

func isDir(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (%v)", p, fi.Mode().Type())
	}
	return nil
}

func checkMarker(p, nonce string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.OpenFile(p, readFlags, 0)
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	defer f.Close()
	if fi2, err := f.Stat(); err != nil || !fi2.Mode().IsRegular() || !os.SameFile(fi, fi2) {
		return fmt.Errorf("%s changed while it was read", p)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMarker+1))
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if string(b) != nonce+"\n" {
		return fmt.Errorf("%s does not hold the nonce", p)
	}
	return nil
}

// checkSocket refuses a socket path that cannot be the one meant.
func checkSocket(flag, p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("%s: %q is not a clean absolute path", flag, p)
	}
	if len(p) > daemonproto.MaxSocketPath {
		return fmt.Errorf("%s: %q is over %d bytes", flag, p, daemonproto.MaxSocketPath)
	}
	return nil
}

// plan is which messages a run sends.
func (c Config) plan() ([]string, error) {
	all := Messages()
	want := c.Only
	if len(want) == 0 {
		want = all
		if c.DaemonSocket == "" {
			want = slices.DeleteFunc(slices.Clone(all), toDaemon)
		}
	}
	for _, w := range want {
		if !slices.Contains(all, w) {
			return nil, fmt.Errorf("%q is not a message the probe knows (%s)", w, strings.Join(all, ", "))
		}
		if toDaemon(w) && c.DaemonSocket == "" {
			return nil, fmt.Errorf("%s needs --daemon SOCKET", w)
		}
	}
	// In the fixed order, each once.
	var out []string
	for _, m := range all {
		if slices.Contains(want, m) {
			out = append(out, m)
		}
	}
	if len(out) > 0 && out[0] != Control && !(len(out) == 1 && toDaemon(out[0])) {
		out = append([]string{Control}, out...)
	}
	return out, nil
}

// Run checks the gate, then sends the messages and returns what each got.
// A gate that refuses is an error, and nothing is dialled.
func Run(c Config) ([]Outcome, error) {
	if err := Gate(c.Work, c.Nonce); err != nil {
		return nil, &GateError{err}
	}
	plan, err := c.plan()
	if err != nil {
		return nil, err
	}
	if err := checkSocket("--link", c.LinkSocket); err != nil {
		return nil, err
	}
	if c.DaemonSocket != "" {
		if err := checkSocket("--daemon", c.DaemonSocket); err != nil {
			return nil, err
		}
	}
	if c.Timeout <= 0 {
		return nil, errors.New("no timeout")
	}
	var out []Outcome
	controlOK := true
	for _, name := range plan {
		var o Outcome
		switch {
		case name == DaemonHello:
			o = daemonHello(c.DaemonSocket, c.Timeout)
		case name == DaemonNoHello:
			o = daemonNoHello(c.DaemonSocket, c.Timeout)
		case !controlOK:
			o = Outcome{Outcome: Skipped, Verdict: Unproven, Detail: "the positive control failed"}
		case name == Control:
			o = control(c.LinkSocket, c.Timeout)
			controlOK = o.Verdict == Held
		case name == Oversize:
			o = oversize(c.LinkSocket, c.Timeout)
			// Dropped before the host: no wait.
		default:
			i := slices.IndexFunc(linkMessages, func(m linkMessage) bool { return m.name == name })
			time.Sleep(c.Interval)
			o = request(c.LinkSocket, linkMessages[i].msg, c.Timeout)
		}
		o.Message = name
		out = append(out, o)
	}
	return out, nil
}

// GateError is the gate's refusal.
type GateError struct{ Err error }

func (e *GateError) Error() string { return "the gate refused: " + e.Err.Error() }

// exchange writes line on a new connection to socket and reads one line
// back. connected says whether a connection was made.
func exchange(socket string, line []byte, timeout time.Duration) (answer []byte, connected bool, err error) {
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(line); err != nil {
		// The peer may close as soon as it has read its limit; what it
		// sent before that, if anything, is still the answer.
		b, rerr := readLine(conn)
		if rerr == nil {
			return b, true, nil
		}
		return nil, true, errWrite{err}
	}
	b, err := readLine(conn)
	return b, true, err
}

type errWrite struct{ error }

// readLine reads up to the first newline, bounded: io.EOF when the peer
// closed before sending one.
func readLine(r io.Reader) ([]byte, error) {
	var b []byte
	buf := make([]byte, 4096)
	for len(b) <= agentproto.MaxPayload {
		n, err := r.Read(buf)
		b = append(b, buf[:n]...)
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			return b[:i], nil
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("an answer over the limit")
}

// classify turns a link answer into an outcome.
func classify(answer []byte) Outcome {
	var m agentproto.Message
	if err := json.Unmarshal(answer, &m); err != nil {
		return Outcome{Outcome: Failed, Detail: "an answer that is not JSON: " + clip(string(answer))}
	}
	switch {
	case m.OK:
		return Outcome{Outcome: Accepted, Detail: clip(m.Error)}
	case m.Error == "":
		return Outcome{Outcome: Failed, Detail: "a refusal that says nothing"}
	case isNoHost(m.Error):
		return Outcome{Outcome: NoHost, Detail: clip(m.Error)}
	case strings.HasPrefix(m.Error, rateLimited):
		return Outcome{Outcome: RateLimited, Detail: clip(m.Error)}
	default:
		return Outcome{Outcome: Refused, Detail: clip(m.Error)}
	}
}

func isNoHost(e string) bool {
	for _, p := range noHost {
		if strings.HasPrefix(e, p) {
			return true
		}
	}
	return false
}

// control is the positive control: the link's own status, which the
// sandbox's end answers, ok, without the host.
func control(socket string, timeout time.Duration) Outcome {
	o := linkExchange(socket, agentproto.Message{Op: Control}, timeout)
	o.Verdict = verdict(o.Outcome, Accepted, "")
	return o
}

// request sends m, which the host must refuse.
func request(socket string, m agentproto.Message, timeout time.Duration) Outcome {
	o := linkExchange(socket, m, timeout)
	o.Verdict = verdict(o.Outcome, Refused, Accepted)
	return o
}

func linkExchange(socket string, m agentproto.Message, timeout time.Duration) Outcome {
	b, err := json.Marshal(m)
	if err != nil {
		return Outcome{Outcome: Failed, Detail: err.Error()}
	}
	answer, connected, err := exchange(socket, append(b, '\n'), timeout)
	switch {
	case !connected:
		return Outcome{Outcome: Unreachable, Detail: clip(err.Error())}
	case errors.Is(err, io.EOF):
		return Outcome{Outcome: Closed}
	case err != nil:
		return Outcome{Outcome: Failed, Detail: clip(err.Error())}
	}
	return classify(answer)
}

// oversize sends a request line one byte over the link's limit: the link
// must drop it, unanswered, or refuse it.
func oversize(socket string, timeout time.Duration) Outcome {
	line := []byte(`{"op":"answer","text":"`)
	line = append(line, bytes.Repeat([]byte{'a'}, agentproto.MaxPayload+1-len(line)-3)...)
	line = append(line, `"}`+"\n"...)
	answer, connected, err := exchange(socket, line, timeout)
	var o Outcome
	var w errWrite
	switch {
	case !connected:
		o = Outcome{Outcome: Unreachable, Detail: clip(err.Error())}
	case errors.Is(err, io.EOF), isReset(err), errors.As(err, &w) && isReset(w.error):
		// Dropped unread: a peer that closes with the line still
		// unread resets the connection.
		o = Outcome{Outcome: Closed}
	case err != nil:
		o = Outcome{Outcome: Failed, Detail: clip(err.Error())}
	default:
		o = classify(answer)
	}
	switch o.Outcome {
	case Closed, Refused:
		o.Verdict = Held
	case Accepted:
		o.Verdict = NotHeld
	default:
		o.Verdict = Unproven
	}
	return o
}

// verdict is Held for the outcome held, NotHeld for notHeld, and
// Unproven for anything else.
func verdict(outcome, held, notHeld string) string {
	switch outcome {
	case held:
		return Held
	case notHeld:
		return NotHeld
	default:
		return Unproven
	}
}

func isReset(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// daemonHello tries to open a daemon conversation on socket. It holds only
// when no connection can be made to a socket that is there: a daemon
// socket in the sandbox is the finding, whatever it then answers, and a
// path that is no socket (gone, mistyped, a plain file) proves nothing.
func daemonHello(socket string, timeout time.Duration) Outcome {
	nc, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return unreachable(socket, err)
	}
	// Connected: not held, whatever follows, unless the answer is in
	// another protocol; the outcome says what it was.
	defer nc.Close()
	return daemonAnswer(nc, daemonproto.Hello(daemonproto.ClientCLI, "conformance-probe"), timeout)
}

// daemonAnswer writes f on nc and reads what comes back: NotHeld, but
// for an answer in another protocol (Foreign, Held).
func daemonAnswer(nc net.Conn, f *daemonproto.Frame, timeout time.Duration) Outcome {
	o := Outcome{Verdict: NotHeld}
	_ = nc.SetWriteDeadline(time.Now().Add(timeout))
	if err := daemonproto.Write(nc, f); err != nil {
		o.Outcome, o.Detail = Closed, "connected; "+clip(err.Error())
		return o
	}
	_ = nc.SetReadDeadline(time.Now().Add(timeout))
	br := bufio.NewReaderSize(nc, 4096)
	if first, err := br.Peek(1); err == nil && first[0] != '{' {
		head, _ := br.Peek(min(br.Buffered(), 40))
		o.Outcome, o.Verdict = Foreign, Held
		o.Detail = "connected; answered in another protocol: " + clip(string(head))
		return o
	}
	fr, err := daemonproto.NewReader(br).Read()
	switch {
	case err != nil:
		o.Outcome, o.Detail = Closed, "connected; "+clip(err.Error())
	case fr.Type == daemonproto.TypeResponse && fr.Error != nil:
		o.Outcome, o.Detail = Refused, "connected; "+clip(fr.Error.Error())
	case fr.Type == daemonproto.TypeHello:
		o.Outcome, o.Detail = Accepted, "connected; a daemon hello"
	default:
		o.Outcome, o.Detail = Failed, "connected; an unexpected "+clip(fr.Type)
	}
	return o
}

// daemonNoHello opens a connection to socket and sends a request before
// any hello. It is the host's witness's message: a connection that
// reaches the daemon is refused there and logged. What it gets back here
// holds unless it is in the daemon's protocol (a refusal, any frame),
// which only the daemon or a relay to it speaks; a silence or a close
// is the host's log to judge.
func daemonNoHello(socket string, timeout time.Duration) Outcome {
	nc, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return unreachable(socket, err)
	}
	defer nc.Close()
	f, err := daemonproto.Request(1, "status", nil)
	if err != nil {
		return Outcome{Outcome: Failed, Verdict: Unproven, Detail: clip(err.Error())}
	}
	o := daemonAnswer(nc, f, timeout)
	if o.Outcome == Closed {
		o.Verdict = Held
	}
	return o
}

// unreachable is the outcome of a dial that failed: Held only when the
// path is a socket, seen after the dial and without following a link, and
// the dial was refused for want of a listener or for its permissions.
// ENOENT, ENOTSOCK, a refusal on a path that is not (or no longer) a
// socket, a timeout and anything else are Unproven.
func unreachable(socket string, err error) Outcome {
	o := Outcome{Outcome: Unreachable, Verdict: Unproven, Detail: clip(err.Error())}
	refused := errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
	fi, lerr := os.Lstat(socket)
	switch {
	case lerr != nil:
		o.Detail = "no socket there: " + clip(err.Error())
	case fi.Mode()&os.ModeSocket == 0:
		o.Detail = "not a socket: " + clip(err.Error())
	case refused:
		o.Verdict = Held
	}
	return o
}

// clip makes s safe to print: printable ASCII, at most 200 bytes.
func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// Exit is the status a run's outcomes give: any NotHeld is ExitNotHeld;
// else any not Held (or none at all) is ExitError.
func Exit(out []Outcome) int {
	code := ExitHeld
	if len(out) == 0 {
		code = ExitError
	}
	for _, o := range out {
		switch o.Verdict {
		case Held:
		case NotHeld:
			return ExitNotHeld
		default:
			code = ExitError
		}
	}
	return code
}

const usage = `usage: conformance-probe --nonce HEX --link SOCKET [--daemon SOCKET] [--only NAME,...] [--timeout SECONDS] [--interval MS]
       conformance-probe --nonce HEX --mount OP --target MOUNTPOINT
       conformance-probe --nonce HEX --mount userns-remount --target RO --part PART --witness CANARY

The conformance suite's probe. Runs only in the suite's own sandbox: /work
must hold exactly its two roots, each with its nonce. Prints one JSON line
per message; exits 0 when every control held, 1 when one did not, 2 for
usage, 3 when the gate refused, 4 when nothing could be proven. Requests
that reach the host are --interval apart (2500 ms by default), under its
rate limit.

With --mount, makes the one call OP (remount-bind-rw, remount-rw,
remount-bind-ro, umount, umount-detach) on MOUNTPOINT, a root of the
suite's or its writable part, and exits 0 when it succeeded, 1 with the
error's strerror text on stderr when it was refused.

With --mount userns-remount, enters a new user and mount namespace and
from inside it bind-remounts RO read-write, unmounts PART and writes to
CANARY (a read-only canary under RO); exits 1 when the namespace, every
call and the write were refused, 0 when any of them succeeded.
`

// maxTimeout bounds --timeout: above the link's own wait for the host.
const maxTimeout = 600

// maxInterval bounds --interval, in milliseconds.
const maxInterval = 10000

// Main is the command line, with work the directory the gate lists.
func Main(work string, args []string, stdout, stderr io.Writer) int {
	c := Config{Work: work, Timeout: 150 * time.Second, Interval: 2500 * time.Millisecond}
	if len(args) > 0 && args[0] == userNSChildArg {
		// The copy userNS starts inside the new namespace.
		return UserNSChild(work, args[1:], stdout, stderr)
	}
	var mountOp, mountTarget, mountPart, mountWitness string
	for len(args) > 0 {
		if len(args) < 2 {
			fmt.Fprint(stderr, usage)
			return ExitUsage
		}
		v := args[1]
		switch args[0] {
		case "--nonce":
			c.Nonce = v
		case "--link":
			c.LinkSocket = v
		case "--daemon":
			c.DaemonSocket = v
		case "--only":
			c.Only = strings.Split(v, ",")
		case "--timeout":
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 || n > maxTimeout || fmt.Sprint(n) != v {
				fmt.Fprintf(stderr, "conformance-probe: --timeout %q is not a number of seconds up to %d\n", clip(v), maxTimeout)
				return ExitUsage
			}
			c.Timeout = time.Duration(n) * time.Second
		case "--mount":
			mountOp = v
		case "--target":
			mountTarget = v
		case "--part":
			mountPart = v
		case "--witness":
			mountWitness = v
		case "--interval":
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 || n > maxInterval || fmt.Sprint(n) != v {
				fmt.Fprintf(stderr, "conformance-probe: --interval %q is not a number of milliseconds up to %d\n", clip(v), maxInterval)
				return ExitUsage
			}
			c.Interval = time.Duration(n) * time.Millisecond
		default:
			fmt.Fprint(stderr, usage)
			return ExitUsage
		}
		args = args[2:]
	}
	if mountOp != "" || mountTarget != "" || mountPart != "" || mountWitness != "" {
		if c.Nonce == "" || mountOp == "" || mountTarget == "" || c.LinkSocket != "" || c.DaemonSocket != "" || c.Only != nil {
			fmt.Fprint(stderr, usage)
			return ExitUsage
		}
		if mountOp == MountUserNS {
			if mountPart == "" || mountWitness == "" {
				fmt.Fprint(stderr, usage)
				return ExitUsage
			}
			return usernsMain(work, c.Nonce, mountTarget, mountPart, mountWitness, stderr)
		}
		if mountPart != "" || mountWitness != "" {
			fmt.Fprint(stderr, usage)
			return ExitUsage
		}
		return mountMain(work, c.Nonce, mountOp, mountTarget, stderr)
	}
	if c.Nonce == "" || c.LinkSocket == "" {
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}
	out, err := Run(c)
	var g *GateError
	switch {
	case errors.As(err, &g):
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(err.Error()))
		return ExitGate
	case err != nil:
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(err.Error()))
		return ExitUsage
	}
	enc := json.NewEncoder(stdout)
	for _, o := range out {
		_ = enc.Encode(o)
	}
	return Exit(out)
}

// mountMain is the mount mode's command line.
func mountMain(work, nonce, op, target string, stderr io.Writer) int {
	err := Mount(work, nonce, op, target)
	var g *GateError
	var u *usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &g):
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(err.Error()))
		return ExitGate
	case errors.As(err, &u):
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(err.Error()))
		return ExitUsage
	}
	fmt.Fprintf(stderr, "conformance-probe: %s %s: %s\n", clip(op), clip(target), ErrnoText(err))
	return ExitRefused
}

// usernsMain is the user-namespace mode's command line: it prints a line
// per step, then a verdict line, and exits ExitRefused when the
// protection held, 0 when something succeeded (a call or the witness
// write), so a breach is the zero exit as it is for a single mount call.
func usernsMain(work, nonce, target, part, witness string, stderr io.Writer) int {
	held, lines, err := UserNS(work, nonce, target, part, witness)
	var g *GateError
	var u *usageError
	switch {
	case errors.As(err, &g):
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(err.Error()))
		return ExitGate
	case errors.As(err, &u):
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(err.Error()))
		return ExitUsage
	case err != nil:
		for _, l := range lines {
			fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(l))
		}
		fmt.Fprintf(stderr, "conformance-probe: userns: %s\n", ErrnoText(err))
		return ExitError
	}
	for _, l := range lines {
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(l))
	}
	if held {
		fmt.Fprintln(stderr, "conformance-probe: userns: held")
		return ExitRefused
	}
	fmt.Fprintln(stderr, "conformance-probe: userns: a call or the write succeeded")
	return 0
}
