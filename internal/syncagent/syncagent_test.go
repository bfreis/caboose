package syncagent

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/statesync"
)

// The test binary stands in for caboose-agent: run with serveEnv set, it
// is `caboose-agent sync OP` on the home and repo the environment names;
// run as `askpass PROMPT` (as the sync's askpass script runs it), it is
// caboose-agent askpass.
const (
	serveEnv = "SYNCAGENT_TEST_SERVE"
	homeEnv  = "SYNCAGENT_TEST_HOME"
	repoEnv  = "SYNCAGENT_TEST_REPO"
	// markersEnv sets maxMarkers, for a conflict too large to edit.
	markersEnv = "SYNCAGENT_TEST_MAX_MARKERS"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "askpass" {
		os.Exit(Askpass(os.Args[2:], os.Stdout, os.Stderr))
	}
	if n, err := strconv.Atoi(os.Getenv(markersEnv)); err == nil {
		maxMarkers = n
	}
	if op := os.Getenv(serveEnv); op != "" {
		exe, _ := os.Executable()
		os.Exit(Serve(op, os.Stdin, os.Stdout, os.Stderr, Paths{Home: os.Getenv(homeEnv), Repo: os.Getenv(repoEnv), Exe: exe}))
	}
	os.Exit(m.Run())
}

func needGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "maintenance.auto")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	return git
}

// machine is a sandbox: a home and a sync repo, whose syncs run in a
// child process speaking the protocol, as caboose-agent does.
type machine struct {
	t          *testing.T
	home, repo string
	path       string // PATH for the agent: git alone (no gh)
}

