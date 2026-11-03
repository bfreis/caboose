package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/session"
)

// startBox answers the probes a session start makes: which sandbox paths
// are directories, what /proc/self/mountinfo says, and whether a tmux
// session exists.
type startBox struct {
	dirs     map[string]bool // the directories the session's user can enter
	mounts   []string        // mount points; nil reads as an empty mountinfo
	mountErr bool
	dirErr   bool // the probe cannot run (status 126)
	dirOne   bool // the probe fails with status 1, as an engine's error does
	session  bool // tmux has-session finds the session
}

func (s *startBox) exec(sp backend.ExecSpec) *exec.Cmd {
	a := sp.Argv
	switch {
	case len(a) == 5 && a[0] == "/bin/sh" && a[2] == `cd -- "$1" || exit 3`:
		switch {
		case s.dirErr:
			return backendtest.Reply("", "exec: no such shell", 126)
		case s.dirOne:
			return backendtest.Reply("", "Error response from daemon", 1)
		case s.dirs[a[4]]:
			return backendtest.OK()
		}
		return backendtest.Reply("", "", 3)
	case len(a) == 3 && a[0] == "/bin/sh" && strings.Contains(a[2], "/proc/self/mountinfo"):
		if s.mountErr {
			return backendtest.Reply("", "boom", 2)
		}
		var b strings.Builder
		for i, m := range s.mounts {
			// 36 35 98:0 /root MOUNTPOINT opts - type source opts
			b.WriteString("36 35 98:" + string(rune('0'+i)) + " /root " + strings.ReplaceAll(m, " ", `\040`) + " rw - ext4 /dev/vda rw\n")
		}
		return backendtest.Reply(b.String(), "", 0)
	case len(a) >= 2 && a[0] == "tmux" && a[1] == "has-session":
		if s.session {
			return backendtest.OK()
		}
		return backendtest.Fail()
	}
	return nil
}

// probes are the executions that looked at the sandbox for a start dir.
func probes(f *backendtest.Fake) int {
	n := 0
	for _, e := range f.Execs {
		if len(e.Argv) > 0 && e.Argv[0] == "/bin/sh" {
			n++
		}
	}
	return n
}

