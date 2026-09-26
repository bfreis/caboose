package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// finale runs one of the whole run's closing steps with answers.
func (e *setupEnv) finale(step func(*prompter) error, answers string) error {
	e.t.Helper()
	e.errb.Reset()
	return step(newPrompter(strings.NewReader(answers), e.errb))
}

func TestSetupStart(t *testing.T) {
	t.Run("running: left as it is", func(t *testing.T) {
		e := newSetupEnv(t, "default", "", true)
		if err := e.finale(e.a.setupStart, ""); err != nil {
			t.Fatal(err)
		}
		e.wantOut("Container", "Where sessions run: caboose.", "✓ Running")
		if strings.Contains(e.errb.String(), "? ") {
			t.Errorf("asked:\n%s", e.errb)
		}
	})
	t.Run("absent: asked, declined", func(t *testing.T) {
		e := newSetupEnv(t, "default", "", false)
		if err := e.finale(e.a.setupStart, "n\n"); err != nil {
			t.Fatal(err)
		}
		e.wantOut("? Create and start it now? When there is no image, that builds it first, which takes a few minutes. [Y/n] ",
			"· Not created: the first launch creates it")
	})
	t.Run("absent: asked, and failing is a note", func(t *testing.T) {
		e := newSetupEnv(t, "default", "", false)
		if err := e.finale(e.a.setupStart, "\n"); err != nil {
			t.Fatal(err)
		}
		e.wantOut("✗ Not started: cannot inspect image 'caboose'")
	})
	t.Run("roots that do not exist: not started", func(t *testing.T) {
		e := newSetupEnv(t, "default", "", false)
		if err := os.Remove(filepath.Join(e.a.Cfg.Home, "dev")); err != nil {
			t.Fatal(err)
		}
		if err := e.finale(e.a.setupStart, ""); err != nil {
			t.Fatal(err)
		}
		e.wantOut("✗ Not started: repo root " + filepath.Join(e.a.Cfg.Home, "dev") + " does not exist")
	})
	// The roots are read back first: the roots section may have changed
	// them in this very run.
	t.Run("the roots are read back", func(t *testing.T) {
		e := newSetupEnv(t, "default", "", true)
		e.mkdir("src")
		e.writeConfig("repo_root = \"~/src\"\n")
		if err := e.finale(e.a.setupStart, ""); err != nil {
			t.Fatal(err)
		}
		want, _ := config.Physical(filepath.Join(e.a.Cfg.Home, "src"))
		if len(e.a.Cfg.Roots) != 1 || e.a.Cfg.Roots[0].Host != want {
			t.Errorf("roots %+v", e.a.Cfg.Roots)
		}
	})
}

func TestSetupLogin(t *testing.T) {
	loggedIn := func(e *setupEnv) {
		e.t.Helper()
		p := filepath.Join(e.a.Cfg.DataDir, datadir.Credentials)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	// inRoot runs the step from a directory under the configured root,
	// with the container running.
	inRoot := func(t *testing.T) *setupEnv {
		t.Helper()
		e := newSetupEnv(t, "default", "", true)
		root, _ := config.Physical(filepath.Join(e.a.Cfg.Home, "dev"))
		e.a.Cfg.Roots = []config.Root{{Host: root, Container: config.WorkDir}}
		e.mkdir("dev/proj")
		t.Chdir(filepath.Join(root, "proj"))
		return e
	}

	t.Run("logged in", func(t *testing.T) {
		e := inRoot(t)
		loggedIn(e)
		if err := e.finale(e.a.setupLogin, ""); err != nil {
			t.Fatal(err)
		}
		e.wantOut("✓ Logged in")
	})
	t.Run("not logged in, here in a project: a session, when asked", func(t *testing.T) {
		e := inRoot(t)
		var attached []string
		called := false
		e.a.attach = func(args []string) error { called, attached = true, args; return nil }
		if err := e.finale(e.a.setupLogin, "\n"); err != nil {
			t.Fatal(err)
		}
		if !called || attached != nil {
			t.Errorf("attach called %v with %v", called, attached)
		}
		e.wantOut("? Not logged in yet. Start a session here, which asks you to log in? [Y/n] ")
	})
	t.Run("not logged in, declined", func(t *testing.T) {
		e := inRoot(t)
		e.a.attach = func([]string) error { t.Error("attached"); return nil }
		if err := e.finale(e.a.setupLogin, "n\n"); err != nil {
			t.Fatal(err)
		}
		e.wantOut("· Run caboose in a project under ")
	})
	t.Run("not logged in, outside every root", func(t *testing.T) {
		e := inRoot(t)
		t.Chdir(t.TempDir())
		e.a.attach = func([]string) error { t.Error("attached"); return nil }
		if err := e.finale(e.a.setupLogin, ""); err != nil {
			t.Fatal(err)
		}
		e.wantOut("! Not logged in yet. Run caboose in a project under ")
	})
	t.Run("not logged in, no container", func(t *testing.T) {
		e := newSetupEnv(t, "default", "", false)
		e.a.attach = func([]string) error { t.Error("attached"); return nil }
		if err := e.finale(e.a.setupLogin, ""); err != nil {
			t.Fatal(err)
		}
		e.wantOut("! Not logged in yet. Run caboose in a project under ")
	})
	t.Run("a link where the login is", func(t *testing.T) {
		e := inRoot(t)
		p := filepath.Join(e.a.Cfg.DataDir, datadir.Credentials)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/hostname", p); err != nil {
			t.Fatal(err)
		}
		if err := e.finale(e.a.setupLogin, ""); err != nil {
			t.Fatal(err)
		}
		e.wantOut("! Cannot tell whether there is a login: ~/.claude/.credentials.json is not a plain file")
	})
}
