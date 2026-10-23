package launcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// terminalDiff is the lines of the diff caboose apply showed after the
// header +++ to, its indent taken off.
func terminalDiff(t *testing.T, out, to string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "+++ "+to+"\n")
	if !ok {
		t.Fatalf("no diff to %q in:\n%s", to, out)
	}
	var lines []string
	for _, l := range strings.Split(rest, "\n") {
		if l == "" {
			break
		}
		lines = append(lines, strings.TrimPrefix(l, "    "))
	}
	return lines
}

// What the page is shown of a proposal is what caboose apply shows: the
// same diff, line for line.
func TestProposalViewIsWhatApplyShows(t *testing.T) {
	t.Run("section", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		e.useDockerfile()
		dir := e.a.Cfg.ImageProfile.Dir
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(smallDockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
		e.propose("foo.toml", "title = \"Install foo\"\ndockerfile_sha256 = \""+proposal.Hash([]byte(smallDockerfile))+"\"\n"+fooSection)
		v := onlyView(t, e)
		if err := e.apply("2\n"); err != nil {
			t.Fatal(err)
		}
		if want := terminalDiff(t, e.errb.String(), "with the proposal"); len(v.Parts) != 1 || !slices.Equal(v.Parts[0].Diff, want) {
			t.Errorf("view %q\nterminal %q", v.Parts, want)
		}
		if !v.WebApply || v.Confirm != ConfirmNone || v.Parts[0].Kind != PartSection {
			t.Errorf("%+v", v)
		}
	})
	t.Run("alias", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		cmd := tool(t, filepath.Join(filepath.Dir(e.a.Cfg.Home), "bin", "awake"))
		e.propose("awake.toml", strings.Replace(awakeProposal, "%s", cmd, 1))
		v := onlyView(t, e)
		if err := e.apply("2\n"); err != nil {
			t.Fatal(err)
		}
		if want := terminalDiff(t, e.errb.String(), "with the alias"); len(v.Parts) != 1 || !slices.Equal(v.Parts[0].Diff, want) {
			t.Errorf("view %q\nterminal %q", v.Parts, want)
		}
		if !v.WebApply || v.Confirm != ConfirmTypeName || v.ConfirmName != "awake" {
			t.Errorf("%+v", v)
		}
	})
	t.Run("root move", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		e.dir("src/other")
		e.writeConfig("[roots.dev]\nhost = \"~/dev\"\npath = \"/work\"\n")
		e.propose("other.toml", "title = \"Mount other\"\n[roots]\nother = \"~/src/other\"\n")
		v := onlyView(t, e)
		if !v.WebApply || v.ConfirmName != "other" || len(v.Parts) != 1 {
			t.Fatalf("%+v", v)
		}
		w := strings.Join(v.Warnings, "\n")
		for _, want := range []string{"so it moves to /work/dev", "Projects under ~/dev move from /work/... to /work/dev/...", "becomes readable from the sandbox"} {
			if !strings.Contains(w, want) {
				t.Errorf("no %q in the warnings:\n%s", want, w)
			}
		}
		d := strings.Join(v.Parts[0].Diff, "\n")
		if !strings.Contains(d, `- path = "/work"`) || !strings.Contains(d, `+ other = "~/src/other"`) {
			t.Errorf("diff:\n%s", d)
		}
	})
}

