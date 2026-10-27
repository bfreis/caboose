package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/bfreis/caboose/internal/agentproto"
)

// `caboose-agent request` files a request on the host ahead of time, and
// `caboose-agent requests` lists the requests and waits for their
// decisions. Both are what caboose's mod runs; a script may run them too.
// The host decides: nothing here changes a decision.

// maxShown bounds each host text printed.
const (
	maxShown    = 300
	askSlack    = 30 * time.Second
	waitCmdPad  = 15 * time.Second
	sessionVar  = "CLAUDE_CODE_SESSION_ID"
	sinceNow    = "now"
	requestKind = "alias, egress, open_url or port"
)

// sanitize is s without anything a terminal would act on, and without
// line breaks, cut to n characters: what the host says is printed through
// it.
func sanitize(s string, n int) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if r == '\n' || r == '\t' {
			r = ' '
		}
		if !unicode.IsPrint(r) {
			continue
		}
		if count == n {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// askCtx sends m to the link on socket and returns its answer, giving up
// after timeout or when ctx ends. Unlike ask, a refusal is not an error:
// the answer says it (OK, Error, Reason, Verdict, RequestID).
func askCtx(ctx context.Context, socket string, m agentproto.Message, timeout time.Duration) (agentproto.Message, error) {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := d.DialContext(dctx, "unix", socket)
	if err != nil {
		return agentproto.Message{}, ErrNoLink
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-stop:
		}
	}()
	b, err := json.Marshal(m)
	if err != nil {
		return agentproto.Message{}, err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return agentproto.Message{}, err
	}
	var r agentproto.Message
	line, err := readLine(c, agentproto.MaxPayload)
	if err != nil {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		return r, fmt.Errorf("the link gave no answer: %w", err)
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return r, err
	}
	return r, nil
}

// flagSet is a command line of --name value and --name=value flags and
// positional words.
type flagSet struct {
	vals map[string][]string
	pos  []string
}