func newMachine(t *testing.T, git string) *machine {
	t.Helper()
	d := t.TempDir()
	bin := filepath.Join(d, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	return &machine{t: t, home: filepath.Join(d, "home"), repo: filepath.Join(d, "sync"), path: bin}
}

func (m *machine) write(rel, data string) {
	m.t.Helper()
	p := filepath.Join(m.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(m.home, rel))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

var roots = []string{"/work"}

func (m *machine) request() Request {
	return Request{
		Host: filepath.Base(filepath.Dir(m.home)), Sandbox: sandboxcfg.Default(roots), Roots: roots,
		Mounted: []string{".claude", ".claude.json", ".config/caboose"},
	}
}

func (m *machine) cmd(op string, env ...string) *exec.Cmd {
	c := exec.Command(os.Args[0])
	c.Env = append(append(os.Environ(), serveEnv+"="+op, homeEnv+"="+m.home, repoEnv+"="+m.repo, "PATH="+m.path,
		"GIT_SSH_COMMAND=false"), env...)
	return c
}

func (m *machine) run(op string, req Request, h Handler) *Result {
	m.t.Helper()
	res, err := Run(m.cmd(op), req, h, nil)
	if err != nil {
		m.t.Fatalf("%s: %v", op, err)
	}
	return res
}

func newRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", statesync.Branch, dir).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return dir
}

func mem(file string) string {
	return ".claude/projects/" + statesync.ProjectKey("/work/p") + "/memory/" + file
}

// A sync run through the protocol: the repo set up and the remote set as
// asked, a conflict sent to the host with its markers and settled by its
// answer, and the result reported.
func TestServeSyncsAndRelaysAConflict(t *testing.T) {
	git := needGit(t)
	remote := newRemote(t)
	a, b := newMachine(t, git), newMachine(t, git)
	for _, m := range []*machine{a, b} {
		m.write(mem("f.md"), "base\n")
		req := m.request()
		req.Remote = remote
		if res := m.run(OpRun, req, Handler{}); res.Err != nil || res.Report == nil {
			t.Fatalf("first sync: %+v %+v", res.Err, res.Report)
		}
	}
	a.write(mem("f.md"), "alpha\n")
	if res := a.run(OpRun, a.request(), Handler{}); res.Err != nil || !res.Report.Pushed {
		t.Fatalf("alpha's sync: %+v %+v", res.Err, res.Report)
	}
	b.write(mem("f.md"), "beta\n")

	// Nobody to ask: the conflict aborts, and nothing changes.
	res := b.run(OpRun, b.request(), Handler{})
	if res.Err == nil || res.Err.Kind != KindAborted {
		t.Fatalf("with nobody to ask: %+v", res.Err)
	}
	if got := b.read(mem("f.md")); got != "beta\n" {
		t.Errorf("after the abort beta has %q", got)
	}

	// Asked, and told to abort: the same.
	req := b.request()
	req.Interactive = true
	res = b.run(OpRun, req, Handler{Conflict: func(*Conflict) Reply { return Reply{Abort: true} }})
	if res.Err == nil || res.Err.Kind != KindAborted || b.read(mem("f.md")) != "beta\n" {
		t.Fatalf("aborted by the host: %+v, beta has %q", res.Err, b.read(mem("f.md")))
	}

	var seen *Conflict
	res = b.run(OpRun, req, Handler{Conflict: func(c *Conflict) Reply {
		seen = c
		return Reply{Resolution: &statesync.Resolution{Content: []byte("both\n")}}
	}})
	if res.Err != nil || res.Report == nil || !res.Report.Merged || !res.Report.Pushed {
		t.Fatalf("beta's sync: %+v %+v", res.Err, res.Report)
	}
	if seen == nil || seen.Path != "home/"+mem("f.md") || seen.OursDeleted || seen.TheirsDeleted ||
		!statesync.HasMarkers(seen.Markers) || !strings.Contains(string(seen.Markers), "beta\n") {
		t.Errorf("the conflict sent: %+v", seen)
	}
	if got := b.read(mem("f.md")); got != "both\n" {
		t.Errorf("beta has %q", got)
	}
	if res := a.run(OpRun, a.request(), Handler{}); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := a.read(mem("f.md")); got != "both\n" {
		t.Errorf("alpha has %q", got)
	}

	// Status: nothing to send, nothing to take.
	b.write(mem("g.md"), "new\n")
	res = b.run(OpStatus, b.request(), Handler{})
	if res.Err != nil || res.Status == nil {
		t.Fatalf("status: %+v", res.Err)
	}
	st := res.Status
	if !st.Remote || st.FetchErr != "" || !slices.Equal(st.Changed, []string{"home/" + mem("g.md")}) || len(st.Taken) != 0 || !st.Divergence.Remote {
		t.Errorf("status: %+v", st)
	}
}

// basicAuth is an HTTP remote that refuses everyone, recording the
// credentials each request came with.
func basicAuth(t *testing.T) (url string, seen func() []string) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
			mu.Lock()
			got = append(got, string(raw))
			mu.Unlock()
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="t"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/state.git", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// git's prompts reach the host through askpass, and its answers reach git;
// a sync nobody watches refuses them without asking.
func TestServeRelaysPrompts(t *testing.T) {
	git := needGit(t)
	url, seen := basicAuth(t)
	m := newMachine(t, git)
	req := m.request()
	req.Remote = url
	req.Interactive = true
	var asks []Ask
	res := m.run(OpRun, req, Handler{Ask: func(a *Ask) Reply {
		asks = append(asks, *a)
		answer := "pw"
		if strings.HasPrefix(a.Prompt, "Username") {
			answer = "me"
		}
		return Reply{Answer: &answer}
	}})
	if res.Err == nil || res.Err.Kind != KindSync {
		t.Errorf("against a remote that refuses: %+v", res.Err)
	}
	if len(asks) != 2 || !strings.HasPrefix(asks[0].Prompt, "Username for '") ||
		!strings.HasPrefix(asks[1].Prompt, "Password for '") {
		t.Errorf("asked %+v", asks)
	}
	if got := seen(); !slices.Contains(got, "me:pw") {
		t.Errorf("the remote saw %q", got)
	}

	// Auto: refused in the sandbox, never asked.
	url, seen = basicAuth(t)
	req = m.request()
	req.Remote, req.Auto = url, true
	res = m.run(OpRun, req, Handler{Ask: func(a *Ask) Reply {
		t.Errorf("asked %q in an auto sync", a.Prompt)
		return Reply{Abort: true}
	}})
	if res.Err == nil {
		t.Error("an auto sync went through a remote that wants credentials")
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("the remote saw %q", got)
	}
}

// Askpass with nobody to ask fails, so that git or ssh do.
func TestAskpassRefusedFails(t *testing.T) {
	t.Setenv(AskpassEnv, "")
	if code := Askpass([]string{"Password: "}, io.Discard, io.Discard); code != 1 {
		t.Errorf("no sync: exit %d", code)
	}
	s := &statesync.Syncer{}
	stop, err := serveAskpass(s, Paths{Exe: os.Args[0]}, func(Ask) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, e := range s.Env {
		if k, v, _ := strings.Cut(e, "="); k == AskpassEnv {
			t.Setenv(AskpassEnv, v)
		}
	}
	var out bytes.Buffer
	if code := Askpass([]string{"Password: "}, &out, io.Discard); code != 1 || out.Len() != 0 {
		t.Errorf("refused: exit %d, printed %q", code, out.String())
	}
}

// An agent from an older image does not know the sync at all; one of
// another protocol version says so in its hello. Either is told apart
// from a sync that failed, so the host can say to restart.
func TestRunTellsAnOldAgent(t *testing.T) {
	for name, script := range map[string]string{
		"unknown command": `echo "usage: caboose-agent COMMAND" >&2; exit 2`,
		"other version":   `echo '{"type":"hello","version":99}'; head -n 1 >/dev/null`,
	} {
		_, err := Run(exec.Command("sh", "-c", script), Request{Mounted: []string{}}, Handler{}, nil)
		if !errors.Is(err, ErrOldAgent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := Run(exec.Command("sh", "-c", `echo boom >&2; exit 1`), Request{}, Handler{}, nil)
	if err == nil || errors.Is(err, ErrOldAgent) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a failure: %v", err)
	}
	_, err = Run(exec.Command("sh", "-c", `echo '{"type":"hello","version":1}'; head -n 1 >/dev/null`), Request{}, Handler{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no result") {
		t.Errorf("no result: %v", err)
	}
}

// The protocol's lines are bounded both ways.
func TestReaderBoundsLines(t *testing.T) {
	long := `{"type":"` + strings.Repeat("x", 200) + `"}` + "\n"
	r := NewReader(strings.NewReader(long), 100)
	var m Message
	if err := r.Read(&m); !errors.Is(err, ErrTooLong) {
		t.Errorf("a long line: %v", err)
	}
	r = NewReader(strings.NewReader(`{"type":"hello","version":1}`+"\n"+`{"type":`), 100)
	if err := r.Read(&m); err != nil || m.Type != TypeHello || m.Version != 1 {
		t.Errorf("%+v, %v", m, err)
	}
	if err := r.Read(&m); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a cut line: %v", err)
	}
	if err := r.Read(&m); !errors.Is(err, io.EOF) {
		t.Errorf("the end: %v", err)
	}
	var buf bytes.Buffer
	in := Reply{Resolution: &statesync.Resolution{Content: []byte("a\nb\x00")}}
	if err := Write(&buf, in); err != nil || bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("%q, %v", buf.String(), err)
	}
	var out Reply
	if err := NewReader(&buf, MaxLine).Read(&out); err != nil || string(out.Resolution.Content) != "a\nb\x00" {
		t.Errorf("%+v, %v", out, err)
	}
}

// The sync's git never reads the sandbox's global git config, keeps the
// remote's host key in the sync repo, and, with nobody watching, never
// prompts; an ssh command the sandbox's environment sets is left alone.
func TestSyncerGit(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	p := Paths{Home: "/home/agent", Repo: "/home/agent/.caboose-sync"}
	req := &Request{Sandbox: sandboxcfg.Default(roots), Mounted: []string{}}
	ssh := "GIT_SSH_COMMAND=ssh -o 'UserKnownHostsFile=/home/agent/.caboose-sync/.git/known_hosts ~/.ssh/known_hosts'"
	for _, tc := range []struct {
		auto, interactive bool
		want, not         []string
	}{
		{false, true, []string{"GIT_CONFIG_GLOBAL=/dev/null", "HOME=/home/agent", ssh}, []string{"GIT_TERMINAL_PROMPT=0", ssh + " -o BatchMode=yes"}},
		{true, false, []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", ssh + " -o BatchMode=yes"}, []string{ssh}},
	} {
		req.Auto, req.Interactive = tc.auto, tc.interactive
		s, err := newSyncer(req, p)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tc.want {
			if !slices.Contains(s.Env, w) {
				t.Errorf("auto=%v: no %q in %q", tc.auto, w, s.Env)
			}
		}
		for _, n := range tc.not {
			if slices.Contains(s.Env, n) {
				t.Errorf("auto=%v: %q in %q", tc.auto, n, s.Env)
			}
		}
		if len(s.GitArgs) < 2 || s.GitArgs[1] != "safe.directory=/home/agent/.caboose-sync" {
			t.Errorf("git args %q", s.GitArgs)
		}
	}
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /k")
	s, err := newSyncer(req, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			t.Errorf("the sandbox's own ssh command overridden: %q", e)
		}
	}
}

// A conflict whose markers are too large to send is still offered, with
// the sides to keep, and an edit said to be unavailable.
func TestServeOffersALargeConflictWithoutMarkers(t *testing.T) {
	git := needGit(t)
	remote := newRemote(t)
	a, b := newMachine(t, git), newMachine(t, git)
	for _, m := range []*machine{a, b} {
		m.write(mem("f.md"), "base\n")
		req := m.request()
		req.Remote = remote
		if res := m.run(OpRun, req, Handler{}); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	a.write(mem("f.md"), "alpha\n")
	if res := a.run(OpRun, a.request(), Handler{}); res.Err != nil {
		t.Fatal(res.Err)
	}
	b.write(mem("f.md"), "beta\n")
	req := b.request()
	req.Interactive = true
	var seen *Conflict
	res, err := Run(b.cmd(OpRun, markersEnv+"=10"), req, Handler{Conflict: func(c *Conflict) Reply {
		seen = c
		return Reply{Take: TakeTheirs}
	}}, nil)
	if err != nil || res.Err != nil {
		t.Fatalf("%v %+v", err, res.Err)
	}
	if seen == nil || seen.Markers != nil || !strings.Contains(seen.MarkersErr, "too large to edit") {
		t.Errorf("the conflict sent: %+v", seen)
	}
	if got := b.read(mem("f.md")); got != "alpha\n" {
		t.Errorf("taking the remote's left %q", got)
	}
}

// A sync whose host goes away (here: takes longer than the host allows)
// stops in the sandbox: the command waiting on the remote is killed, with
// what it started, and the sandbox's lock is let go.
func TestServeStopsWhenTheHostGoes(t *testing.T) {
	git := needGit(t)
	m := newMachine(t, git)
	pidfile := filepath.Join(t.TempDir(), "pid")
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	req := m.request()
	req.Remote = "ssh://git.example.invalid/state.git"
	start := time.Now()
	res, err := Run(m.cmd(OpRun, "GIT_SSH_COMMAND=echo $$ > '"+pidfile+"'; exec "+sleep+" 30;"), req,
		Handler{Timeout: time.Second}, nil)
	if !errors.Is(err, ErrTimedOut) {
		t.Errorf("err = %v, %+v", err, res)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	data, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil || strings.Contains(string(stat), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the remote's ssh (pid %d) still runs", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Its lock is free once it has stopped.
	deadline = time.Now().Add(5 * time.Second)
	for {
		unlock, err := lockRepo(m.repo)
		if err == nil {
			unlock()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox's lock is still held: %v", err)
		}
	}
}

// One sync at a time in the sandbox, whatever the host's lock says: a
// second one waits a moment, then says another is running.
func TestServeTakesTheSandboxsLock(t *testing.T) {
	git := needGit(t)
	m := newMachine(t, git)
	wait := lockWait
	lockWait = 200 * time.Millisecond
	defer func() { lockWait = wait }()
	unlock, err := lockRepo(m.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	res := m.run(OpRun, m.request(), Handler{})
	if res.Err == nil || res.Err.Kind != KindBusy {
		t.Errorf("with the lock held: %+v", res.Err)
	}
}

// Nothing in the sandbox holds a sync up by connecting to its askpass
// and saying nothing.
func TestAskpassStopsWithASilentClient(t *testing.T) {
	s := &statesync.Syncer{}
	stop, err := serveAskpass(s, Paths{Exe: os.Args[0]}, func(Ask) (string, bool) { return "x", true })
	if err != nil {
		t.Fatal(err)
	}
	var sock string
	for _, e := range s.Env {
		if k, v, _ := strings.Cut(e, "="); k == AskpassEnv {
			sock = v
		}
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	stop()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stop took %v", d)
	}
}

// The host's end: an old agent's usage text is not shown; a reply too
// long to send fails the run rather than leave both ends waiting; and no
// more than MaxAsks prompts reach the person.
func TestRunEdges(t *testing.T) {
	var shown bytes.Buffer
	_, err := Run(exec.Command("sh", "-c", `echo "usage: caboose-agent COMMAND" >&2; exit 2`), Request{}, Handler{}, &shown)
	if !errors.Is(err, ErrOldAgent) || shown.Len() > 0 {
		t.Errorf("old agent: %v, showed %q", err, shown.String())
	}

	hello := `echo '{"type":"hello","version":1}'; read req; `
	big := make([]byte, MaxLine)
	done := make(chan error, 1)
	go func() {
		_, err := Run(exec.Command("sh", "-c", hello+`echo '{"type":"conflict","conflict":{"path":"x"}}'; cat >/dev/null`), Request{},
			Handler{Conflict: func(*Conflict) Reply { return Reply{Resolution: &statesync.Resolution{Content: big}} }}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "answering the sync") {
			t.Errorf("a reply too long: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a reply too long hung the run")
	}

	asked := 0
	script := hello + `i=0; while [ $i -lt 12 ]; do echo '{"type":"ask","ask":{"prompt":"Password: "}}'; read r; i=$((i+1)); done; echo '{"type":"error","error":{"kind":"sync","message":"x"}}'`
	if _, err := Run(exec.Command("sh", "-c", script), Request{}, Handler{Ask: func(*Ask) Reply {
		asked++
		a := "pw"
		return Reply{Answer: &a}
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if asked != MaxAsks {
		t.Errorf("asked %d times", asked)
	}
}