// startApp is a running sandbox under iso whose project dir proj is mapped
// to /src/app by [session.start]; the project is the current directory.
func startApp(t *testing.T, iso string, sb *startBox) (b *boxApp, proj string) {
	t.Helper()
	box := runningBox(iso)
	box.Exec = sb.exec
	b = newBoxApp(t, iso, box)
	b.mountLocal("local/linux-arm64")
	proj = filepath.Join(b.tmp, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(proj)
	b.Cfg.SessionStart = []config.StartDir{{Host: proj, Target: "/src/app"}}
	b.Cfg.Tmux = true
	b.replace = func(*exec.Cmd) error { return nil }
	b.terminal = func() bool { return true }
	return b, proj
}

// lastExec is the command Attach handed to exec.
func lastExec(f *backendtest.Fake) backend.ExecSpec { return f.Execs[len(f.Execs)-1] }

func TestStartFor(t *testing.T) {
	tmp := t.TempDir()
	tmp, _ = filepath.EvalSymlinks(tmp)
	proj := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(proj, link); err != nil {
		t.Fatal(err)
	}
	a := &App{Cfg: &config.Config{}}
	if _, ok := a.startFor(proj); ok {
		t.Error("matched with no entries")
	}
	a.Cfg.SessionStart = []config.StartDir{
		{Host: filepath.Join(tmp, "missing"), Target: "/src/missing"},
		{Host: link, Target: "/src/app"},
		{Host: proj, Target: "/src/later"},
	}
	// Another name of the directory is the directory; the missing entry
	// matches nothing; the first entry that matches wins.
	if s, ok := a.startFor(proj); !ok || s.Target != "/src/app" {
		t.Errorf("by identity: %+v %v", s, ok)
	}
	if s, ok := a.startFor(link); !ok || s.Target != "/src/app" {
		t.Errorf("the link itself: %+v %v", s, ok)
	}
	// Only the directory itself, not one below it, and not its parent.
	if s, ok := a.startFor(filepath.Join(proj, "sub")); ok {
		t.Errorf("a subdirectory matched %+v", s)
	}
	if s, ok := a.startFor(tmp); ok {
		t.Errorf("the parent matched %+v", s)
	}
	if _, ok := a.startFor(filepath.Join(tmp, "nowhere")); ok {
		t.Error("a missing project matched")
	}
}

func TestParseMountPoints(t *testing.T) {
	text := "36 35 98:0 /root /src rw - ext4 /dev/vda rw\n" +
		"37 35 98:1 / /with\\040space rw - ext4 /dev/vdb rw\n" +
		"38 35 98:2 / /back\\134slash rw - tmpfs x rw\n" +
		"short line\n\n" +
		"39 35 98:3 / /odd\\04 rw - tmpfs x rw\n"
	got := parseMountPoints(text)
	want := []string{"/src", "/with space", `/back\slash`, `/odd\04`}
	if !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestStartDirChoice(t *testing.T) {
	const def = "/work/proj"
	vol := []config.Volume{{Name: "src", Path: "/src"}}
	for _, tc := range []struct {
		name string
		sb   startBox
		vols []config.Volume
		want string // the directory
		note string // what the warning must contain, "" for none
	}{
		{"directory there", startBox{dirs: map[string]bool{"/src/app": true}}, nil, "/src/app", ""},
		{"no such directory", startBox{}, nil, def,
			`[session.start] "PROJ" = "/src/app": cannot enter /src/app in the sandbox (it is missing, not a directory, or not allowed to the session's user); starting in /work/proj`},
		{"volume not mounted", startBox{dirs: map[string]bool{"/src/app": true}, mounts: []string{"/", "/proc"}}, vol, def,
			`[session.start] "PROJ" = "/src/app": volume "src" is not mounted; starting in /work/proj`},
		{"volume mounted", startBox{dirs: map[string]bool{"/src/app": true}, mounts: []string{"/", "/src"}}, vol, "/src/app", ""},
		{"volume mounted, directory missing", startBox{mounts: []string{"/", "/src"}}, vol, def, "cannot enter /src/app in the sandbox"},
		{"mountinfo unreadable: directory test decides", startBox{dirs: map[string]bool{"/src/app": true}, mountErr: true}, vol, "/src/app", ""},
		{"mountinfo empty: directory test decides", startBox{dirs: map[string]bool{"/src/app": true}}, vol, "/src/app", ""},
		{"directory outside the volume is not asked about it", startBox{dirs: map[string]bool{"/src/app": true}, mounts: []string{"/"}},
			[]config.Volume{{Name: "other", Path: "/srv/other"}}, "/src/app", ""},
		{"probe cannot run", startBox{dirErr: true}, nil, def, "could not check it in the sandbox"},
		{"an engine error (status 1) is not a missing directory", startBox{dirOne: true}, nil, def, "could not check it in the sandbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, proj := startApp(t, isolationVM, &tc.sb)
			b.Cfg.Volumes = tc.vols
			got, warning := b.startDir(proj, def)
			if (warning != "") != (tc.note != "") {
				t.Errorf("returned warning %q", warning)
			}
			_, errs := b.said()
			if got != tc.want {
				t.Errorf("started in %q, want %q (%s)", got, tc.want, errs)
			}
			if tc.note == "" {
				if errs != "" {
					t.Errorf("said %q", errs)
				}
				return
			}
			want := strings.ReplaceAll(tc.note, "PROJ", proj)
			if !strings.Contains(errs, want) || strings.Count(errs, "\n") != 1 {
				t.Errorf("said %q, want one line with %q", errs, want)
			}
		})
	}
	t.Run("unmapped: nothing is asked of the sandbox", func(t *testing.T) {
		b, _ := startApp(t, isolationVM, &startBox{})
		other := t.TempDir()
		if got, w := b.startDir(other, def); got != def || w != "" || probes(b.box) != 0 {
			t.Errorf("%q, %d probes", got, probes(b.box))
		}
	})
}

func TestStartProbeErrorIsNotAnExitStatus(t *testing.T) {
	b, _ := startApp(t, isolationVM, &startBox{dirErr: true})
	_, err := b.sandboxCanEnter("/x")
	var ee *exec.ExitError
	if err == nil || !errors.As(err, &ee) || ee.ExitCode() != 126 {
		t.Errorf("%v", err)
	}
}

// A session starts in the directory [session.start] gives on every branch
// of Attach, under every isolation, and is named for the host project.
func TestAttachStartsInTheStartDir(t *testing.T) {
	for _, iso := range isolations {
		for _, branch := range []string{"no terminal", "no tmux", "tmux"} {
			t.Run(iso+"/"+branch, func(t *testing.T) {
				b, proj := startApp(t, iso, &startBox{dirs: map[string]bool{"/src/app": true}})
				b.terminal = func() bool { return branch != "no terminal" }
				b.Cfg.Tmux = branch == "tmux"
				if err := b.Attach(nil); err != nil {
					t.Fatalf("%v\n%s", err, b.errb)
				}
				e := lastExec(b.box)
				if branch != "tmux" {
					if e.Dir != "/src/app" {
						t.Errorf("started in %q", e.Dir)
					}
					return
				}
				name := session.NameFor(proj, "")
				want := []string{"tmux", "-u", "new-session", "-A", "-s", name, "-c", "/src/app"}
				if len(e.Argv) < len(want) || !slices.Equal(e.Argv[:len(want)], want) {
					t.Errorf("argv %q, want it to start %q", e.Argv, want)
				}
			})
		}
	}
}

// A project the mapping does not name keeps the mirrored path, and a
// directory below the mapped one is not the mapped one.
func TestAttachUnmappedKeepsTheMirroredPath(t *testing.T) {
	b, proj := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
	sub := filepath.Join(proj, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	b.terminal = func() bool { return false }
	if err := b.Attach(nil); err != nil {
		t.Fatal(err)
	}
	if e := lastExec(b.box); e.Dir != "/work/proj/sub" || probes(b.box) != 0 {
		t.Errorf("started in %q after %d probes", e.Dir, probes(b.box))
	}
}

// A fallback is said once and starts in the mirrored path.
func TestAttachFallsBackWithAWarning(t *testing.T) {
	b, proj := startApp(t, isolationVM, &startBox{})
	b.terminal = func() bool { return false }
	if err := b.Attach(nil); err != nil {
		t.Fatal(err)
	}
	if e := lastExec(b.box); e.Dir != "/work/proj" {
		t.Errorf("started in %q", e.Dir)
	}
	want := `[session.start] "` + proj + `" = "/src/app": cannot enter /src/app in the sandbox (it is missing, not a directory, or not allowed to the session's user); starting in /work/proj`
	if _, errs := b.said(); strings.Count(errs, want) != 1 {
		t.Errorf("said %q", errs)
	}
}

// -A ignores -c for a session that exists, so none of the directory is
// looked at, or warned about, then; the session keeps its directory.
func TestAttachExistingTmuxSessionIsNotProbed(t *testing.T) {
	for _, args := range [][]string{nil, {"--resume"}} {
		b, proj := startApp(t, isolationVM, &startBox{session: true})
		if err := b.Attach(args); err != nil {
			t.Fatal(err)
		}
		e := lastExec(b.box)
		i := slices.Index(e.Argv, "-c")
		if i < 0 || e.Argv[i+1] != "/work/proj" || e.Argv[slices.Index(e.Argv, "-s")+1] != session.NameFor(proj, "") {
			t.Errorf("args %q: argv %q", args, e.Argv)
		}
		if probes(b.box) != 0 {
			t.Errorf("args %q: probed %d times", args, probes(b.box))
		}
		_, errs := b.said()
		if strings.Contains(errs, "[session.start]") {
			t.Errorf("args %q: said %q", args, errs)
		}
		if got := strings.Contains(errs, "reattaching"); got != (len(args) > 0) {
			t.Errorf("args %q: reattach note %v", args, got)
		}
	}
}

// A shell keeps the mirrored path.
func TestShellIgnoresTheStartDir(t *testing.T) {
	b, _ := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
	b.terminal = func() bool { return false }
	if err := b.Shell(nil); err != nil {
		t.Fatal(err)
	}
	if e := lastExec(b.box); e.Dir != "/work/proj" || probes(b.box) != 0 {
		t.Errorf("shell in %q after %d probes", e.Dir, probes(b.box))
	}
}

// rows are c's findings as "level label text | fix".
func rows(c *checkup) []string {
	var out []string
	for _, r := range c.rows {
		s := map[level]string{levelOK: "ok", levelNote: "note", levelProblem: "problem", levelUnchecked: "unchecked"}[r.level] + " " + r.label + " " + r.text
		if r.fix != "" {
			s += " | " + r.fix
		}
		out = append(out, s)
	}
	return out
}

func TestDoctorSessionStart(t *testing.T) {
	roots := func(b *boxApp) { b.Cfg.Roots = []config.Root{{Host: b.tmp, Container: "/work"}} }
	t.Run("no entries, no rows", func(t *testing.T) {
		b, _ := startApp(t, isolationVM, &startBox{})
		b.Cfg.SessionStart = nil
		c := &checkup{}
		b.doctorSessionStart(c, true, true, "")
		if len(c.rows) != 0 || probes(b.box) != 0 {
			t.Errorf("%q", rows(c))
		}
	})
	t.Run("all well", func(t *testing.T) {
		b, proj := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
		roots(b)
		c := &checkup{}
		b.doctorSessionStart(c, true, true, "")
		want := `ok session start [session.start] "` + proj + `" = "/src/app"`
		if got := rows(c); !slices.Equal(got, []string{want}) {
			t.Errorf("%q", got)
		}
	})
	t.Run("host directory missing", func(t *testing.T) {
		b, proj := startApp(t, isolationVM, &startBox{})
		b.Cfg.SessionStart = []config.StartDir{{Host: proj + "/gone", Target: "/src/app"}}
		roots(b)
		c := &checkup{}
		b.doctorSessionStart(c, true, true, "")
		got := rows(c)
		if len(got) != 1 || !strings.HasPrefix(got[0], "note session start") || !strings.Contains(got[0], "is not a directory on this machine, so the entry matches nothing") ||
			probes(b.box) != 0 {
			t.Errorf("%q", got)
		}
	})
	t.Run("not under a root", func(t *testing.T) {
		b, proj := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
		b.Cfg.Roots = []config.Root{{Host: proj + "/sub", Container: "/work"}}
		c := &checkup{}
		b.doctorSessionStart(c, true, true, "")
		got := rows(c)
		if len(got) != 1 || !strings.Contains(got[0], "is under no root") || !strings.Contains(got[0], "| put "+proj+" under a root") {
			t.Errorf("%q", got)
		}
		// Roots that did not resolve say nothing about it.
		c = &checkup{}
		b.doctorSessionStart(c, false, true, "")
		if got := rows(c); len(got) != 1 || !strings.HasPrefix(got[0], "ok ") {
			t.Errorf("unresolved roots: %q", got)
		}
	})
	t.Run("target missing", func(t *testing.T) {
		b, _ := startApp(t, isolationVM, &startBox{})
		roots(b)
		c := &checkup{}
		b.doctorSessionStart(c, true, true, "")
		got := rows(c)
		if len(got) != 1 || !strings.Contains(got[0], "cannot enter /src/app in the sandbox") || !strings.Contains(got[0], "| make /src/app a directory the session's user can enter in the sandbox, or change or remove the entry") {
			t.Errorf("%q", got)
		}
	})
	t.Run("volume not mounted", func(t *testing.T) {
		b, _ := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}, mounts: []string{"/"}})
		b.Cfg.Volumes = []config.Volume{{Name: "src", Path: "/src"}}
		roots(b)
		c := &checkup{}
		b.doctorSessionStart(c, true, true, "")
		got := rows(c)
		if len(got) != 1 || !strings.Contains(got[0], `volume "src" is not mounted`) || !strings.Contains(got[0], "| run 'caboose restart' to mount the volume") {
			t.Errorf("%q", got)
		}
	})
	t.Run("probe cannot run", func(t *testing.T) {
		b, _ := startApp(t, isolationVM, &startBox{dirErr: true})
		roots(b)
		c := &checkup{}
		b.doctorSessionStart(c, true, true, "")
		if got := rows(c); len(got) != 1 || !strings.HasPrefix(got[0], "note session start") || !strings.Contains(got[0], "could not check /src/app") {
			t.Errorf("%q", got)
		}
	})
	t.Run("sandbox not running: host side checked, the rest said to be unchecked", func(t *testing.T) {
		b, _ := startApp(t, isolationVM, &startBox{})
		roots(b)
		c := &checkup{}
		b.doctorSessionStart(c, true, false, "docker did not answer")
		got := rows(c)
		if len(got) != 1 || !strings.HasPrefix(got[0], "unchecked session start docker did not answer, and the start directories are checked in it") || probes(b.box) != 0 {
			t.Errorf("%q, %d probes", got, probes(b.box))
		}
	})
}

