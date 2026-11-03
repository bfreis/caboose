package launcher

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
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
	dirErr   bool     // the probe cannot run (status 126)
	dirOne   bool     // the probe fails with status 1, as an engine's error does
	session  bool     // tmux has-session finds the session
	sessions []string // sessions with a client (list-sessions, list-clients)
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
	case len(a) >= 2 && a[0] == "tmux" && a[1] == "list-sessions":
		return backendtest.Reply(strings.Join(s.sessions, "\n")+"\n", "", 0)
	case len(a) >= 2 && a[0] == "tmux" && a[1] == "list-clients":
		if slices.Contains(s.sessions, strings.TrimPrefix(a[len(a)-1], "=")) {
			return backendtest.Reply("client\n", "", 0)
		}
		return backendtest.Reply("", "", 0)
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

// startApp is a running sandbox under iso whose one root is b.tmp, at /work;
// the current directory is proj, under it (so /work/proj in the sandbox).
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
	b.Cfg.Tmux = true
	b.replace = func(*exec.Cmd) error { return nil }
	b.terminal = func() bool { return true }
	return b, proj
}

// lastExec is the command Attach handed to exec.
func lastExec(f *backendtest.Fake) backend.ExecSpec { return f.Execs[len(f.Execs)-1] }

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

func TestStartProbeErrorIsNotAnExitStatus(t *testing.T) {
	b, _ := startApp(t, isolationVM, &startBox{dirErr: true})
	_, err := b.sandboxCanEnter("/x")
	var ee *exec.ExitError
	if err == nil || !errors.As(err, &ee) || ee.ExitCode() != 126 {
		t.Errorf("%v", err)
	}
}

// A session starts in the directory [session.start] gives on every branch

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

func TestStartNotice(t *testing.T) {
	if got := startNotice([]string{"tmux"}, ""); got != nil {
		t.Errorf("no warning: %q", got)
	}
	got := startNotice([]string{"tmux", "x"}, "a #{b} c;")
	if want := []string{";", "display-message", "-d", "15000", "a ##{b} c; "}; !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
	// An argument ending in ; would be read as the end of the command; one
	// ending in \; is a literal ; to tmux (as the directory is given).
	if got := startNotice([]string{"tmux", "--flag;"}, "w"); got != nil {
		t.Errorf("chained after an argument ending in ;: %q", got)
	}
	if got := startNotice([]string{"tmux", `-c`, `/d\;`}, "w"); got == nil {
		t.Error("not chained after an escaped ;")
	}
	// tmux expands strftime conversions in the text too: % is escaped as %%.
	got = startNotice([]string{"tmux"}, "100% of %Y # x")
	if want := []string{";", "display-message", "-d", "15000", "100%% of %%Y ## x"}; !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
}

// A tmux launch for a project no entry names looks at nothing in the

// proj2 makes dir (under the root tmp, so /work/<rel> in the sandbox) and
// makes it the current directory.
func chdirUnder(t *testing.T, b *boxApp, rel string) {
	t.Helper()
	d := filepath.Join(b.tmp, rel)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(d)
}

// outside makes a directory under no root the current one.
func chdirOutside(t *testing.T) {
	t.Helper()
	t.Chdir(physicalTempDir(t))
}

