package launcher

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/hostlink"
)

// socketApp is an App whose config.toml names body's forwards.
func socketApp(t *testing.T, body string) *App {
	t.Helper()
	home := t.TempDir()
	getenv := func(k string) string { return map[string]string{"HOME": home, "CABOOSE_HOME": home}[k] }
	dir := config.EnvDirFor(home, config.DefaultEnv)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(getenv, config.OSFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &App{Cfg: cfg}
}

const socketConf = "[link.forward_sockets]\napp = \"/tmp/app.sock\"\n"

func TestSocketLinesAndDoctor(t *testing.T) {
	a := socketApp(t, socketConf)
	host := config.SocketHostPath(a.Cfg.EnvDir, "app")
	info := func(reason string) []agentproto.SocketInfo {
		return []agentproto.SocketInfo{{Name: "app", Path: "/tmp/app.sock", HostPath: host, Reason: reason}}
	}
	write := func(s []agentproto.SocketInfo) {
		t.Helper()
		if err := writeLinkState(a.Cfg.DataDir, linkState{linkSettings: settingsOf(a.Cfg), Sockets: s}); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		up          bool
		said        []agentproto.SocketInfo
		line        string
		doctor, fix string
	}{
		"no link":    {false, nil, "(while the link runs)", host, ""},
		"linked":     {true, info(""), host + " reaches /tmp/app.sock", host, ""},
		"not yet":    {true, nil, "not listening yet", "has not served", ""},
		"old agent":  {true, info(hostlink.ReasonSocketOldAgent), "caboose restart", "too old", "caboose restart" + endsSessions},
		"not a sock": {true, info("x is there and is not a socket"), "not a socket", "not a socket", "caboose link --restart"},
	} {
		t.Run(name, func(t *testing.T) {
			write(tc.said)
			lines := a.socketLines(tc.up)
			if len(lines) != 1 || !strings.Contains(lines[0], tc.line) || !strings.Contains(lines[0], host) {
				t.Errorf("status: %q, want %q", lines, tc.line)
			}
			c := &checkup{}
			a.doctorSockets(c, tc.up)
			if len(c.rows) != 1 {
				t.Fatalf("doctor: %+v", c.rows)
			}
			if out := c.String(); !strings.Contains(out, tc.doctor) {
				t.Errorf("doctor: %q, want %q", out, tc.doctor)
			}
			if got := c.rows[0].fix; !strings.Contains(got, tc.fix) || (tc.fix == "") != (got == "") {
				t.Errorf("doctor's fix: %q, want %q", got, tc.fix)
			}
		})
	}
}

// What link.json says for other settings does not count: the running link
// has not read config.toml yet.
func TestSocketViewsIgnoreOtherSettings(t *testing.T) {
	a := socketApp(t, socketConf)
	st := settingsOf(a.Cfg)
	st.ForwardSockets = "app=/old"
	if err := writeLinkState(a.Cfg.DataDir, linkState{linkSettings: st, Sockets: []agentproto.SocketInfo{{Name: "app", Path: "/tmp/app.sock"}}}); err != nil {
		t.Fatal(err)
	}
	if v := a.socketViews(true); len(v) != 1 || v[0].serving {
		t.Errorf("%+v", v)
	}
}

// A path too long says so whether or not a link runs.
func TestDoctorSocketPathTooLong(t *testing.T) {
	a := socketApp(t, socketConf)
	a.Cfg.EnvDir = "/" + strings.Repeat("e", hostlink.MaxSocketPath)
	c := &checkup{}
	a.doctorSockets(c, false)
	if out := c.String(); !strings.Contains(out, "shorter name") || c.rows[0].fix == "" {
		t.Errorf("doctor: %q %+v", out, c.rows)
	}
	if l := a.socketLines(false); len(l) != 1 || !strings.Contains(l[0], "not listening") {
		t.Errorf("status: %q", l)
	}
}

// None configured, nothing is said.
func TestSocketsNoneConfigured(t *testing.T) {
	a := socketApp(t, "")
	c := &checkup{}
	a.doctorSockets(c, true)
	if len(c.rows) != 0 || len(a.socketLines(true)) != 0 || settingsOf(a.Cfg).ForwardSockets != "" {
		t.Errorf("rows %+v", c.rows)
	}
}

// An edit of the sockets reaches the running link: the session ends, and the
// next runs with them, at the fixed place in the environment's directory. A
// bad name keeps what runs.
func TestReloadForwardSockets(t *testing.T) {
	a := socketApp(t, "")
	r := &linkRunner{a: a, log: log.New(io.Discard, "", 0), settings: settingsOf(a.Cfg)}
	sess := agentproto.NewSession(bytes.NewReader(nil), nopCloser{io.Discard}, true)
	r.sess = sess
	path := filepath.Join(a.Cfg.EnvDir, config.FileName)
	if err := os.WriteFile(path, []byte(socketConf), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reload()
	select {
	case <-sess.Done():
	default:
		t.Fatal("the session goes on with the old settings")
	}
	want := hostlink.UnixForward{Name: "app", Sandbox: "/tmp/app.sock", Host: a.Cfg.EnvDir + "/sockets/app.sock"}
	if len(r.cfg.UnixForwards) != 1 || r.cfg.UnixForwards[0] != want {
		t.Fatalf("%+v", r.cfg.UnixForwards)
	}
	if st, ok := readLinkState(a.Cfg.DataDir); !ok || st.ForwardSockets != "app=/tmp/app.sock" {
		t.Fatalf("link.json %+v", st)
	}
	if err := os.WriteFile(path, []byte("[link.forward_sockets]\nBad = \"/s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if len(r.cfg.UnixForwards) != 1 || r.settings.ForwardSockets != "app=/tmp/app.sock" {
		t.Fatalf("a broken edit replaced them: %+v", r.settings)
	}
}

// The runner's wiring, config.toml to link.json: the forwards it is given
// listen, what came of them is written there, and the link's end clears it.
func TestRunnerSocketsReachLinkState(t *testing.T) {
	a := socketApp(t, socketConf)
	r, err := a.newLinkRunner(linkRun{out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	agentSide := agentproto.NewSession(ar, aw, false)
	defer agentSide.Close()
	go agentSide.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, SocketForwards: true})
	go func() {
		for range agentSide.Control() {
		}
	}()
	sess := agentproto.NewSession(hr, hw, true)
	done := make(chan error, 1)
	go func() { done <- hostlink.Run(sess, r.cfg) }()
	host := config.SocketHostPath(a.Cfg.EnvDir, "app")
	var st linkState
	for i := 0; i < 500; i++ {
		st, _ = readLinkState(a.Cfg.DataDir)
		if len(st.Sockets) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(st.Sockets) != 1 || st.Sockets[0].Name != "app" || st.Sockets[0].HostPath != host || st.Sockets[0].Reason != "" {
		t.Fatalf("link.json's sockets: %+v", st.Sockets)
	}
	if v := a.socketViews(true); len(v) != 1 || !v[0].serving {
		t.Errorf("status and doctor read: %+v", v)
	}
	sess.Close()
	<-done
	if st, ok := readLinkState(a.Cfg.DataDir); !ok || len(st.Sockets) != 0 {
		t.Errorf("after the link's end: %+v", st.Sockets)
	}
}

// Everything that writes link.json, at once, as a config edit at the moment
// of the agent's hello does: a reader only ever finds a whole file, no write
// fails, and the last state written is what stays.
func TestLinkStateWritersRace(t *testing.T) {
	a := socketApp(t, socketConf)
	var logs safeBuffer
	r := &linkRunner{a: a, log: log.New(&logs, "", 0), settings: settingsOf(a.Cfg)}
	big := strings.Repeat("x", 32<<10)
	info := func(i int) []agentproto.SocketInfo {
		return []agentproto.SocketInfo{{Name: "app", Path: "/tmp/app.sock", HostPath: strconv.Itoa(i), Reason: big}}
	}
	conf := filepath.Join(a.Cfg.EnvDir, config.FileName)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // the config's edits
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(conf, []byte("[link.forward_sockets]\napp = \"/tmp/app"+strconv.Itoa(i%2)+".sock\"\n"), 0o644)
			r.reload()
		}
	}()
	go func() { // another writer of the same state
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.writeState()
		}
	}()
	go func() { // a reader
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(filepath.Join(a.Cfg.DataDir, linkStateFile))
			if err == nil && !json.Valid(b) {
				t.Errorf("a partial link.json, %d bytes", len(b))
				return
			}
		}
	}()
	for i := range 300 {
		r.setSockets(info(i))
	}
	close(stop)
	wg.Wait()
	if s := logs.String(); strings.Contains(s, "cannot write") {
		t.Errorf("a write failed: %s", s[:min(len(s), 300)])
	}
	if entries, _ := filepath.Glob(filepath.Join(a.Cfg.DataDir, linkStateFile+".*")); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	// A writer that took its snapshot before the last set must not land after
	// it.
	for i := range 200 {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.writeState()
			}
		}()
		r.setSockets(info(1000 + i))
		close(stop)
		wg.Wait()
		st, ok := readLinkState(a.Cfg.DataDir)
		if !ok || len(st.Sockets) != 1 || st.Sockets[0].HostPath != strconv.Itoa(1000+i) {
			t.Fatalf("round %d: link.json holds an older state", i)
		}
	}
}

// writeLinkState from writers that share no lock, as the daemon's stop mark
// and a link's own: each publishes whole what it wrote, and none fails.
func TestWriteLinkStateConcurrent(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 32<<10)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				st := linkState{Sockets: []agentproto.SocketInfo{{Name: strconv.Itoa(w*1000 + i), Reason: big}}}
				if err := writeLinkState(dir, st); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				if b, err := os.ReadFile(filepath.Join(dir, linkStateFile)); err != nil || !json.Valid(b) {
					t.Errorf("read %d bytes, %v", len(b), err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if entries, _ := filepath.Glob(filepath.Join(dir, linkStateFile+".*")); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