// Doctor runs the check, and only when there are entries.
func TestDoctorReportsSessionStart(t *testing.T) {
	b, proj := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
	b.Cfg.Roots = []config.Root{{Host: b.tmp, Container: "/work"}}
	_ = b.Doctor([]string{"--offline"})
	out, _ := b.said()
	if !strings.Contains(out, `session start`) || !strings.Contains(out, `[session.start] "`+proj+`" = "/src/app"`) {
		t.Errorf("doctor:\n%s", out)
	}
	b.Cfg.SessionStart = nil
	_ = b.Doctor([]string{"--offline"})
	if out, _ := b.said(); strings.Contains(out, "session start") {
		t.Errorf("doctor with no entries:\n%s", out)
	}
}

// A fallback on the path that creates a tmux session is shown in the
// client too, chained after new-session, since stderr is hidden by the
// client's alternate screen; claude's arguments and -c are untouched.
func TestAttachFallbackIsShownInTmux(t *testing.T) {
	b, proj := startApp(t, isolationVM, &startBox{})
	if err := b.Attach([]string{"--resume", "a b"}); err != nil {
		t.Fatal(err)
	}
	argv := lastExec(b.box).Argv
	i := slices.Index(argv, ";")
	if i < 0 {
		t.Fatalf("no chained command: %q", argv)
	}
	want := `[session.start] "` + strings.ReplaceAll(proj, "#", "##") + `" = "/src/app": cannot enter /src/app in the sandbox (it is missing, not a directory, or not allowed to the session's user); starting in /work/proj`
	if got := argv[i+1:]; !slices.Equal(got, []string{"display-message", "-d", "15000", want}) {
		t.Errorf("chained %q, want display-message of %q", got, want)
	}
	if got := argv[i-3 : i]; !slices.Equal(got, []string{Entrypoint, "--resume", "a b"}) {
		t.Errorf("the command and its arguments are %q", got)
	}
	if j := slices.Index(argv, "-c"); j < 0 || argv[j+1] != "/work/proj" {
		t.Errorf("-c in %q", argv)
	}
	if _, errs := b.said(); !strings.Contains(errs, "starting in /work/proj") {
		t.Errorf("not noted on stderr too: %q", errs)
	}
}