func TestPickDir(t *testing.T) {
	one := []config.Root{{Host: "/h/dev", Container: "/work/dev"}}
	for _, tc := range []struct {
		name string
		c    dirChoice
		want launchDir
		err  string
	}{
		{"-C wins over everything", dirChoice{Flag: "/src/x", Setting: "/src", Cwd: "/h/dev/p", Configured: one}, launchDir{"/src/x", dirFlag}, ""},
		{"-C relative to dir", dirChoice{Flag: "x/../y", Setting: "/src", Cwd: "/h/dev/p", Configured: one}, launchDir{"/src/y", dirFlag}, ""},
		{"-C relative with no dir", dirChoice{Flag: "x", Cwd: "/h/dev/p", Configured: one}, launchDir{}, "no [session] dir"},
		{"-C with a ~", dirChoice{Flag: "~/x", Setting: "/src", Configured: one}, launchDir{}, "-C takes a directory in the sandbox"},
		{"mirrored beats dir", dirChoice{Setting: "/src", Cwd: "/h/dev/p", Configured: one}, launchDir{"/work/dev/p", dirMirrored}, ""},
		{"mirrored uses what is mounted", dirChoice{Cwd: "/h/dev/p", Known: true, Mounted: []config.Root{{Host: "/h/dev", Container: "/work/other"}}, Configured: one},
			launchDir{"/work/other/p", dirMirrored}, ""},
		{"outside the roots: dir", dirChoice{Setting: "/src", Cwd: "/tmp/x", Configured: one}, launchDir{"/src", dirSetting}, ""},
		{"outside the roots: refused, naming -C and dir", dirChoice{Cwd: "/tmp/x", Configured: one}, launchDir{}, "caboose -C DIR for one run, or dir in [session]"},
		{"no roots: home", dirChoice{Cwd: "/tmp/x"}, launchDir{sandboxHome, dirHome}, ""},
		{"no roots, no cwd: home", dirChoice{}, launchDir{sandboxHome, dirHome}, ""},
		{"no roots: dir beats home", dirChoice{Setting: "/src", Cwd: "/tmp/x"}, launchDir{"/src", dirSetting}, ""},
		{"created with no roots, configured none: home", dirChoice{Cwd: "/tmp/x", Known: true, Mounted: []config.Root{}}, launchDir{sandboxHome, dirHome}, ""},
		{"created with no roots, root since configured: restart", dirChoice{Cwd: "/h/dev/p", Known: true, Mounted: []config.Root{}, Configured: one}, launchDir{}, "'caboose restart'"},
		{"created with no roots, root since configured, even with dir", dirChoice{Setting: "/src", Cwd: "/h/dev/p", Known: true, Mounted: []config.Root{}, Configured: one}, launchDir{}, "'caboose restart'"},
		{"created with no roots, outside the new root: home", dirChoice{Cwd: "/tmp/x", Known: true, Mounted: []config.Root{}, Configured: one}, launchDir{sandboxHome, dirHome}, ""},
		{"unknown sandbox judged by the configured roots", dirChoice{Cwd: "/h/dev/p", Configured: one}, launchDir{"/work/dev/p", dirMirrored}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.Origin, tc.c.Noun = "o", "container"
			got, err := pickDir(tc.c)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("err %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("%+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestMountedRootsKnown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		known  bool
		n      int
	}{
		{"no label", map[string]string{}, false, 0},
		{"a bad label", map[string]string{assets.LabelRoots: "{"}, false, 0},
		{"none", map[string]string{assets.LabelRoots: "[]"}, true, 0},
		{"one", map[string]string{assets.LabelRoots: `["/work"]`}, true, 1},
		{"null is not none", map[string]string{assets.LabelRoots: "null"}, false, 0},
		{"a root with no mount", map[string]string{assets.LabelRoots: `["/work/dev"]`}, false, 0},
		{"one of two with no mount", map[string]string{assets.LabelRoots: `["/work","/work/dev"]`}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := runningBox(isolationVM)
			maps.Copy(box.SandboxLabels, tc.labels)
			box.SandboxMounts = []backend.Mount{{Source: "/h", Target: "/work"}}
			b := newBoxApp(t, isolationVM, box)
			roots, known := b.mountedRootsKnown()
			if known != tc.known || len(roots) != tc.n {
				t.Errorf("%v %v", roots, known)
			}
			if !known && roots != nil {
				t.Error("unknown with roots")
			}
		})
	}
	t.Run("no sandbox", func(t *testing.T) {
		b := newBoxApp(t, isolationVM, &backendtest.Fake{})
		if _, known := b.mountedRootsKnown(); known {
			t.Error("known with no sandbox")
		}
	})
}

// Zero roots seen from a sandbox that has none are none (fail closed), and
// a configured root is a restart away, not visible.
func TestNoRootsDrift(t *testing.T) {
	box := runningBox(isolationVM)
	box.SandboxLabels[assets.LabelRoots] = "[]"
	b := newBoxApp(t, isolationVM, box)
	if got := b.visibleRoots(); len(got) != 0 {
		t.Errorf("visible roots of a sandbox with none: %v", got)
	}
	b.warnIfRootsDrifted()
	if _, errs := b.said(); !strings.Contains(errs, "mounts no roots") || !strings.Contains(errs, "caboose restart") {
		t.Errorf("drift not said: %q", errs)
	}
	b.Cfg.Roots = []config.Root{}
	b.warnIfRootsDrifted()
	if _, errs := b.said(); errs != "" {
		t.Errorf("said %q", errs)
	}
}

// pinned is the sequence of tmux/probe commands a tmux launch ran, by
// first word and subcommand.
func execKinds(f *backendtest.Fake) []string {
	var out []string
	for _, e := range f.Execs {
		if len(e.Argv) > 1 && e.Argv[0] == "tmux" {
			out = append(out, "tmux "+e.Argv[1])
		} else if len(e.Argv) > 0 {
			out = append(out, e.Argv[0])
		}
	}
	return out
}

func tmuxName(argv []string) string {
	if i := slices.Index(argv, "-s"); i >= 0 {
		return argv[i+1]
	}
	return ""
}

func tmuxDir(argv []string) string {
	if i := slices.Index(argv, "-c"); i >= 0 {
		return argv[i+1]
	}
	return ""
}

// Every branch of Attach, under every isolation, starts where the order of
// choices says.
func TestAttachDirectoryChoice(t *testing.T) {
	good := map[string]bool{"/src/app": true, "/src": true, "/src/rel": true}
	for _, iso := range isolations {
		for _, branch := range []string{"no terminal", "no tmux", "tmux"} {
			for _, tc := range []struct {
				name    string
				flag    string
				setting string
				outside bool
				noRoots bool
				want    string
			}{
				{"mirrored", "", "", false, false, "/work/proj"},
				{"mirrored beats dir", "", "/src", false, false, "/work/proj"},
				{"-C beats mirrored", "/src/app", "", false, false, "/src/app"},
				{"-C relative", "rel", "/src", false, false, "/src/rel"},
				{"dir outside the roots", "", "/src", true, false, "/src"},
				{"no roots: home", "", "", true, true, sandboxHome},
				{"no roots: home from the old project dir", "", "", false, true, sandboxHome},
				{"no roots: dir", "", "/src", true, true, "/src"},
			} {
				t.Run(iso+"/"+branch+"/"+tc.name, func(t *testing.T) {
					b, _ := startApp(t, iso, &startBox{dirs: good})
					b.terminal = func() bool { return branch != "no terminal" }
					b.Cfg.Tmux = branch == "tmux"
					b.Dir, b.Cfg.SessionDir = tc.flag, tc.setting
					if tc.noRoots {
						b.Cfg.Roots = []config.Root{}
						b.box.SandboxLabels[assets.LabelRoots] = "[]"
					}
					if tc.outside {
						chdirOutside(t)
					}
					if err := b.Attach(nil); err != nil {
						t.Fatalf("%v\n%s", err, b.errb)
					}
					e := lastExec(b.box)
					got := e.Dir
					if branch == "tmux" {
						got = tmuxDir(e.Argv)
					}
					if got != tc.want {
						t.Errorf("started in %q, want %q", got, tc.want)
					}
					if _, errs := b.said(); strings.Contains(errs, "starting in") {
						t.Errorf("warned: %q", errs)
					}
				})
			}
		}
	}
}

// Outside the roots with nothing to choose from is today's refusal, now
// naming dir and -C; and nothing is started.
func TestAttachOutsideRootsRefusedNamingTheWays(t *testing.T) {
	for _, iso := range isolations {
		b, _ := startApp(t, iso, &startBox{})
		chdirOutside(t)
		err := b.Attach(nil)
		if err == nil || !strings.Contains(err.Error(), "-C DIR") || !strings.Contains(err.Error(), "[session]") {
			t.Errorf("%s: %v", iso, err)
		}
		if err := b.Shell(nil); err == nil || !strings.Contains(err.Error(), "-C DIR") {
			t.Errorf("%s shell: %v", iso, err)
		}
	}
}

// A directory choice that can only fail is refused before the sandbox is
// built, created or started, for Attach and Shell: from outside the roots
// with no -C and no dir, a stopped sandbox stays stopped and an absent one
// is not made.
func TestUnusableChoiceIsRefusedBeforeAnythingStarts(t *testing.T) {
	for _, iso := range isolations {
		for _, status := range []string{"exited", ""} {
			for _, cmd := range []string{"attach", "shell"} {
				t.Run(iso+"/"+status+"/"+cmd, func(t *testing.T) {
					b, _ := startApp(t, iso, &startBox{})
					b.box.Status = status
					chdirOutside(t)
					var err error
					if cmd == "shell" {
						err = b.Shell(nil)
					} else {
						err = b.Attach(nil)
					}
					if err == nil || !strings.Contains(err.Error(), "-C DIR") {
						t.Errorf("err %v", err)
					}
					if len(b.box.Calls) != 0 || len(b.box.Specs) != 0 {
						t.Errorf("lifecycle calls %q, created %d", b.box.Calls, len(b.box.Specs))
					}
				})
			}
		}
	}
}

// The same session from every route to one sandbox path.
func TestSessionNameIsTheSandboxPath(t *testing.T) {
	want := session.NameFor("/work/projects/app", "")
	run := func(t *testing.T, setup func(t *testing.T, b *boxApp)) string {
		b, _ := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/work/projects/app": true}})
		chdirUnder(t, b, "projects/app")
		setup(t, b)
		if err := b.Attach(nil); err != nil {
			t.Fatalf("%v\n%s", err, b.errb)
		}
		return tmuxName(lastExec(b.box).Argv)
	}
	for name, setup := range map[string]func(t *testing.T, b *boxApp){
		"run in the Mac directory": func(t *testing.T, b *boxApp) {},
		"-C, absolute":             func(t *testing.T, b *boxApp) { b.Dir = "/work/projects/app"; chdirOutside(t) },
		"-C relative to dir": func(t *testing.T, b *boxApp) {
			b.Dir, b.Cfg.SessionDir = "app", "/work/projects"
			chdirOutside(t)
		},
		"-C, unclean": func(t *testing.T, b *boxApp) { b.Dir = "/work/projects/x/../app/"; chdirOutside(t) },
	} {
		t.Run(name, func(t *testing.T) {
			if got := run(t, setup); got != want {
				t.Errorf("session %q, want %q", got, want)
			}
		})
	}
}

// Detach names the session the same way, with no probing and no refusal
// for a missing directory.
func TestDetachFollowsTheDirectoryChoice(t *testing.T) {
	name := session.NameFor("/src/app", "")
	b, _ := startApp(t, isolationVM, &startBox{sessions: []string{name}})
	b.Dir = "/src/app" // not there in the sandbox: it only names
	if err := b.Detach(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range b.box.Execs {
		if len(e.Argv) > 1 && e.Argv[1] == "detach-client" && e.Argv[len(e.Argv)-1] == "="+name {
			found = true
		}
		if len(e.Argv) > 0 && e.Argv[0] == "/bin/sh" {
			t.Errorf("probed: %q", e.Argv)
		}
	}
	if !found {
		t.Errorf("no detach of %s: %v", name, execKinds(b.box))
	}
	// A project under a root is named by its sandbox path too.
	b, _ = startApp(t, isolationVM, &startBox{sessions: []string{session.NameFor("/work/proj", "")}})
	if err := b.Detach(); err != nil {
		t.Fatal(err)
	}
	if _, errs := b.said(); !strings.Contains(errs, "detached clients from '"+session.NameFor("/work/proj", "")+"'") {
		t.Errorf("said %q", errs)
	}
}

// With [session] dir set, Detach names the session Attach started, also
// when the directory could not be used and the session started in the home
// directory: both name it from the setting's path, not from where it
// started.
func TestDetachFindsTheSessionOfDir(t *testing.T) {
	name := session.NameFor("/src/app", "")
	for _, tc := range []struct {
		name string
		sb   startBox
		home bool
	}{
		{"usable", startBox{dirs: map[string]bool{"/src/app": true}}, false},
		{"home fallback", startBox{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := tc.sb
			b, _ := startApp(t, isolationVM, &sb)
			b.Cfg.SessionDir = "/src/app"
			chdirOutside(t)
			if err := b.Attach(nil); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			argv := lastExec(b.box).Argv
			wantDir := "/src/app"
			if tc.home {
				wantDir = sandboxHome
			}
			if tmuxName(argv) != name || tmuxDir(argv) != wantDir {
				t.Fatalf("attach made session %q in %q, want %q in %q", tmuxName(argv), tmuxDir(argv), name, wantDir)
			}
			sb.sessions = []string{name} // a client is on it now
			if err := b.Detach(); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range b.box.Execs {
				if len(e.Argv) > 1 && e.Argv[1] == "detach-client" && e.Argv[len(e.Argv)-1] == "="+name {
					found = true
				}
			}
			if !found {
				t.Errorf("no detach of %s: %v", name, execKinds(b.box))
			}
		})
	}
}

// -C that the session's user cannot enter starts nothing and says what to
// do, on every branch; so does a check that could not run.
func TestFlagDirMissingRefusesAndStartsNothing(t *testing.T) {
	for _, iso := range isolations {
		for _, branch := range []string{"no terminal", "no tmux", "tmux", "shell"} {
			for _, sb := range []startBox{{}, {dirErr: true}} {
				t.Run(iso+"/"+branch, func(t *testing.T) {
					b, _ := startApp(t, iso, &sb)
					b.terminal = func() bool { return branch != "no terminal" }
					b.Cfg.Tmux = branch == "tmux"
					b.Dir = "/Users/me/x"
					var err error
					if branch == "shell" {
						err = b.Shell(nil)
					} else {
						err = b.Attach(nil)
					}
					if err == nil || !strings.Contains(err.Error(), "-C takes a directory in the sandbox, not on this machine") ||
						!strings.Contains(err.Error(), "caboose shell") || !strings.Contains(err.Error(), "No session was started") {
						t.Fatalf("err %v", err)
					}
					for _, e := range b.box.Execs {
						if len(e.Argv) > 1 && (e.Argv[1] == "new-session" || e.Argv[0] == Entrypoint || e.Argv[0] == "bash") {
							t.Errorf("started %q", e.Argv)
						}
					}
				})
			}
		}
	}
}

// [session] dir that cannot be used falls back to the home with one line
// naming the setting and both directories, everywhere.
func TestSettingDirFallsBackToHome(t *testing.T) {
	vol := []config.Volume{{Name: "src", Path: "/src"}}
	for _, tc := range []struct {
		name string
		sb   startBox
		why  string
	}{
		{"missing", startBox{}, "cannot enter /src/app in the sandbox"},
		{"volume not mounted", startBox{dirs: map[string]bool{"/src/app": true}, mounts: []string{"/", "/proc"}}, `volume "src" is not mounted`},
		{"check cannot run", startBox{dirErr: true}, "could not check it in the sandbox"},
		{"engine error is not a missing directory", startBox{dirOne: true}, "could not check it in the sandbox"},
	} {
		for _, branch := range []string{"no terminal", "tmux", "shell"} {
			t.Run(tc.name+"/"+branch, func(t *testing.T) {
				b, _ := startApp(t, isolationVM, &tc.sb)
				b.Cfg.Volumes = vol
				b.Cfg.SessionDir = "/src/app"
				b.terminal = func() bool { return branch != "no terminal" }
				chdirOutside(t)
				var err error
				if branch == "shell" {
					err = b.Shell(nil)
				} else {
					err = b.Attach(nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := `[session] dir = "/src/app": ` + tc.why
				_, errs := b.said()
				if strings.Count(errs, want) != 1 || !strings.Contains(errs, "starting in "+sandboxHome) {
					t.Errorf("said %q", errs)
				}
				e := lastExec(b.box)
				if branch == "tmux" {
					if tmuxDir(e.Argv) != sandboxHome {
						t.Errorf("argv %q", e.Argv)
					}
					i := slices.Index(e.Argv, ";")
					if i < 0 || e.Argv[i+1] != "display-message" || !strings.Contains(e.Argv[len(e.Argv)-1], want) {
						t.Errorf("not shown in tmux: %q", e.Argv)
					}
				} else if e.Dir != sandboxHome {
					t.Errorf("started in %q", e.Dir)
				}
			})
		}
	}
}

// An existing tmux session is reattached with no check and no warning,
// whatever -C or dir say; has-session is asked once.
func TestExistingSessionIsNotChecked(t *testing.T) {
	for _, args := range [][]string{nil, {"--resume"}} {
		b, _ := startApp(t, isolationVM, &startBox{session: true})
		b.Dir = "/gone/x"
		if err := b.Attach(args); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range b.box.Execs {
			if len(e.Argv) > 1 && e.Argv[1] == "has-session" {
				n++
			}
		}
		if n != 1 || probes(b.box) != 0 {
			t.Errorf("args %q: %d has-session, %d probes", args, n, probes(b.box))
		}
		if tmuxName(lastExec(b.box).Argv) != session.NameFor("/gone/x", "") {
			t.Errorf("name %q", tmuxName(lastExec(b.box).Argv))
		}
		if _, errs := b.said(); strings.Contains(errs, "starting in") {
			t.Errorf("warned %q", errs)
		}
	}
}

// A launch that needs nothing from the sandbox -- the mirrored path or the
// home, no arguments -- runs no has-session and no probe: the execs on the
// tmux branch are exactly the free-name search and the session.
func TestUnmappedLaunchRunsNoExtraExec(t *testing.T) {
	for _, noRoots := range []bool{false, true} {
		b, _ := startApp(t, isolationVM, &startBox{})
		if noRoots {
			b.Cfg.Roots = []config.Root{}
			b.box.SandboxLabels[assets.LabelRoots] = "[]"
			chdirOutside(t)
		}
		if err := b.Attach(nil); err != nil {
			t.Fatal(err)
		}
		for _, k := range execKinds(b.box) {
			if k == "tmux has-session" || k == "/bin/sh" {
				t.Errorf("noRoots=%v ran %s in %v", noRoots, k, execKinds(b.box))
			}
		}
		want := []string{"test", "/usr/local/bin/caboose-agent", "infocmp", "tmux list-clients", "tmux -u"}
		if got := execKinds(b.box); !slices.Equal(got, want) {
			t.Errorf("noRoots=%v: execs %v, want %v", noRoots, got, want)
		}
	}
}

// With a -C or dir to look at, has-session is asked once and the probe once.
func TestCheckedLaunchAsksOnce(t *testing.T) {
	b, _ := startApp(t, isolationVM, &startBox{dirs: map[string]bool{"/src/app": true}})
	b.Dir = "/src/app"
	if err := b.Attach(nil); err != nil {
		t.Fatal(err)
	}
	kinds := execKinds(b.box)
	if c := strings.Count(strings.Join(kinds, ","), "tmux has-session"); c != 1 || probes(b.box) != 1 {
		t.Errorf("%v", kinds)
	}
}

// doctor: [session] dir by the launch's probe, or why it was not asked.
func TestDoctorSessionDir(t *testing.T) {
	setup := func(sb startBox, dir string) (*boxApp, *checkup) {
		b, _ := startApp(t, isolationVM, &sb)
		b.Cfg.SessionDir = dir
		return b, &checkup{}
	}
	t.Run("none", func(t *testing.T) {
		b, c := setup(startBox{}, "")
		b.doctorSessionDir(c, true, "")
		if len(c.rows) != 0 {
			t.Error(rows(c))
		}
	})
	t.Run("ok", func(t *testing.T) {
		b, c := setup(startBox{dirs: map[string]bool{"/src": true}}, "/src")
		b.doctorSessionDir(c, true, "")
		if got := rows(c); len(got) != 1 || got[0] != `ok session dir [session] dir = "/src"` {
			t.Error(got)
		}
	})
	t.Run("missing", func(t *testing.T) {
		b, c := setup(startBox{}, "/src")
		b.doctorSessionDir(c, true, "")
		if got := rows(c); len(got) != 1 || !strings.HasPrefix(got[0], "problem session dir") || !strings.Contains(got[0], "cannot enter /src") {
			t.Error(got)
		}
	})
	t.Run("volume not mounted", func(t *testing.T) {
		b, c := setup(startBox{dirs: map[string]bool{"/src": true}, mounts: []string{"/"}}, "/src")
		b.Cfg.Volumes = []config.Volume{{Name: "src", Path: "/src"}}
		b.doctorSessionDir(c, true, "")
		if got := rows(c); len(got) != 1 || !strings.Contains(got[0], "caboose restart") {
			t.Error(got)
		}
	})
	t.Run("check cannot run", func(t *testing.T) {
		b, c := setup(startBox{dirErr: true}, "/src")
		b.doctorSessionDir(c, true, "")
		if got := rows(c); len(got) != 1 || !strings.HasPrefix(got[0], "note session dir") {
			t.Error(got)
		}
	})
	t.Run("not running", func(t *testing.T) {
		b, c := setup(startBox{}, "/src")
		b.doctorSessionDir(c, false, "docker did not answer")
		if got := rows(c); len(got) != 1 || got[0] != "unchecked session dir docker did not answer, and the directory is checked in it" || probes(b.box) != 0 {
			t.Error(got)
		}
	})
}

// A sandbox-config roots entry that is, or holds, a volume's path or
// [session] dir is expected without a root; one that holds neither is not.
func TestDoctorRootHoldingAWorkdirIsExpected(t *testing.T) {
	for name, set := range map[string]func(a *App){
		"dir":           func(a *App) { a.Cfg.SessionDir = "/src/app" },
		"volume":        func(a *App) { a.Cfg.Volumes = []config.Volume{{Name: "src", Path: "/src/app"}} },
		"volume equals": func(a *App) { a.Cfg.Volumes = []config.Volume{{Name: "src", Path: "/src"}} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newAutoEnv(t, newBare(t))
			e.setRemote()
			set(e.a)
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
		})
	}
}

// With sync on, a volume or dir the roots do not hold is noted, a cache
// volume is not.
func TestDoctorUnsyncedWorkdirs(t *testing.T) {
	a := &App{Cfg: &config.Config{SessionDir: "/src/app", Volumes: []config.Volume{
		{Name: "data", Path: "/srv/data"}, {Name: "gocache", Path: "/home/agent/.cache/go"}, {Name: "held", Path: "/work/dev/v"}}}}
	c := &checkup{}
	a.doctorUnsyncedWorkdirs(c, []string{"/work/dev"}, "edit it")
	got := c.String()
	if !strings.Contains(got, "/src/app is a volume's path or [session] dir") || !strings.Contains(got, "/srv/data is") ||
		strings.Contains(got, "gocache") || strings.Contains(got, ".cache/go is") || strings.Contains(got, "/work/dev/v is") {
		t.Errorf("%s", got)
	}
}

// With no roots a volume holds the only copy: noted unless a checkpoint
// covers it (vm) or cannot exist (container, gvisor).
func TestDoctorVolumeCopies(t *testing.T) {
	vols := []config.Volume{{Name: "src", Path: "/src"}, {Name: "other", Path: "/srv/o"}}
	for _, tc := range []struct {
		iso   string
		roots []config.Root
		cp    []string
		notes int
		has   string
	}{
		{isolationVM, []config.Root{}, nil, 2, "is in no [checkpoints] paths entry"},
		{isolationVM, []config.Root{}, []string{"/src"}, 1, "/srv/o"},
		{isolationVM, []config.Root{}, []string{"/src", "/srv/o"}, 0, ""},
		{isolationVM, []config.Root{{Host: "/h", Container: "/work"}}, nil, 0, ""},
		{isolationContainer, []config.Root{}, []string{"/src"}, 2, "cannot be checkpointed under container"},
		{isolationGVisor, []config.Root{}, nil, 2, "cannot be checkpointed under gvisor"},
	} {
		b := newBoxApp(t, tc.iso, runningBox(tc.iso))
		b.Cfg.Volumes, b.Cfg.Roots, b.Cfg.Checkpoints = vols, tc.roots, tc.cp
		c := &checkup{}
		b.doctorVolumeCopies(c)
		if len(c.rows) != tc.notes || tc.has != "" && !strings.Contains(c.String(), tc.has) {
			t.Errorf("%s %v %v: %s", tc.iso, tc.roots, tc.cp, c.String())
		}
	}
}

// Under every isolation a sandbox created with no roots has the mounts
// caboose owns and no root: no mount at /work or at any host directory
// but its own, a roots label that says none, and the sandbox that is made
// reads back as one with none. (The vm isolation's planShares works on
// whatever mounts it is given: its own test covers a list with no root.)
func TestCreateWithNoRoots(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := &backendtest.Fake{}
			b := newBoxApp(t, iso, box)
			b.Cfg.Roots = []config.Root{}
			if err := b.ensureRunning(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			if len(box.Specs) != 1 {
				t.Fatalf("%d sandboxes created", len(box.Specs))
			}
			sp := box.Specs[0]
			for _, m := range sp.Mounts {
				if config.Within(m.Target, config.WorkDir) {
					t.Errorf("mount at %s with no roots: %+v", m.Target, m)
				}
			}
			if !slices.Contains(sp.Labels, assets.LabelRoots+"=[]") {
				t.Errorf("no roots label saying none in %q", sp.Labels)
			}
			if roots, known := b.mountedRootsKnown(); !known || len(roots) != 0 {
				t.Errorf("reads back as %v, known %v", roots, known)
			}
			if _, err := b.pickLaunchDir(); err != nil {
				t.Errorf("a launch from here is refused: %v", err)
			}
		})
	}
}

// A sandbox created with no roots against a configuration that now has
// one, and the reverse, is drift that a restart settles, said by the
// permissions report and by doctor; two agreeing on none is not. The
// writable parts are not compared when the roots differ, and there are none
// to compare when both have none.
func TestNoRootsRestartDrift(t *testing.T) {
	for _, iso := range isolations {
		for _, tc := range []struct {
			name       string
			mounted    string // the roots label
			mounts     []backend.Mount
			configured bool
			drift      []string // what it says; none for no drift

		}{
			{"none, none", "[]", nil, false, nil},
			{"none, one configured", "[]", nil, true, []string{"mounts no roots; the configuration says "}},
			{"one, none configured", `["/work"]`, []backend.Mount{{Source: "/h/dev", Target: "/work"}}, false, []string{"mounts /h/dev at /work; the configuration says no roots"}},
		} {
			t.Run(iso+"/"+tc.name, func(t *testing.T) {
				box := runningBox(iso)
				b := newBoxApp(t, iso, box)
				box.SandboxLabels[assets.LabelRoots] = tc.mounted
				box.SandboxLabels[assets.LabelWritable] = "{}"
				box.SandboxMounts = tc.mounts
				if tc.configured {
					b.Cfg.Roots = []config.Root{{Name: "p", Host: b.tmp, Container: "/work"}}
				} else {
					b.Cfg.Roots = []config.Root{}
				}
				var mountsDrift []string
				for _, d := range b.restartDrift() {
					if strings.Contains(d, "mounts ") {
						mountsDrift = append(mountsDrift, d)
					}
				}
				c := &checkup{}
				b.doctorContainer(c, true)
				doctor := c.String()
				if len(tc.drift) == 0 {
					if len(mountsDrift) != 0 || strings.Contains(doctor, "the configuration says") {
						t.Errorf("drift %q, doctor:\n%s", mountsDrift, doctor)
					}
				} else {
					if len(mountsDrift) != 1 || !strings.Contains(mountsDrift[0], tc.drift[0]) {
						t.Errorf("restartDrift %q, want one with %q", mountsDrift, tc.drift[0])
					}
					found := false
					for _, r := range c.rows {
						if r.label == "roots" && r.level == levelProblem && strings.Contains(r.text, tc.drift[0]) && strings.HasPrefix(r.fix, "caboose restart") {
							found = true
						}
					}
					if !found {
						t.Errorf("doctor has no problem saying %q, fixed by a restart:\n%s", tc.drift[0], doctor)
					}
				}
				if d := b.writableDrift(); d != "" {
					t.Errorf("writableDrift %q", d)
				}
			})
		}
	}
}

// A sandbox made with no roots records its (empty) writable parts like any
// other: one that records none is said not to, and one that does has no
// drift to say.
func TestNoRootsWritableDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"recorded", map[string]string{assets.LabelWritable: "{}"}, ""},
		{"not recorded", nil, "records no writable parts"},
		// A sandbox whose roots cannot be told is not one with none: the
		// drift is not judged.
		{"roots label null", map[string]string{assets.LabelRoots: "null"}, ""},
		{"roots label unreadable", map[string]string{assets.LabelRoots: "{"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := runningBox(isolationVM)
			b := newBoxApp(t, isolationVM, box)
			box.SandboxLabels[assets.LabelRoots] = "[]"
			maps.Copy(box.SandboxLabels, tc.labels)
			b.Cfg.Roots = []config.Root{}
			got := b.writableDrift()
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("writableDrift %q, want %q", got, tc.want)
			}
		})
	}
}