func onlyView(t *testing.T, e *setupEnv) ProposalView {
	t.Helper()
	entries, err := proposal.List(e.a.Cfg.DataDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %v", entries, err)
	}
	v, err := e.a.ProposalView(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// proposalsWorld is one environment, alpha, served by the daemon's links,
// with ~/dev its root and ~/src/other a directory to propose, the home
// apart from CABOOSE_HOME.
type proposalsWorld struct {
	*linksWorld
	l    *DaemonLinks
	user string // the home
}

func newProposalsWorld(t *testing.T, config string) *proposalsWorld {
	w := newLinksWorld(t)
	user := t.TempDir()
	vars := map[string]string{"HOME": user, "CABOOSE_HOME": w.home}
	w.getenv = func(k string) string { return vars[k] }
	w.env("alpha", "absent")
	for _, d := range []string{"dev", "src/other"} {
		if err := os.MkdirAll(filepath.Join(user, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pw := &proposalsWorld{linksWorld: w, l: w.links(filepath.Join(w.home, "daemon")), user: user}
	pw.config(config)
	return pw
}

func (w *proposalsWorld) configPath() string {
	return filepath.Join(config.EnvDirFor(w.home, "alpha"), config.FileName)
}

func (w *proposalsWorld) config(s string) {
	w.t.Helper()
	if err := os.WriteFile(w.configPath(), []byte("[roots]\ndev = \"~/dev\"\n\n"+s), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *proposalsWorld) readConfig() string {
	b, _ := os.ReadFile(w.configPath())
	return string(b)
}

func (w *proposalsWorld) propose(name, data string) string {
	w.t.Helper()
	dir := filepath.Join(w.dataDir("alpha"), proposal.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return p
}

// list is the proposals as the daemon answers them, checked as the
// protocol checks them.
func (w *proposalsWorld) list() []daemonproto.ProposalInfo {
	w.t.Helper()
	r, err := w.l.Proposals(context.Background(), "alpha")
	if err != nil {
		w.t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		w.t.Fatalf("the daemon's answer does not validate: %v", err)
	}
	return r.Proposals
}

func (w *proposalsWorld) apply(file, hash, typed string) (*daemonproto.ProposalApplyResult, error) {
	p := daemonproto.ProposalApplyParams{Env: "alpha", File: file, Hash: hash, Typed: typed}
	if err := p.Validate(); err != nil {
		w.t.Fatal(err)
	}
	r, err := w.l.ProposalApply(context.Background(), p)
	if err == nil {
		if verr := r.Validate(); verr != nil {
			w.t.Fatalf("the daemon's answer does not validate: %v", verr)
		}
	}
	return r, err
}

func gone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("%s is still there: %v", p, err)
	}
}

func there(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("%s went: %v", p, err)
	}
}

const otherRoot = "title = \"Mount other\"\nreason = \"its docs\"\n[roots]\nother = \"~/src/other\"\n"

// A root is applied from the daemon only with its name typed, and as the
// page was shown it.
func TestDaemonApplyRoot(t *testing.T) {
	w := newProposalsWorld(t, "")
	p := w.propose("other.toml", otherRoot)
	ps := w.list()
	if len(ps) != 1 {
		t.Fatalf("%+v", ps)
	}
	v := ps[0]
	if !v.WebApply || v.Confirm.Kind != daemonproto.ProposalConfirmTypeName || v.Confirm.Name != "other" || v.Title != "Mount other" || v.Reason != "its docs" {
		t.Fatalf("%+v", v)
	}
	if len(v.Parts) != 1 || v.Parts[0].Kind != PartRoot || !slices.Contains(v.Parts[0].Diff, `+ other = "~/src/other"`) ||
		!strings.Contains(v.Parts[0].Summary, "read-only at /work/other") {
		t.Fatalf("%+v", v.Parts)
	}
	before := w.readConfig()
	for _, typed := range []string{"", "othre"} {
		if _, err := w.apply("other.toml", v.Hash, typed); daemonproto.Kind(err) != daemonproto.KindBadRequest || !strings.Contains(err.Error(), "not other") {
			t.Errorf("typed %q: %v", typed, err)
		}
	}
	if w.readConfig() != before {
		t.Fatalf("written on a wrong name:\n%s", w.readConfig())
	}
	there(t, p)
	r, err := w.apply("other.toml", v.Hash, "other")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Applied || !strings.Contains(strings.Join(r.Notes, "\n"), "restart") {
		t.Errorf("%+v", r)
	}
	f, err := config.ParseFile("config.toml", []byte(w.readConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if f.Roots["other"].Host != "~/src/other" || f.Roots["dev"].Host != "~/dev" || len(f.Roots) != 2 {
		t.Errorf("roots %+v", f.Roots)
	}
	gone(t, p)
	// Sessions are told what a proposal is now made against.
	state, _ := os.ReadFile(filepath.Join(w.dataDir("alpha"), proposal.Dir, proposal.CurrentDir, "state.toml"))
	if !strings.Contains(string(state), `host = "~/src/other"`) {
		t.Errorf("state.toml:\n%s", state)
	}
}

// A hash of anything but what would be applied now is stale: config.toml
// edited since, or the proposal itself.
func TestDaemonApplyStale(t *testing.T) {
	w := newProposalsWorld(t, "")
	p := w.propose("other.toml", otherRoot)
	v := w.list()[0]
	w.config("[session]\nauto_sync = true\n")
	if _, err := w.apply("other.toml", v.Hash, "other"); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("config.toml changed: %v", err)
	}
	v = w.list()[0]
	w.propose("other.toml", strings.Replace(otherRoot, "its docs", "its docs, and more", 1))
	if _, err := w.apply("other.toml", v.Hash, "other"); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("the proposal changed: %v", err)
	}
	there(t, p)
	if strings.Contains(w.readConfig(), "other") {
		t.Errorf("written:\n%s", w.readConfig())
	}
	if _, err := w.apply("nope.toml", v.Hash, "other"); daemonproto.Kind(err) != daemonproto.KindNotFound {
		t.Errorf("a proposal that is not there: %v", err)
	}
}

// A section is written into the dockerfile profile's Dockerfile, and the
// image is left to the next restart.
func TestDaemonApplySection(t *testing.T) {
	w := newProposalsWorld(t, "image = \"dockerfile.default\"\n")
	w.config("")
	if err := os.WriteFile(w.configPath(), []byte("image = \"dockerfile.default\"\n\n[roots]\ndev = \"~/dev\"\n\n[dockerfile.default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := config.DockerfileDir(config.EnvDirFor(w.home, "alpha"), "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(smallDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	p := w.propose("foo.toml", "title = \"Install foo\"\ndockerfile_sha256 = \""+proposal.Hash([]byte(smallDockerfile))+"\"\n"+fooSection)
	ps := w.list()
	if len(ps) != 1 || !ps[0].WebApply || ps[0].Confirm.Kind != daemonproto.ProposalConfirmNone || ps[0].NeedsBuild {
		t.Fatalf("%+v", ps)
	}
	if !slices.Contains(ps[0].Parts[0].Diff, "+ # caboose:section foo foo 2.3") {
		t.Errorf("diff %q", ps[0].Parts[0].Diff)
	}
	r, err := w.apply("foo.toml", ps[0].Hash, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Join(r.Notes, "\n"); !strings.Contains(n, "caboose -e alpha restart") {
		t.Errorf("notes %q", r.Notes)
	}
	df, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if !strings.HasPrefix(string(df), smallDockerfile) || !strings.Contains(string(df), "# caboose:section foo foo 2.3\nARG FOO_VERSION=2.3.0\n") {
		t.Errorf("Dockerfile:\n%s", df)
	}
	gone(t, p)
}

// An alias is written as caboose apply writes it, with its name typed.
func TestDaemonApplyAlias(t *testing.T) {
	w := newProposalsWorld(t, "")
	cmd := tool(t, filepath.Join(t.TempDir(), "bin", "awake"))
	p := w.propose("awake.toml", strings.Replace(awakeProposal, "%s", cmd, 1))
	ps := w.list()
	if len(ps) != 1 || !ps[0].WebApply || ps[0].Confirm.Name != "awake" {
		t.Fatalf("%+v", ps)
	}
	if _, err := w.apply("awake.toml", ps[0].Hash, "awak"); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("a wrong name: %v", err)
	}
	if _, err := w.apply("awake.toml", ps[0].Hash, "awake"); err != nil {
		t.Fatal(err)
	}
	f, err := config.ParseFile("config.toml", []byte(w.readConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Aliases["awake"]; !slices.Equal(got.Command, []string{cmd, "-i", "{mode}"}) || got.Policy != "ask" {
		t.Errorf("%+v", got)
	}
	gone(t, p)
}

// What the sandbox wrote comes back as text, printable, apart from the
// host's parts: HTML stays characters, a bidi override or an escape is
// refused by the proposal and said escaped, a long reason whole. A refused
// proposal can be deleted, never applied.
func TestDaemonHostileProposals(t *testing.T) {
	w := newProposalsWorld(t, "")
	long := strings.Repeat("very long reason ", 110)
	w.propose("html.toml", "title = \"<img src=x onerror=alert(1)>\"\nreason = \"<script>alert(1)</script>\\n"+long+"\"\n[roots]\nother = \"~/src/other\"\n")
	w.propose("bidi.toml", "title = \"a\\u202eb\"\n[roots]\nother = \"~/src/other\"\n")
	w.propose("esc\x1b[2J.toml", "junk")
	ssh := w.propose("ssh.toml", "title = \"Mount ssh\"\n[roots]\nssh = \"~/.ssh\"\n")
	if err := os.MkdirAll(filepath.Join(w.user, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	ps := w.list()
	byFile := map[string]daemonproto.ProposalInfo{}
	for _, p := range ps {
		byFile[p.File] = p
	}
	h := byFile["html.toml"]
	if h.Title != "<img src=x onerror=alert(1)>" || !strings.HasPrefix(h.Reason, "<script>alert(1)</script> very long") ||
		!strings.HasSuffix(h.Reason, strings.TrimSpace(long)) || !h.WebApply {
		t.Errorf("html: %+v", h)
	}
	b := byFile["bidi.toml"]
	if b.WebApply || len(b.Refusals) != 1 || !strings.Contains(b.Refusals[0], "U+202E") || b.Title != "" {
		t.Errorf("bidi: %+v", b)
	}
	esc, ok := byFile[`esc\u001B[2J.toml`]
	if !ok || esc.WebApply || len(esc.Refusals) != 1 {
		t.Errorf("escape: %+v", ps)
	}
	s := byFile["ssh.toml"]
	if s.WebApply || len(s.Refusals) == 0 || len(s.Parts) != 0 {
		t.Errorf("ssh: %+v", s)
	}
	if _, err := w.apply("ssh.toml", s.Hash, "ssh"); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("a refused proposal: %v", err)
	}
	there(t, ssh)
	if err := w.l.ProposalDelete(context.Background(), "alpha", "ssh.toml", strings.Repeat("0", 64)); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("deleted by another hash: %v", err)
	}
	there(t, ssh)
	if err := w.l.ProposalDelete(context.Background(), "alpha", "ssh.toml", s.Hash); err != nil {
		t.Fatal(err)
	}
	gone(t, ssh)
	// The escaped name the list showed names the file it stands for.
	if err := w.l.ProposalDelete(context.Background(), "alpha", `esc\u001B[2J.toml`, esc.Hash); err != nil {
		t.Fatal(err)
	}
	if err := w.l.ProposalDelete(context.Background(), "alpha", "ssh.toml", s.Hash); daemonproto.Kind(err) != daemonproto.KindNotFound {
		t.Errorf("deleted twice: %v", err)
	}
	if n := len(w.list()); n != 2 {
		t.Errorf("%d proposals left", n)
	}
}