// No fallback, nothing chained: a mapped session that starts where it was
// asked to, an unmapped one, and an existing one are the argv they were.
func TestAttachWithoutFallbackChainsNothing(t *testing.T) {
	b, _ := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
	if err := b.Attach(nil); err != nil {
		t.Fatal(err)
	}
	if argv := lastExec(b.box).Argv; slices.Contains(argv, ";") {
		t.Errorf("mapped: %q", argv)
	}
	b, _ = startApp(t, isolationVM, &startBox{session: true})
	if err := b.Attach([]string{"--resume"}); err != nil {
		t.Fatal(err)
	}
	if argv := lastExec(b.box).Argv; slices.Contains(argv, ";") {
		t.Errorf("existing session: %q", argv)
	}
}

func TestStartNotice(t *testing.T) {
	if got := startNotice([]string{"tmux"}, ""); got != nil {
		t.Errorf("no warning: %q", got)
	}
	got := startNotice([]string{"tmux", "x"}, "a #{b} c;")
	if want := []string{";", "display-message", "-d", "15000", "a ##{b} c; "}; !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
	// An argument ending in ; would be read as the end of the command.
	if got := startNotice([]string{"tmux", "--flag;"}, "w"); got != nil {
		t.Errorf("chained after an argument ending in ;: %q", got)
	}
}

