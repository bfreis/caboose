package launcher

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

func TestLinkLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	if held, err := linkRunning(dir); err != nil || held {
		t.Fatalf("fresh dir: held %v, err %v", held, err)
	}
	unlock, err := lockLink(dir)
	if err != nil {
		t.Fatal(err)
	}
	if held, _ := linkRunning(dir); !held {
		t.Fatal("a held lock reads as free")
	}
	if _, err := lockLink(dir); err == nil {
		t.Fatal("locked twice")
	}
	unlock()
	if held, _ := linkRunning(dir); held {
		t.Fatal("a released lock reads as held")
	}
}

func TestLinkRefusesBadSettings(t *testing.T) {
	cases := map[string]config.Config{
		"forward_ports": {ForwardPorts: "3000-", OpenURLs: "ask"},
		"open_urls":     {ForwardPorts: "3000", OpenURLs: "sometimes"},
	}
	for want, cfg := range cases {
		cfg.DataDir = t.TempDir()
		var stderr bytes.Buffer
		a := &App{Cfg: &cfg, Stderr: &stderr}
		err := a.Link(nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	a := &App{Cfg: &config.Config{ForwardPorts: "3000", OpenURLs: "ask", DataDir: t.TempDir()}}
	if err := a.Link([]string{"--now"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("a stray argument: %v", err)
	}
}

// A launch starts no second helper while one holds the lock.
func TestStartLinkOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	var spawned [][]string
	a := &App{
		Cfg:        &config.Config{Env: "default", DataDir: dir},
		Executable: func() (string, error) { return "/bin/caboose", nil },
		Spawn: func(exe string, args ...string) error {
			spawned = append(spawned, append([]string{exe}, args...))
			return nil
		},
	}
	a.startLink()
	if len(spawned) != 1 || strings.Join(spawned[0], " ") != "/bin/caboose --env default link --background" {
		t.Fatalf("spawned %v", spawned)
	}
	unlock, err := lockLink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a.startLink()
	if len(spawned) != 1 {
		t.Fatalf("spawned again while a link runs: %v", spawned)
	}
}

// fakeHelper holds the link lock as a running helper would, with state
// saying it runs with settings; linkKill "stops" it by releasing the lock.
func fakeHelper(t *testing.T, dir string, settings linkSettings) (killed *[]int) {
	t.Helper()
	unlock, err := lockLink(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLinkState(dir, linkState{PID: 4242, linkSettings: settings}); err != nil {
		t.Fatal(err)
	}
	var k []int
	orig := linkKill
	linkKill = func(pid int, sig syscall.Signal) error {
		k = append(k, pid)
		unlock()
		return nil
	}
	t.Cleanup(func() { linkKill = orig; unlock() })
	return &k
}

func spawnRecorder(a *App) *[][]string {
	var spawned [][]string
	a.Executable = func() (string, error) { return "/bin/caboose", nil }
	a.Spawn = func(exe string, args ...string) error {
		spawned = append(spawned, append([]string{exe}, args...))
		return nil
	}
	return &spawned
}

// A launch whose settings differ from the running helper's -- a variable
// set in that shell, say -- replaces it; one with the same leaves it be.
func TestStartLinkReplacesAHelperWithOtherSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		running  linkSettings
		replaced bool
	}{
		{"same settings", linkSettings{"3000", "ask"}, false},
		{"other ports", linkSettings{"4000", "ask"}, true},
		{"other open_urls", linkSettings{"3000", "off"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			killed := fakeHelper(t, dir, tc.running)
			a := &App{Cfg: &config.Config{Env: "default", DataDir: dir, ForwardPorts: "3000", OpenURLs: "ask"}}
			spawned := spawnRecorder(a)
			a.startLink()
			if got := len(*spawned) == 1 && len(*killed) == 1 && (*killed)[0] == 4242; got != tc.replaced {
				t.Fatalf("killed %v, spawned %v; want replaced %v", *killed, *spawned, tc.replaced)
			}
		})
	}
}

func TestRestartLink(t *testing.T) {
	dir := t.TempDir()
	killed := fakeHelper(t, dir, linkSettings{"3000", "ask"})
	var stderr bytes.Buffer
	a := &App{Cfg: &config.Config{Env: "default", DataDir: dir, Container: "c", ForwardPorts: "3000", OpenURLs: "ask"},
		Docker: &docker.CLI{Path: "/nonexistent/docker"}, Stderr: &stderr}
	spawned := spawnRecorder(a)
	if err := a.Link([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	if len(*killed) != 1 || len(*spawned) != 1 || !strings.Contains(stderr.String(), "stopped the running link") {
		t.Fatalf("killed %v, spawned %v, said %q", *killed, *spawned, stderr.String())
	}
	// With nothing running, it only starts one.
	*spawned = nil
	stderr.Reset()
	if err := a.Link([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	if len(*spawned) != 1 || strings.Contains(stderr.String(), "stopped") {
		t.Fatalf("spawned %v, said %q", *spawned, stderr.String())
	}
}

// An edit to config.toml reaches the running helper: new settings end the
// session, which reconnects with them; a broken edit keeps the old ones.
func TestReloadOnConfigChange(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home, "CABOOSE_HOME": home}
	getenv := func(k string) string { return env[k] }
	cfg, err := config.Load(getenv, config.OSFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.EnvDir, 0o755)
	os.MkdirAll(cfg.DataDir, 0o755)
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(cfg.EnvDir, config.FileName), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &linkRunner{a: &App{Cfg: cfg}, log: log.New(io.Discard, "", 0), settings: settingsOf(cfg)}
	sess := agentproto.NewSession(bytes.NewReader(nil), nopCloser{io.Discard}, true)
	r.sess = sess

	write("forward_ports = \"4000-4100\"\nopen_urls = \"off\"\n")
	r.reload()
	if r.settings != (linkSettings{"4000-4100", "off"}) || !r.cfg.Ports.Has(4050) || r.cfg.OpenURL != "off" {
		t.Fatalf("settings %+v after the edit", r.settings)
	}
	select {
	case <-sess.Done():
	default:
		t.Fatal("the session goes on with the old settings")
	}
	if st, ok := readLinkState(cfg.DataDir); !ok || st.linkSettings != r.settings {
		t.Fatalf("link.json %+v", st)
	}

	write("forward_ports = \"4000-\"\n")
	r.reload()
	if r.settings != (linkSettings{"4000-4100", "off"}) {
		t.Fatalf("a broken edit replaced the settings: %+v", r.settings)
	}

	// A variable the helper was started with still wins over the file.
	env["CABOOSE_FORWARD_PORTS"] = "5000"
	write("forward_ports = \"6000\"\n")
	r.reload()
	if r.settings.ForwardPorts != "5000" {
		t.Fatalf("forward_ports %q, want the variable's", r.settings.ForwardPorts)
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