// parseFlags reads args. flags take a value, bools do not.
func parseFlags(args []string, flags, bools []string) (*flagSet, error) {
	fs := &flagSet{vals: map[string][]string{}}
	has := func(list []string, n string) bool {
		for _, f := range list {
			if f == n {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			fs.pos = append(fs.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			fs.pos = append(fs.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a[2:], "=")
		switch {
		case has(bools, name):
			if hasVal {
				return nil, fmt.Errorf("--%s takes no value", name)
			}
			fs.vals[name] = append(fs.vals[name], "")
		case has(flags, name):
			if !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			fs.vals[name] = append(fs.vals[name], val)
		default:
			return nil, fmt.Errorf("unknown flag %s", sanitize(a, 40))
		}
	}
	return fs, nil
}

func (f *flagSet) set(n string) bool { return len(f.vals[n]) > 0 }

// one is the flag's only value, and an error when it was given twice.
func (f *flagSet) one(n string) (string, error) {
	v := f.vals[n]
	if len(v) > 1 {
		return "", fmt.Errorf("--%s is given twice", n)
	}
	if len(v) == 0 {
		return "", nil
	}
	return v[0], nil
}

// withSignals runs f, and cancels the context it gets when interrupted or
// terminated, returning 130 or 143 then.
func withSignals(f func(ctx context.Context) int) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- f(ctx) }()
	select {
	case code := <-done:
		return code
	case sig := <-sigs:
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

// requestResult is `request --json`'s output.
type requestResult struct {
	Verdict string `json:"verdict,omitempty"`
	Request uint64 `json:"request,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// requestCommand is `caboose-agent request KIND [flags]`: 0 when the host
// allows it, 75 when it waits for a person (pending #N), 77 when denied,
// 1 when it could not be asked, 2 for a usage error.
func requestCommand(args []string, stdout, stderr io.Writer, usage func() int) int {
	return requestWith(linkSocket, os.Getenv, args, stdout, stderr, usage)
}

func requestWith(socket string, getenv func(string) string, args []string, stdout, stderr io.Writer, usage func() int) int {
	fs, err := parseFlags(args,
		[]string{"alias", "arg", "host", "port", "url", "context", "option", "recommend", "meanwhile", "session"},
		[]string{"json"})
	if err != nil {
		fmt.Fprintf(stderr, "caboose-agent: request: %v\n", err)
		return 2
	}
	if len(fs.pos) != 1 {
		return usage()
	}
	asJSON := fs.set("json")
	fail := func(format string, a ...any) int {
		msg := fmt.Sprintf(format, a...)
		if asJSON {
			b, _ := json.Marshal(requestResult{Error: msg})
			fmt.Fprintf(stdout, "%s\n", b)
		} else {
			fmt.Fprintf(stderr, "caboose-agent: request: %s\n", msg)
		}
		return 2
	}
	ask := agentproto.Ask{Kind: fs.pos[0]}
	switch ask.Kind {
	case "alias", "egress", "open_url", "port":
	default:
		return fail("the kind is %s, not %q", requestKind, sanitize(ask.Kind, 32))
	}
	str := map[string]*string{"alias": &ask.Alias, "host": &ask.Host, "url": &ask.URL, "context": &ask.Context,
		"recommend": &ask.Recommendation, "meanwhile": &ask.Meanwhile, "session": &ask.Session}
	for n, p := range str {
		if *p, err = fs.one(n); err != nil {
			return fail("%v", err)
		}
	}
	if ask.Session == "" {
		ask.Session = getenv(sessionVar)
	}
	if v, err := fs.one("port"); err != nil {
		return fail("%v", err)
	} else if v != "" {
		if ask.Port, err = strconv.Atoi(v); err != nil || !agentproto.ValidPort(ask.Port) {
			return fail("%q is not a port", sanitize(v, 32))
		}
	}
	ask.Options = fs.vals["option"]
	argv := append([]string{ask.Alias}, fs.vals["arg"]...)
	switch ask.Kind {
	case "alias":
		_, a, err := agentproto.ParseAliasArgv(argv)
		if err != nil {
			return fail("%v", err)
		}
		ask.Args = a
	case "egress":
		if ask.Host == "" || ask.Port == 0 {
			return fail("an egress request needs --host and --port")
		}
	case "open_url":
		if ask.URL == "" {
			return fail("an open_url request needs --url")
		}
	case "port":
		if ask.Port == 0 {
			return fail("a port request needs --port")
		}
		if ask.Host != "" || ask.URL != "" || ask.Alias != "" || len(fs.vals["arg"]) > 0 {
			return fail("a port request takes --port alone")
		}
	}
	for _, c := range []struct {
		name   string
		n, max int
	}{
		{"context", len(ask.Context), agentproto.MaxAskContext},
		{"recommendation", len(ask.Recommendation), agentproto.MaxAskRecommendation},
		{"meanwhile", len(ask.Meanwhile), agentproto.MaxAskMeanwhile},
		{"session", len(ask.Session), agentproto.MaxAskSession},
		{"options", len(ask.Options), agentproto.MaxAskOptions},
	} {
		if c.n > c.max {
			return fail("%s: %d is over the limit of %d", c.name, c.n, c.max)
		}
	}
	for _, o := range ask.Options {
		if len(o) > agentproto.MaxAskOption {
			return fail("an option is over the limit of %d bytes", agentproto.MaxAskOption)
		}
	}
	return withSignals(func(ctx context.Context) int {
		r, err := askCtx(ctx, socket, agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &ask}, requestTimeout+askSlack)
		if err != nil {
			if asJSON {
				b, _ := json.Marshal(requestResult{Error: sanitize(err.Error(), maxShown)})
				fmt.Fprintf(stdout, "%s\n", b)
			} else {
				fmt.Fprintf(stderr, "caboose-agent: request: %s\n", sanitize(err.Error(), maxShown))
			}
			return 1
		}
		return report(r, asJSON, stdout, stderr)
	})
}

// report prints the host's answer to a request and gives the exit status.
func report(r agentproto.Message, asJSON bool, stdout, stderr io.Writer) int {
	res := requestResult{Verdict: r.Verdict, Request: r.RequestID, Message: sanitize(r.Error, maxShown)}
	code := 1
	switch {
	case r.OK && r.Verdict == agentproto.VerdictAllowed:
		code = 0
		res.Message = ""
	case r.Verdict == agentproto.VerdictPending:
		code = ExitPending
	case r.Verdict == agentproto.VerdictDenied:
		code = ExitDenied
	default:
		res = requestResult{Error: res.Message}
		if res.Error == "" {
			res.Error = "the host gave no verdict"
		}
	}
	if asJSON {
		b, _ := json.Marshal(res)
		fmt.Fprintf(stdout, "%s\n", b)
		return code
	}
	switch code {
	case 0:
		fmt.Fprintln(stdout, "allowed")
	case ExitPending:
		fmt.Fprintf(stdout, "pending #%d\n", res.Request)
		if res.Message != "" {
			fmt.Fprintf(stderr, "%s\n", res.Message)
		}
	case ExitDenied:
		fmt.Fprintf(stderr, "denied: %s\n", res.Message)
	default:
		fmt.Fprintf(stderr, "caboose-agent: request: %s\n", res.Error)
	}
	return code
}

// requestsResult is `requests --json`'s output.
type requestsResult struct {
	Cursor   uint64                   `json:"cursor"`
	Requests []agentproto.RequestInfo `json:"requests"`
}

// cleanInfo is q with each text the host sent made printable and bounded.
func cleanInfo(q agentproto.RequestInfo) agentproto.RequestInfo {
	q.Kind = sanitize(q.Kind, 32)
	q.Effect = sanitize(q.Effect, maxShown)
	q.State = sanitize(q.State, 32)
	q.Scope = sanitize(q.Scope, 32)
	q.Reason = sanitize(q.Reason, maxShown)
	q.Session = sanitize(q.Session, agentproto.MaxAskSession)
	return q
}

// requestsCommand is `caboose-agent requests [flags]`.
func requestsCommand(args []string, stdout, stderr io.Writer, usage func() int) int {
	return requestsWith(linkSocket, "/proc", os.Getppid(), args, stdout, stderr, usage)
}

func requestsWith(socket, procRoot string, parent int, args []string, stdout, stderr io.Writer, usage func() int) int {
	fs, err := parseFlags(args, []string{"since", "wait", "session"}, []string{"all", "json", "mod"})
	if err != nil {
		fmt.Fprintf(stderr, "caboose-agent: requests: %v\n", err)
		return 2
	}
	if len(fs.pos) != 0 {
		return usage()
	}
	bad := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "caboose-agent: requests: "+format+"\n", a...)
		return 2
	}
	m := agentproto.Message{Op: agentproto.OpBrokerRequests, All: fs.set("all")}
	now := false
	if v, err := fs.one("since"); err != nil {
		return bad("%v", err)
	} else if v == sinceNow {
		now = true
		// Nothing is newer than this: the answer is the cursor alone.
		m.Since = math.MaxUint64
	} else if v != "" {
		if m.Since, err = strconv.ParseUint(v, 10, 64); err != nil {
			return bad("--since is a number or %q, not %q", sinceNow, sanitize(v, 32))
		}
	}
	if v, err := fs.one("wait"); err != nil {
		return bad("%v", err)
	} else if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return bad("--wait is a number of seconds, not %q", sanitize(v, 32))
		}
		m.Wait = min(max(n, 0), agentproto.MaxWait)
	}
	if now {
		m.Wait = 0
	}
	if m.Session, err = fs.one("session"); err != nil {
		return bad("%v", err)
	}
	if len(m.Session) > agentproto.MaxAskSession {
		return bad("--session is over %d bytes", agentproto.MaxAskSession)
	}
	if fs.set("mod") {
		m.ModPID = claudeAbove(procRoot, parent)
	}
	asJSON := fs.set("json")
	return withSignals(func(ctx context.Context) int {
		timeout := time.Duration(m.Wait)*time.Second + waitCmdPad
		r, err := askCtx(ctx, socket, m, timeout)
		if err == nil && !r.OK {
			err = errors.New(r.Error)
			if r.Error == "" {
				err = errors.New("refused")
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "caboose-agent: requests: %s\n", sanitize(err.Error(), maxShown))
			return 1
		}
		res := requestsResult{Cursor: r.Cursor, Requests: []agentproto.RequestInfo{}}
		if !now {
			for i, q := range r.Requests {
				if i == agentproto.MaxRequests {
					break
				}
				res.Requests = append(res.Requests, cleanInfo(q))
			}
		}
		if asJSON {
			b, _ := json.Marshal(res)
			fmt.Fprintf(stdout, "%s\n", b)
			return 0
		}
		writeRequests(stdout, res, now)
		return 0
	})
}

// writeRequests lists requests for a person.
func writeRequests(w io.Writer, res requestsResult, now bool) {
	if now {
		fmt.Fprintf(w, "cursor %d\n", res.Cursor)
		return
	}
	if len(res.Requests) == 0 {
		fmt.Fprintf(w, "no requests (cursor %d)\n", res.Cursor)
		return
	}
	for _, q := range res.Requests {
		fmt.Fprintf(w, "#%d  %s  %s  %s\n", q.ID, q.State, q.Kind, q.Effect)
		if q.Reason != "" {
			fmt.Fprintf(w, "    %s\n", q.Reason)
		}
	}
	fmt.Fprintf(w, "cursor %d\n", res.Cursor)
}