// A tmux launch for a project no entry names looks at nothing in the
// sandbox: no has-session (with no arguments to warn about), no probe.
func TestAttachUnmappedTmuxRunsNoProbe(t *testing.T) {
	b, _ := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
	other := filepath.Join(b.tmp, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(other)
	if err := b.Attach(nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.box.Execs {
		if len(e.Argv) > 1 && e.Argv[0] == "tmux" && e.Argv[1] == "has-session" || len(e.Argv) > 0 && e.Argv[0] == "/bin/sh" {
			t.Errorf("ran %q", e.Argv)
		}
	}
	if argv := lastExec(b.box).Argv; slices.Contains(argv, ";") || argv[0] != "tmux" {
		t.Errorf("argv %q", argv)
	}
}

// A sandbox config root that holds a [session.start] target is there so
// that the memory of that project syncs: doctor does not ask for a root of
// this machine at it. One that holds none is still reported.
func TestDoctorRootHoldingAStartTargetIsExpected(t *testing.T) {
	e := newAutoEnv(t, newBare(t))
	e.setRemote()
	e.a.Cfg.SessionStart = []config.StartDir{{Host: "/h/app", Target: "/src/app"}}
	e.write(filepath.Join(e.data, datadir.SandboxConfig), string(sandboxcfg.Default([]string{"/work/dev", "/src", "/srv/other"})))
	c := &checkup{}
	e.a.sb = nil
	e.a.doctorSandbox(c)
	got := c.String()
	if strings.Contains(got, "expects a root at /src") {
		t.Errorf("reported /src:\n%s", got)
	}
	if !strings.Contains(got, "expects a root at /srv/other") {
		t.Errorf("did not report /srv/other:\n%s", got)
	}
}

// Doctor says why the sandbox was not asked: it is not running, or docker
// did not answer.
func TestDoctorSessionStartSaysWhyItWasNotAsked(t *testing.T) {
	const row = "session start  not checked: "
	t.Run("not running", func(t *testing.T) {
		box := runningBox(isolationVM)
		box.Status = "exited"
		b := newBoxApp(t, isolationVM, box)
		b.Cfg.Roots = []config.Root{{Host: b.tmp, Container: "/work"}}
		b.Cfg.SessionStart = []config.StartDir{{Host: b.tmp, Target: "/src/app"}}
		_ = b.Doctor([]string{"--offline"})
		if out, _ := b.said(); !strings.Contains(out, row+"the VM is not running, and the start directories are checked in it") {
			t.Errorf("doctor:\n%s", out)
		}
	})
	t.Run("docker did not answer", func(t *testing.T) {
		b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
		if err := os.WriteFile(filepath.Join(b.tmp, "docker"), []byte("#!/bin/sh\necho down >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		b.Cfg.Roots = []config.Root{{Host: b.tmp, Container: "/work"}}
		b.Cfg.SessionStart = []config.StartDir{{Host: b.tmp, Target: "/src/app"}}
		_ = b.Doctor([]string{"--offline"})
		if out, _ := b.said(); !strings.Contains(out, row+"docker did not answer, and the start directories are checked in it") {
			t.Errorf("doctor:\n%s", out)
		}
	})
}
