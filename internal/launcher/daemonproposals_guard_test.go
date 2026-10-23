package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// dockerfileWorld is alpha under a dockerfile profile whose Dockerfile is
// df, and its Dockerfile's path.
func dockerfileWorld(t *testing.T, df string) (*proposalsWorld, string) {
	w := newProposalsWorld(t, "")
	if err := os.WriteFile(w.configPath(), []byte("image = \"dockerfile.default\"\n\n[roots]\ndev = \"~/dev\"\n\n[dockerfile.default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := config.DockerfileDir(config.EnvDirFor(w.home, "alpha"), "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	return w, path
}

func sectionProposal(df string) string {
	return "title = \"Install foo\"\ndockerfile_sha256 = \"" + proposal.Hash([]byte(df)) + "\"\n" + fooSection
}

// onlyEntry is alpha's one proposal file, and its App.
func (w *proposalsWorld) onlyEntry() (*App, proposal.Entry) {
	w.t.Helper()
	app, err := w.l.newApp("alpha")
	if err != nil {
		w.t.Fatal(err)
	}
	entries, err := proposal.List(app.Cfg.DataDir)
	if err != nil || len(entries) != 1 {
		w.t.Fatalf("%v %v", entries, err)
	}
	return app, entries[0]
}

// A proposal the list could not show whole is not applied from the page,
// though its plan alone would allow it.
func TestDaemonApplyRefusesWhatWasNotShown(t *testing.T) {
	df := smallDockerfile + "# " + strings.Repeat("x", 70<<10) + "\n"
	w, path := dockerfileWorld(t, df)
	p := w.propose("foo.toml", sectionProposal(df))
	app, e := w.onlyEntry()
	if v, err := app.ProposalView(e); err != nil || !v.WebApply {
		t.Fatalf("the plan alone should allow it: %+v %v", v.WebApplyWhy, err)
	}
	info := w.list()[0]
	if info.WebApply || len(info.Parts) != 1 || len(info.Parts[0].Diff) != 0 {
		t.Fatalf("listed as appliable, or with its diff: %+v", info)
	}
	if _, err := w.apply("foo.toml", info.Hash, ""); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("applied what the list could not show: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != df {
		t.Error("the Dockerfile was written")
	}
	there(t, p)
}

// A proposal that changes more than one thing is applied only in a
// terminal, so that no file changed between two writes leaves it half
// applied.
func TestDaemonApplyRefusesMoreThanOnePart(t *testing.T) {
	w, path := dockerfileWorld(t, smallDockerfile)
	p := w.propose("both.toml", sectionProposal(smallDockerfile)+"\n[roots]\nother = \"~/src/other\"\n")
	info := w.list()[0]
	if len(info.Parts) != 2 || len(info.Refusals) != 0 || info.WebApply || !strings.Contains(info.WebApplyWhy, "caboose -e alpha apply") {
		t.Fatalf("%+v", info)
	}
	before := w.readConfig()
	if _, err := w.apply("both.toml", info.Hash, "other"); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("a proposal of two parts: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != smallDockerfile {
		t.Error("the Dockerfile was written")
	}
	if w.readConfig() != before {
		t.Error("config.toml was written")
	}
	there(t, p)
}

// What ApplyPlanned writes is checked once more just before the write: a
// Dockerfile or config.toml changed after the plan was confirmed is left
// as it is.
func TestApplyPlannedChecksAgainBeforeWriting(t *testing.T) {
	t.Run("section", func(t *testing.T) {
		w, path := dockerfileWorld(t, smallDockerfile)
		p := w.propose("foo.toml", sectionProposal(smallDockerfile))
		app, e := w.onlyEntry()
		v, err := app.ProposalView(e)
		if err != nil || !v.WebApply {
			t.Fatalf("%+v %v", v.WebApplyWhy, err)
		}
		changed := smallDockerfile + "RUN true\n"
		app.beforeProposalWrite = func() {
			if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := app.ApplyPlanned(e, v.Hash, ""); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
			t.Errorf("the Dockerfile changed: %v", err)
		}
		if b, _ := os.ReadFile(path); string(b) != changed {
			t.Errorf("the Dockerfile was written:\n%s", b)
		}
		there(t, p)
	})
	t.Run("root", func(t *testing.T) {
		w := newProposalsWorld(t, "")
		p := w.propose("other.toml", otherRoot)
		app, e := w.onlyEntry()
		v, err := app.ProposalView(e)
		if err != nil || !v.WebApply {
			t.Fatalf("%+v %v", v.WebApplyWhy, err)
		}
		app.beforeProposalWrite = func() { w.config("[session]\nauto_sync = true\n") }
		if _, err := app.ApplyPlanned(e, v.Hash, "other"); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
			t.Errorf("config.toml changed: %v", err)
		}
		if c := w.readConfig(); strings.Contains(c, "other") || !strings.Contains(c, "auto_sync") {
			t.Errorf("config.toml:\n%s", c)
		}
		there(t, p)
	})
	t.Run("unchanged", func(t *testing.T) {
		w := newProposalsWorld(t, "")
		w.propose("other.toml", otherRoot)
		app, e := w.onlyEntry()
		v, _ := app.ProposalView(e)
		called := false
		app.beforeProposalWrite = func() { called = true }
		if _, err := app.ApplyPlanned(e, v.Hash, "other"); err != nil || !called {
			t.Fatalf("%v, hook called %v", err, called)
		}
		if !strings.Contains(w.readConfig(), "other") {
			t.Errorf("not written:\n%s", w.readConfig())
		}
	})
}

// Deleting takes the hash the list showed: a proposal rewritten since is
// not deleted unseen.
func TestDaemonDeleteTakesTheShownHash(t *testing.T) {
	w := newProposalsWorld(t, "")
	p := w.propose("other.toml", otherRoot)
	h := w.list()[0].Hash
	w.propose("other.toml", strings.Replace(otherRoot, "its docs", "something else", 1))
	if err := w.l.ProposalDelete(context.Background(), "alpha", "other.toml", h); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("deleted a rewritten proposal: %v", err)
	}
	there(t, p)
	if err := w.l.ProposalDelete(context.Background(), "alpha", "other.toml", w.list()[0].Hash); err != nil {
		t.Fatal(err)
	}
	gone(t, p)
}

// ApplyPlanned itself refuses a hash that is not its plan's, and a name
// typed wrong, whoever calls it.
func TestApplyPlannedRefusesAnotherHashOrName(t *testing.T) {
	w := newProposalsWorld(t, "")
	p := w.propose("other.toml", otherRoot)
	app, e := w.onlyEntry()
	v, err := app.ProposalView(e)
	if err != nil || !v.WebApply {
		t.Fatalf("%+v %v", v.WebApplyWhy, err)
	}
	if _, err := app.ApplyPlanned(e, strings.Repeat("0", 64), "other"); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("another hash: %v", err)
	}
	if _, err := app.ApplyPlanned(e, v.Hash, "othe"); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("a wrong name: %v", err)
	}
	if strings.Contains(w.readConfig(), "other") {
		t.Errorf("written:\n%s", w.readConfig())
	}
	there(t, p)
}
