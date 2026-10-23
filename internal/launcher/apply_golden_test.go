package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/proposal"
)

// applyNow is the time the golden apply runs see, and proposed is when
// their proposals were written: an hour before.
var (
	applyNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	proposed = applyNow.Add(-time.Hour)
)

// goldenApply runs caboose apply with answers on e and compares what it
// said with testdata/golden/apply-name.out and .err, the test's temp dir
// written TMP.
func goldenApply(t *testing.T, e *setupEnv, name, answers string) {
	t.Helper()
	e.a.Now = func() time.Time { return applyNow }
	dir := filepath.Join(e.a.Cfg.DataDir, proposal.Dir)
	ents, _ := os.ReadDir(dir)
	for _, ent := range ents {
		if !ent.IsDir() {
			if err := os.Chtimes(filepath.Join(dir, ent.Name()), proposed, proposed); err != nil {
				t.Fatal(err)
			}
		}
	}
	out := e.a.Stdout.(interface {
		String() string
		Reset()
	})
	out.Reset()
	err := e.apply(answers)
	errs := e.errb.String()
	if err != nil {
		errs += "error: " + err.Error() + "\n"
	}
	tmp := filepath.Dir(e.a.Cfg.Home)
	goldenFile(t, "apply-"+name+".out", strings.ReplaceAll(out.String(), tmp, "TMP"))
	goldenFile(t, "apply-"+name+".err", strings.ReplaceAll(errs, tmp, "TMP"))
}

// goldenTmpLen is how long the golden runs' temp dir is: apply wraps its
// lines at 80 columns, so a path's length moves the words after it.
const goldenTmpLen = 120

// newGoldenEnv is newSetupEnv in a temp dir of goldenTmpLen bytes, its
// physical path, padded with a directory of its own.
func newGoldenEnv(t *testing.T, env string) *setupEnv {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pad := goldenTmpLen - len(tmp) - 1
	if pad < 1 {
		t.Skipf("the temp dir %s is too long for the golden files", tmp)
	}
	tmp = filepath.Join(tmp, strings.Repeat("p", pad))
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	return newSetupEnvIn(t, tmp, env, "", false)
}

// goldenFile compares got with testdata/golden/name, or rewrites it.
func goldenFile(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if os.Getenv(updateGolden) == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from its golden file:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// smallDockerfile is a dockerfile profile's own Dockerfile, as a user
// would have it.
const smallDockerfile = "FROM debian:13\nRUN apt-get update\n"

// caboose apply prints exactly what the golden files hold, for each kind
// of proposal and for those it cannot apply.
func TestApplyGolden(t *testing.T) {
	t.Run("section-dockerfile", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		e.useDockerfile()
		dir := e.a.Cfg.ImageProfile.Dir
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(smallDockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
		e.propose("foo.toml", "title = \"Install foo\"\nreason = \"for the docs\"\ndockerfile_sha256 = \""+proposal.Hash([]byte(smallDockerfile))+"\"\n"+fooSection)
		goldenApply(t, e, "section-dockerfile", "1\nn\n")
	})
	t.Run("section-seed", func(t *testing.T) {
		e := newGoldenEnv(t, "work")
		e.useDockerfile()
		e.propose("foo.toml", "title = \"Install foo\"\nreason = \"for the docs\"\ndockerfile_sha256 = \""+seedHash(t)+"\"\n"+fooSection)
		goldenApply(t, e, "section-seed", "1\nn\n")
	})
	t.Run("root", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		other := e.dir("src/other")
		if err := os.WriteFile(filepath.Join(other, ".env"), []byte("TOKEN=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.propose("other.toml", "title = \"Mount other\"\nreason = \"its docs\"\n[roots]\nother = \"~/src/other\"\n")
		goldenApply(t, e, "root", "1\nother\n")
	})
	t.Run("root-move", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		e.dir("src/other")
		e.writeConfig("[roots.dev]\nhost = \"~/dev\"\npath = \"/work\"\n")
		e.propose("other.toml", "title = \"Mount other\"\n[roots]\nother = \"~/src/other\"\n")
		goldenApply(t, e, "root-move", "1\ny\nother\n")
	})
	t.Run("alias", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		cmd := tool(t, filepath.Join(filepath.Dir(e.a.Cfg.Home), "bin", "awake"))
		e.propose("awake.toml", strings.Replace(awakeProposal, "%s", cmd, 1))
		goldenApply(t, e, "alias", "1\nawake\n")
	})
	t.Run("refused", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		e.mkdir(".ssh")
		e.propose("ssh.toml", "title = \"Mount ssh\"\n[roots]\nssh = \"~/.ssh\"\n")
		goldenApply(t, e, "refused", "2\n")
	})
	t.Run("unparsable", func(t *testing.T) {
		e := newGoldenEnv(t, "default")
		e.propose("junk.toml", "title = \"t\"\n[nope]\nx = 1\n")
		goldenApply(t, e, "unparsable", "2\n")
	})
}
