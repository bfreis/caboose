package launcher

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/datadir"
)

func writeLinkLog(t *testing.T, w *linksWorld, env, content string) string {
	t.Helper()
	dir := w.dataDir(env)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, linkLogFile)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// link_log gives the last lines, printable and cut, from at most the last
// window of the file, and refuses what is not a plain file.
func TestLinkLogTail(t *testing.T) {
	w := newLinksWorld(t)
	w.env("alpha", "running")
	l := w.links(w.daemonDir())

	r, err := l.LinkLog(t.Context(), "alpha", 10)
	if err != nil || len(r.Lines) != 0 || r.Truncated || r.Lines == nil {
		t.Fatalf("no log: %+v, %v", r, err)
	}

	var sb strings.Builder
	for i := range 10 {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	p := writeLinkLog(t, w, "alpha", sb.String())
	r, _ = l.LinkLog(t.Context(), "alpha", 3)
	if !slices.Equal(r.Lines, []string{"line 7", "line 8", "line 9"}) || !r.Truncated {
		t.Errorf("last 3: %+v", r)
	}
	r, _ = l.LinkLog(t.Context(), "alpha", daemonproto.MaxLinkLogLines)
	if len(r.Lines) != 10 || r.Truncated || r.Lines[0] != "line 0" {
		t.Errorf("all: %+v", r)
	}
	// A last line with no newline is a line.
	writeLinkLog(t, w, "alpha", "a\nb")
	if r, _ = l.LinkLog(t.Context(), "alpha", 5); !slices.Equal(r.Lines, []string{"a", "b"}) || r.Truncated {
		t.Errorf("no final newline: %+v", r)
	}

	// Control characters are escaped, a long line is cut at a character.
	writeLinkLog(t, w, "alpha", "red \x1b[31m and \u202e bidi\n"+strings.Repeat("é", 800)+"\n")
	r, _ = l.LinkLog(t.Context(), "alpha", 5)
	if len(r.Lines) != 2 || strings.ContainsAny(r.Lines[0], "\x1b\u202e") || !strings.Contains(r.Lines[0], `\u001B`) {
		t.Errorf("escapes: %q", r.Lines)
	}
	if n := len(r.Lines[1]); n > daemonproto.MaxLinkLogLine || n < daemonproto.MaxLinkLogLine-1 {
		t.Errorf("a long line is %d bytes", n)
	}
	if res := (&daemonproto.LinkLogResult{Lines: r.Lines}); res.Validate() != nil {
		t.Errorf("the lines do not validate: %v", res.Validate())
	}

	// Only the last window is read: 1000 lines of 1000 bytes are a megabyte,
	// of which at most a window comes back, whole lines, the newest last.
	sb.Reset()
	for i := range 1000 {
		fmt.Fprintf(&sb, "%04d %s\n", i, strings.Repeat("x", 990))
	}
	writeLinkLog(t, w, "alpha", sb.String())
	r, _ = l.LinkLog(t.Context(), "alpha", daemonproto.MaxLinkLogLines)
	if !r.Truncated || len(r.Lines) >= daemonproto.MaxLinkLogLines || len(r.Lines)*1000 > linkLogWindow+1000 || len(r.Lines) < 200 {
		t.Fatalf("window: %d lines, truncated %v", len(r.Lines), r.Truncated)
	}
	if !strings.HasPrefix(r.Lines[len(r.Lines)-1], "0999 ") {
		t.Errorf("the last line is %q", r.Lines[len(r.Lines)-1][:10])
	}
	for _, s := range r.Lines {
		if len(s) != 995 {
			t.Fatalf("a partial line of %d bytes: %.12q", len(s), s)
		}
	}
	// A window with no newline at all has no whole line.
	writeLinkLog(t, w, "alpha", strings.Repeat("y", 2*linkLogWindow))
	if r, err = l.LinkLog(t.Context(), "alpha", 5); err != nil || len(r.Lines) != 0 || !r.Truncated {
		t.Errorf("no newline in the window: %+v, %v", r, err)
	}

	// A link log that is a symlink is not read.
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil { // the test's own temp dir
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if r, err = l.LinkLog(t.Context(), "alpha", 5); err == nil {
		t.Errorf("a symlinked log was read: %+v", r)
	}
	if _, err = l.LinkLog(t.Context(), "nosuch", 5); daemonproto.Kind(err) != daemonproto.KindUnknownEnv {
		t.Errorf("an unknown environment: %v", err)
	}
}

func partErrors(r *daemonproto.EnvDetailsResult) []string {
	var parts []string
	for _, e := range r.Errors {
		parts = append(parts, e.Part)
	}
	slices.Sort(parts)
	return parts
}

// env_details answers what it can: sessions through the Backend, the keep
// entries through nofollow, the ports the link forwards; a part that fails
// is an errors entry and the rest still answers.
func TestEnvDetails(t *testing.T) {
	w := newLinksWorld(t)
	b := w.env("alpha", "running")
	var tmux []string
	b.Exec = func(s backend.ExecSpec) *exec.Cmd {
		if len(s.Argv) > 0 && s.Argv[0] == "tmux" {
			return backendtest.Reply(strings.Join(tmux, "\n")+"\n", "", 0)
		}
		return nil
	}
	tmux = []string{"main", "work\x1b[2J"}
	l := w.links(w.daemonDir())

	r, err := l.EnvDetails(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("%v\n%+v", err, r)
	}
	if !strings.HasPrefix(r.Isolation, "container") || r.Env != "alpha" {
		t.Errorf("isolation %q", r.Isolation)
	}
	if !slices.Equal(r.Sessions, []string{"main", `work\u001B[2J`}) {
		t.Errorf("sessions %q", r.Sessions)
	}
	// The start time is the backend's, when it can tell, in UTC.
	if !r.Started.IsZero() {
		t.Errorf("started %v, from a backend that cannot tell", r.Started)
	}
	at := time.Date(2026, 10, 18, 13, 0, 0, 0, time.FixedZone("X", 3600))
	b.Fake.StartedAt = at
	if r2, _ := l.EnvDetails(t.Context(), "alpha"); !r2.Started.Equal(at) || r2.Started.Location() != time.UTC || r2.Validate() != nil {
		t.Errorf("started %v", r2.Started)
	}
	// The world's docker never answers: the image part fails, alone.
	if !slices.Equal(partErrors(r), []string{"image"}) || r.Image != nil {
		t.Errorf("errors %+v, image %+v", r.Errors, r.Image)
	}
	var paths []string
	for _, k := range r.Keep {
		paths = append(paths, k.Path)
	}
	if !slices.Contains(paths, "~/.claude") || !slices.Contains(paths, "~/.ssh") {
		t.Errorf("keep %q", paths)
	}
	if len(r.Ports) != 0 || r.Resources != nil {
		t.Errorf("ports %+v, resources %+v", r.Ports, r.Resources)
	}

	// Sessions are bounded.
	tmux = nil
	for i := range 100 {
		tmux = append(tmux, fmt.Sprintf("s%d", i))
	}
	if r, _ = l.EnvDetails(t.Context(), "alpha"); len(r.Sessions) != daemonproto.MaxDetailSessions || r.Validate() != nil {
		t.Errorf("%d sessions, %v", len(r.Sessions), r.Validate())
	}
	// A failing tmux is its part's error; a stopped sandbox lists none.
	b.Exec = func(backend.ExecSpec) *exec.Cmd { return backendtest.Reply("", "boom", 1) }
	if r, _ = l.EnvDetails(t.Context(), "alpha"); !slices.Equal(partErrors(r), []string{"image", "sessions"}) || len(r.Sessions) != 0 {
		t.Errorf("tmux fails: %+v", r.Errors)
	}
	b.Fake.Status = "exited"
	if r, _ = l.EnvDetails(t.Context(), "alpha"); !slices.Equal(partErrors(r), []string{"image"}) || !r.Started.IsZero() {
		t.Errorf("stopped: %+v, started %v", r.Errors, r.Started)
	}

	// The ports are the running link's.
	e := l.env("alpha")
	e.mu.Lock()
	e.cur = &linkInstance{runner: &linkRunner{forwards: []int{3000, 3001}}}
	e.mu.Unlock()
	if r, _ = l.EnvDetails(t.Context(), "alpha"); len(r.Ports) != 2 || r.Ports[1].URL != "http://localhost:3001" || r.Validate() != nil {
		t.Errorf("ports %+v", r.Ports)
	}
	e.mu.Lock()
	e.cur = nil
	e.mu.Unlock()

	// A sandbox config that is a symlink is not read: the defaults stand
	// in, as for the launcher itself, and what the link points to is
	// never listed.
	cfg := filepath.Join(w.dataDir("alpha"), datadir.SandboxConfig)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte("[[keep]]\npath = \"~/.stolen\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cfg); err != nil {
		t.Fatal(err)
	}
	r, err = l.EnvDetails(t.Context(), "alpha")
	if err != nil || len(r.Keep) == 0 {
		t.Fatalf("symlinked config: %+v, %v", r, err)
	}
	for _, k := range r.Keep {
		if strings.Contains(k.Path, "stolen") {
			t.Errorf("read through a symlink: %+v", k)
		}
	}

	if _, err := l.EnvDetails(t.Context(), "nosuch"); daemonproto.Kind(err) != daemonproto.KindUnknownEnv {
		t.Errorf("an unknown environment: %v", err)
	}
}

// The daemon's doctor returns the findings the command line renders.
func TestDaemonDoctorMatchesTheCommand(t *testing.T) {
	w := newLinksWorld(t)
	w.env("alpha", "running")
	l := w.links(w.daemonDir())

	res, err := l.Doctor(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	if res.As != daemonproto.DoctorAsDaemon || time.Since(res.RanAt) > time.Minute || len(res.Findings) == 0 {
		t.Fatalf("%+v", res)
	}

	// The command's output, offline as the daemon runs it.
	app, _ := w.newApp("alpha")
	var cli bytes.Buffer
	app.Stdout = &cli
	_ = app.Doctor([]string{"--offline"})

	// The findings, rendered the way the command renders them.
	c := &checkup{}
	for _, f := range res.Findings {
		lv := map[string]level{daemonproto.LevelOK: levelOK, daemonproto.LevelNote: levelNote,
			daemonproto.LevelUnchecked: levelUnchecked, daemonproto.LevelProblem: levelProblem}[f.Level]
		c.add(lv, f.Label, f.Fix, "%s", f.Message)
	}
	var out bytes.Buffer
	u := newUI(&out, 0)
	u.banner("caboose doctor", "environment alpha")
	u.blank()
	c.render(u, app.Cfg.Home)
	if out.String() != cli.String() {
		t.Errorf("the daemon's findings render as\n%s\nthe command prints\n%s", out.String(), cli.String())
	}
	for _, f := range res.Findings {
		if f.Level == daemonproto.LevelProblem && f.Fix == "" {
			t.Errorf("a problem with no fix: %+v", f)
		}
	}

	// More findings than the limit are cut and say so.
	big := &checkup{}
	for range daemonproto.MaxDoctorFindings + 5 {
		big.ok("x", "y")
	}
	if r := big.result(time.Now()); len(r.Findings) != daemonproto.MaxDoctorFindings || !r.Truncated || r.Validate() != nil {
		t.Errorf("%d findings, truncated %v", len(r.Findings), r.Truncated)
	}
	// Text from the sandbox is made printable.
	esc := &checkup{}
	esc.problem("l\x1babel", "fix\x1b", "msg \x1b[31m")
	if r := esc.result(time.Now()); r.Validate() != nil || strings.Contains(r.Findings[0].Message, "\x1b") {
		t.Errorf("%+v", r.Findings)
	}
	if _, err := l.Doctor(t.Context(), "nosuch"); daemonproto.Kind(err) != daemonproto.KindUnknownEnv {
		t.Errorf("an unknown environment: %v", err)
	}
}

// The status a daemon gives carries the trust level, the checkpoints line,
// the mods without caboose's mod and, for a link another daemon serves,
// which one.
func TestEnvStatusDetails(t *testing.T) {
	w := newLinksWorld(t)
	w.env("alpha", "running")
	l := w.links(w.daemonDir())
	e := l.env("alpha")
	e.mu.Lock()
	e.state, e.sandbox = daemonproto.LinkStopped, daemonproto.SandboxRunning
	e.mu.Unlock()

	envs := l.Envs()
	if len(envs) != 1 || envs[0].Trust != "fallible" || envs[0].Owner != "" || envs[0].ModsMissing != 0 || envs[0].CheckpointsLine != "" {
		t.Fatalf("%+v", envs)
	}

	owner := func(dir string, stopped bool) string {
		t.Helper()
		if err := writeLinkState(w.dataDir("alpha"), linkState{Daemon: dir, Stopped: stopped}); err != nil {
			t.Fatal(err)
		}
		return l.Envs()[0].Owner
	}
	if got := owner(filepath.Join(w.home, daemon.DevDirName), false); got != daemonproto.OwnerDev {
		t.Errorf("a checkout's daemon: %q", got)
	}
	if got := owner(filepath.Join(w.home, "daemon-9"), false); got != daemonproto.OwnerRelease {
		t.Errorf("another release daemon: %q", got)
	}
	if got := owner(w.daemonDir(), false); got != "" {
		t.Errorf("this daemon's own record: %q", got)
	}
	if got := owner(filepath.Join(w.home, daemon.DevDirName), true); got != "" {
		t.Errorf("a link stopped on purpose: %q", got)
	}
	if got := owner("", false); got != "" {
		t.Errorf("a detached helper: %q", got)
	}

	// With this daemon's link running, the mods count shows and no other
	// daemon owns it.
	_ = writeLinkState(w.dataDir("alpha"), linkState{Daemon: filepath.Join(w.home, daemon.DevDirName)})
	e.mu.Lock()
	e.cur = &linkInstance{runner: &linkRunner{mods: &agentproto.ModsInfo{Sessions: 5, WithMod: 3}}}
	e.mu.Unlock()
	st := l.Envs()[0]
	if st.Owner != "" || st.ModsMissing != 2 {
		t.Errorf("%+v", st)
	}
	e.mu.Lock()
	e.cur = nil
	e.mu.Unlock()
}
