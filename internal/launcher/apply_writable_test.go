package launcher

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// writableRefusals are checkProposedWritable's refusals for path, one
// string.
func writableRefusals(e *setupEnv, path string) string {
	_, refusals, _ := e.a.checkProposedWritable(proposal.Writable{Path: path})
	return strings.Join(refusals, "\n")
}

func symlink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}

func TestProposedWritableRefusals(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	tmp := filepath.Dir(home)
	for _, d := range []string{"dev/a/b/c", "dev/ok", "dev/x/y/z", "dev/w/q", "dev/whole/p", "elsewhere", ".ssh/keys", "Library/Keychains"} {
		e.mkdir(d)
	}
	if err := os.WriteFile(filepath.Join(home, "dev", "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A symbolic link at each component of the path, the first, a middle
	// one and the last, each to a real directory.
	symlink(t, filepath.Join(home, "elsewhere"), filepath.Join(home, "dev", "l1"))
	symlink(t, filepath.Join(home, "elsewhere"), filepath.Join(home, "dev", "a", "l2"))
	symlink(t, filepath.Join(home, "dev", "ok"), filepath.Join(home, "dev", "a", "b", "l3"))
	if err := os.MkdirAll(filepath.Join(e.a.Cfg.DataDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.writeConfig("[roots]\n" +
		"dev = { host = \"~/dev\", writable = [\"w\"] }\n" +
		"whole = { host = \"~/dev/whole\", path = \"/work/whole\", writable = [\".\"] }\n" +
		"h = { host = \"~\", path = \"/work/h\" }\n" +
		"t = { host = \"" + tmp + "\", path = \"/opt/t\" }\n")

	for _, tc := range []struct{ path, want string }{
		{"/work/nothing/x", "not inside a root the container mounts"},
		{"/work/dev/nope", "does not exist"},
		{"/work/dev/file", "is not a directory"},
		{"/work/dev/l1", "dev/l1 is a symbolic link"},
		{"/work/dev/l1/x", "dev/l1 is a symbolic link"},
		{"/work/dev/a/l2/x", "dev/a/l2 is a symbolic link"},
		{"/work/dev/a/b/l3", "dev/a/b/l3 is a symbolic link"},
		{"/work/dev/w", "writable already: the root dev's writable part w holds it"},
		{"/work/dev/w/q", "writable already: the root dev's writable part w holds it"},
		{"/work/whole/p", "writable already: the whole root whole is"},
		{"/work/whole", "writable already: the whole root whole is"},
		{"/work/h", "your home directory, or holds it"},
		{"/work/h/.ssh", "is in ~/.ssh"},
		{"/work/h/.ssh/keys", "is in ~/.ssh"},
		{"/work/h/Library/Keychains", "is in ~/Library"},
		{"/work/dev/ok", ""},
		{"/work/dev/a/b/c", ""},
		{"/work/dev", ""},
	} {
		got := writableRefusals(e, tc.path)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusals %q, want %q", tc.path, got, tc.want)
		}
	}
	// /opt/t is outside /work, which a proposal cannot name; asked of the
	// host check directly, a root holding the home and caboose's state is
	// refused for both.
	got := writableRefusals(e, "/opt/t")
	for _, want := range []string{"your home directory, or holds it", "is in, is or holds caboose's home"} {
		if !strings.Contains(got, want) {
			t.Errorf("/opt/t: refusals %q, want %q", got, want)
		}
	}
	for _, p := range []string{"/opt/t/.caboose", "/opt/t/.caboose/envs", e.a.Cfg.DataDir + "/sub"} {
		p = strings.Replace(p, tmp, "/opt/t", 1)
		if got := writableRefusals(e, p); !strings.Contains(got, "is in, is or holds caboose's home") {
			t.Errorf("%s: refusals %q", p, got)
		}
	}
}

// CABOOSE_HOME under a second name: a symlink to a directory the
// proposal would make writable, CABOOSE_HOME below it and not made yet.
// No comparison of the path as written sees it; its nearest ancestor's
// identity does.
func TestProposedWritableRefusesCabooseHomeByIdentity(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	e.mkdir("dev/p")
	e.mkdir("other")
	symlink(t, filepath.Join(home, "dev", "p"), filepath.Join(home, "other", "second"))
	e.a.Cfg.CabooseHome = filepath.Join(home, "other", "second", "caboose")
	e.a.Cfg.EnvDir, e.a.Cfg.DataDir = "", ""
	if got := writableRefusals(e, "/work/dev/p"); !strings.Contains(got, "is in, is or holds caboose's home") {
		t.Errorf("refusals %q", got)
	}
	// And one inside it, by a second name for a directory above it.
	e.mkdir("dev/p/caboose/envs")
	if got := writableRefusals(e, "/work/dev/p/caboose/envs"); !strings.Contains(got, "is in, is or holds caboose's home") {
		t.Errorf("inside: refusals %q", got)
	}
}

// The path is the sandbox's, mapped through what the sandbox mounts: a
// root mounted elsewhere than config.toml now says is found where the
// session sees it.
func TestProposedWritableMapsThroughTheMounts(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/p")
	e.writeConfig("[roots]\ndev = \"~/dev\"\n")
	e.a.Backend = sandbox([]config.Root{{Host: physicalOr(filepath.Join(e.a.Cfg.Home, "dev")), Container: "/work/code"}})
	pl, refusals, _ := e.a.checkProposedWritable(proposal.Writable{Path: "/work/code/p"})
	if len(refusals) > 0 || pl == nil || pl.root.Name != "dev" || pl.part != "p" {
		t.Fatalf("%+v %q", pl, refusals)
	}
	if got := writableRefusals(e, "/work/dev/p"); !strings.Contains(got, "not inside a root the container mounts") {
		t.Errorf("config.toml's path, which the sandbox does not mount: %q", got)
	}
}

// The new list: the parts the new one holds go, the rest stay, and the
// whole root is ".".
func TestProposedWritableList(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/p/q")
	e.mkdir("dev/r")
	e.writeConfig("[roots]\ndev = { host = \"~/dev\", writable = [\"p/q\", \"r\"] }\n")
	pl, refusals, warnings := e.a.checkProposedWritable(proposal.Writable{Path: "/work/dev/p"})
	if len(refusals) > 0 {
		t.Fatal(refusals)
	}
	if w := pl.edit.Roots["dev"].Writable; !slices.Equal(w, []string{"r", "p"}) || !slices.Equal(pl.dropped, []string{"p/q"}) {
		t.Errorf("writable %q, dropped %q", w, pl.dropped)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "(p/q), which it replaces") {
		t.Errorf("warnings %q", warnings)
	}
	pl, refusals, _ = e.a.checkProposedWritable(proposal.Writable{Path: "/work/dev"})
	if len(refusals) > 0 || !slices.Equal(pl.edit.Roots["dev"].Writable, []string{"."}) {
		t.Errorf("%+v %q", pl, refusals)
	}
}

const writableP = "title = \"Write p\"\nreason = \"its build\"\n[writable]\npath = \"/work/dev/p\"\n"

// caboose apply shows what becomes writable, asks for the root's name,
// and edits config.toml line by line: the comments outside [roots] stay
// where they are, and those in it are kept.
func TestApplyWritable(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/p")
	e.mkdir("src/other")
	if err := os.WriteFile(filepath.Join(e.a.Cfg.Home, "dev", "p", ".env"), []byte("T=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.writeConfig("# my roots\n[roots]\n# the projects\ndev = \"~/dev\"\nother = \"~/src/other\"\n")
	p := e.propose("p.toml", writableP)
	before := e.configFile()

	if err := e.apply("1\ndve\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("/work/dev/p", "change or delete anything under", "next restart", "credentials or keys: .env", "That is not dev")
	if e.configFile() != before {
		t.Fatalf("written on a wrong name:\n%s", e.configFile())
	}
	if err := e.apply("\n"); err != nil || !e.said("Left pending") || e.configFile() != before {
		t.Fatalf("Enter applied it: %v\n%s", err, e.errb)
	}
	there(t, p)

	if err := e.apply("1\ndev\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("is writable from the container's next restart")
	// [roots] is rewritten in place, as every edit of the roots is: its
	// comment lines kept, after its keys.
	want := "# my roots\n[roots]\ndev = { host = \"~/dev\", writable = [\"p\"] }\nother = \"~/src/other\"\n# the projects\n"
	if got := e.configFile(); got != want {
		t.Errorf("config.toml:\n%s\nwant:\n%s", got, want)
	}
	gone(t, p)
}

// The view is what apply shows, its hash covers the host path, and the
// page applies it only with the root's name typed and as it was shown: a
// symlink swapped in after the view makes it stale.
func TestProposalViewWritable(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/p")
	e.mkdir("elsewhere")
	e.writeConfig("[roots]\ndev = \"~/dev\"\n")
	e.propose("p.toml", writableP)
	v := onlyView(t, e)
	if !v.WebApply || v.Confirm != ConfirmTypeName || v.ConfirmName != "dev" || len(v.Parts) != 1 || v.Parts[0].Kind != PartWritable {
		t.Fatalf("%+v", v)
	}
	host := physicalOr(filepath.Join(e.a.Cfg.Home, "dev", "p"))
	if s := v.Parts[0].Summary; !strings.Contains(s, "make /work/dev/p writable") || !strings.Contains(s, e.a.short(host)) {
		t.Errorf("summary %q", s)
	}
	if !slices.Contains(v.Parts[0].Diff, `+ dev = { host = "~/dev", writable = ["p"] }`) {
		t.Errorf("diff %q", v.Parts[0].Diff)
	}
	entries, _ := proposal.List(e.a.Cfg.DataDir)

	// A wrong name, and a stale hash, write nothing.
	before := e.configFile()
	if _, err := e.a.ApplyPlanned(entries[0], v.Hash, "dve"); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("a wrong name: %v", err)
	}
	if _, err := e.a.ApplyPlanned(entries[0], strings.Repeat("0", 64), "dev"); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("a stale hash: %v", err)
	}
	// p swapped for a symlink to somewhere else after the view.
	pDir := filepath.Join(e.a.Cfg.Home, "dev", "p")
	if err := os.Rename(pDir, pDir+".was"); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(e.a.Cfg.Home, "elsewhere"), pDir)
	if _, err := e.a.ApplyPlanned(entries[0], v.Hash, "dev"); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("swapped for a symlink: %v", err)
	}
	if e.configFile() != before {
		t.Fatalf("written:\n%s", e.configFile())
	}
	// Put back, it applies.
	if err := os.Remove(pDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pDir+".was", pDir); err != nil {
		t.Fatal(err)
	}
	notes, err := e.a.ApplyPlanned(entries[0], v.Hash, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Join(notes, "\n"); !strings.Contains(n, "writable takes effect at the container's next restart") {
		t.Errorf("notes %q", notes)
	}
	f, err := config.ParseFile("config.toml", []byte(e.configFile()))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Roots["dev"].Writable, []string{"p"}) {
		t.Errorf("roots %+v", f.Roots)
	}
}

// The daemon serves a writable part as the protocol carries it, and
// applies it with the root's name typed.
func TestDaemonApplyWritable(t *testing.T) {
	w := newProposalsWorld(t, "")
	if err := os.MkdirAll(filepath.Join(w.user, "dev", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := w.propose("p.toml", writableP)
	ps := w.list()
	if len(ps) != 1 || !ps[0].WebApply || len(ps[0].Parts) != 1 || ps[0].Parts[0].Kind != daemonproto.ProposalPartWritable ||
		ps[0].Confirm.Name != "dev" {
		t.Fatalf("%+v", ps)
	}
	if _, err := w.apply("p.toml", ps[0].Hash, "p"); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("the path typed, not the root's name: %v", err)
	}
	there(t, p)
	if _, err := w.apply("p.toml", ps[0].Hash, "dev"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.readConfig(), `dev = { host = "~/dev", writable = ["p"] }`) {
		t.Errorf("config.toml:\n%s", w.readConfig())
	}
	gone(t, p)
}

// The home's hidden directories and Library are judged by identity: a
// directory reached under a second name for the home is the home's.
func TestProposedWritableHomeByIdentity(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	home := e.a.Cfg.Home
	tmp := filepath.Dir(home)
	e.mkdir(".ssh/keys")
	e.mkdir("library/x")
	e.mkdir("dev/p")
	if err := os.MkdirAll(filepath.Join(tmp, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(tmp, "other", "second")
	symlink(t, home, second)
	for _, p := range []string{filepath.Join(second, ".ssh"), filepath.Join(second, ".ssh", "keys"), filepath.Join(second, "library", "x")} {
		if _, ok := e.a.homeEntryOf(p); !ok {
			t.Errorf("%s: not seen as in the home", p)
		}
	}
	if name, ok := e.a.homeEntryOf(filepath.Join(second, "dev", "p")); ok {
		t.Errorf("dev/p seen as in ~/%s", name)
	}
	// The home configured through a symlink to it, CABOOSE_HOME outside
	// it, and a root at the home under its real name.
	e.writeConfig("[roots]\nh = { host = \"" + home + "\", path = \"/work/h\" }\n")
	e.a.Cfg.Home = second
	for _, tc := range []struct{ path, want string }{
		{"/work/h/.ssh/keys", "is in ~/.ssh"},
		{"/work/h/library/x", "is in ~/library"},
		{"/work/h/dev/p", ""},
	} {
		got := writableRefusals(e, tc.path)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refusals %q, want %q", tc.path, got, tc.want)
		}
	}
}

// After the name is typed, apply plans again from what is there: a
// directory swapped for a link, or config.toml edited, since it was
// shown is not written.
func TestApplyWritablePlansAgain(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.mkdir("dev/p")
	e.mkdir("elsewhere")
	e.writeConfig("[roots]\ndev = \"~/dev\"\n")
	p := e.propose("p.toml", writableP)
	pDir := filepath.Join(e.a.Cfg.Home, "dev", "p")

	e.a.beforeProposalWrite = func() {
		if err := os.Rename(pDir, pDir+".was"); err != nil {
			t.Fatal(err)
		}
		symlink(t, filepath.Join(e.a.Cfg.Home, "elsewhere"), pDir)
	}
	before := e.configFile()
	if err := e.apply("1\ndev\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("is a symbolic link", "It changed since it was shown")
	if e.configFile() != before {
		t.Fatalf("written after a swap:\n%s", e.configFile())
	}
	there(t, p)
	if err := os.Remove(pDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pDir+".was", pDir); err != nil {
		t.Fatal(err)
	}

	edited := "[roots]\ndev = \"~/dev\"\n\n[session]\ntmux = true\n"
	e.a.beforeProposalWrite = func() {
		if err := os.WriteFile(e.configPath(), []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.apply("1\ndev\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("config.toml changed since it was shown")
	if e.configFile() != edited {
		t.Fatalf("written over an edit:\n%s", e.configFile())
	}
	there(t, p)
}
