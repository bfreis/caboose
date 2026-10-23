package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

const testAppExe = "/Applications/Caboose.app/Contents/MacOS/caboose"

// startAppServer is a daemon in this process whose peers all say they are
// process 4242, whose executable is testAppExe: what an attach records
// comes from these, never from params.
func startAppServer(t *testing.T, n *Notifier) (string, *server) {
	t.Helper()
	return startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Notifier: n,
		peer: func(net.Conn) (peerCred, error) { return peerCred{UID: ownUID(), PID: 4242}, nil },
		executable: func(pid int) string {
			if pid == 4242 {
				return testAppExe
			}
			return ""
		}})
}

func attach(t *testing.T, c *daemonproto.Client, params string) (*daemonproto.AppAttachResult, error) {
	t.Helper()
	var r daemonproto.AppAttachResult
	err := c.Call(t.Context(), daemonproto.MethodAppAttach, json.RawMessage(params), &r)
	return &r, err
}

func appOf(t *testing.T, c *daemonproto.Client) daemonproto.AppInfo {
	t.Helper()
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Daemon.App == nil {
		t.Fatal("status has no app")
	}
	return *st.Daemon.App
}

// One app at a time: the first app connection attaches, with what the
// socket says of it (a pid or an executable in params is ignored); the
// command line and the page cannot attach or poll; a second app is busy
// and may not poll; re-attaching changes the capabilities; once the
// first connection closes, another app attaches.
func TestAppAttach(t *testing.T) {
	dir, s := startAppServer(t, nil)
	cli := dialAs(t, dir, daemonproto.ClientCLI, 0)
	if a := appOf(t, cli); a.Attached || a.PID != 0 {
		t.Fatalf("an app before any attached: %+v", a)
	}
	a1 := dialAs(t, dir, daemonproto.ClientApp, 0)
	r, err := attach(t, a1, `{"capabilities":["notifications","bogus","dialogs","notifications"],"version":"v1.2.3","pid":1,"executable":"/tmp/evil"}`)
	if err != nil || !r.Attached || !slices.Equal(r.Capabilities, []string{daemonproto.CapDialogs, daemonproto.CapNotifications}) {
		t.Fatalf("attach: %+v %v", r, err)
	}
	a := appOf(t, cli)
	if !a.Attached || a.PID != 4242 || a.Executable != testAppExe || a.Version != "v1.2.3" || a.Since.IsZero() ||
		!slices.Equal(a.Capabilities, []string{daemonproto.CapDialogs, daemonproto.CapNotifications}) {
		t.Errorf("status: %+v", a)
	}
	// The page sees the app, never its executable.
	b, e := s.callWeb(t.Context(), daemonproto.MethodStatus, nil)
	if e != nil {
		t.Fatal(e)
	}
	var web daemonproto.StatusResult
	if err := json.Unmarshal(b, &web); err != nil || web.Daemon.App == nil || !web.Daemon.App.Attached || web.Daemon.App.PID != 4242 || web.Daemon.App.Executable != "" {
		t.Errorf("the page's status: %s %v", b, err)
	}
	if bytes.Contains(b, []byte("Caboose.app")) {
		t.Errorf("the page got the executable: %s", b)
	}
	for _, m := range []string{daemonproto.MethodAppAttach, daemonproto.MethodNotifications} {
		if err := cli.Call(t.Context(), m, paramsFor(m), nil); daemonproto.Kind(err) != daemonproto.KindNotAllowed {
			t.Errorf("%s from the cli: %v", m, err)
		}
		if _, e := s.callWeb(t.Context(), m, paramsFor(m)); webKind(e) != daemonproto.KindNotAllowed {
			t.Errorf("%s from the web: %v", m, e)
		}
	}
	a2 := dialAs(t, dir, daemonproto.ClientApp, 0)
	if _, err := attach(t, a2, `{"capabilities":["notifications"]}`); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Errorf("a second app: %v", err)
	}
	if _, err := a2.Notifications(t.Context(), 0); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("notifications from an app not attached: %v", err)
	}
	if n, err := a1.Notifications(t.Context(), 0); err != nil || len(n.Items) != 0 {
		t.Errorf("notifications: %+v %v", n, err)
	}
	if r, err := attach(t, a1, `{"version":"v1.2.3"}`); err != nil || !r.Attached || len(r.Capabilities) != 0 {
		t.Errorf("re-attach: %+v %v", r, err)
	}
	if a := appOf(t, cli); !a.Attached || len(a.Capabilities) != 0 {
		t.Errorf("after re-attach: %+v", a)
	}
	a1.Close()
	waitFor(t, 5*time.Second, "the attachment ends", func() bool { return !appOf(t, cli).Attached })
	if r, err := attach(t, a2, `{"capabilities":["dialogs"]}`); err != nil || !r.Attached {
		t.Errorf("an app after the first went away: %+v %v", r, err)
	}
	if _, err := a2.Notifications(t.Context(), 0); err != nil {
		t.Errorf("notifications once attached: %v", err)
	}
}

// Bad params are refused before anything is attached.
func TestAppAttachParams(t *testing.T) {
	dir, _ := startAppServer(t, nil)
	a := dialAs(t, dir, daemonproto.ClientApp, 0)
	for _, p := range []string{`{"capabilities":["Notifications"]}`, `{"capabilities":["a","b","c","d","e","f","g","h","i"]}`,
		`{"version":"v1\u0000"}`, `{"capabilities":"notifications"}`} {
		if _, err := attach(t, a, p); daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s: %v", p, err)
		}
	}
	if appOf(t, a).Attached {
		t.Error("attached by bad params")
	}
}

// On Linux, the PID is the socket's (SO_PEERCRED): this process's, and the
// executable is this test binary.
func TestAppPIDFromSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PEERCRED")
	}
	dir, _ := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}})
	a := dialAs(t, dir, daemonproto.ClientApp, 0)
	if _, err := attach(t, a, `{"pid":1}`); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := appOf(t, a); got.PID != os.Getpid() || got.Executable != exe {
		t.Errorf("attached %+v, want pid %d, %s", got, os.Getpid(), exe)
	}
	if got := executableOf(0); got != "" {
		t.Errorf("no pid's executable: %q", got)
	}
}

// The path in a kern.procargs2 buffer is its first string after the
// argument count.
func TestProcargsPath(t *testing.T) {
	buf := binary.LittleEndian.AppendUint32(nil, 2)
	buf = append(buf, testAppExe+"\x00\x00\x00caboose\x00app\x00"...)
	if got := string(procargsPath(buf)); got != testAppExe {
		t.Errorf("got %q", got)
	}
	if got := procargsPath(buf[:4]); got != nil {
		t.Errorf("a count alone: %q", got)
	}
	if got := string(procargsPath(append(buf[:4:4], "/x"...))); got != "/x" {
		t.Errorf("no NUL: %q", got)
	}
	long := boundExecutable("/" + strings.Repeat("a", 2*daemonproto.MaxExecutable) + "\x1b")
	if len(long) > daemonproto.MaxExecutable || strings.ContainsRune(long, 0x1b) {
		t.Errorf("an executable of %d bytes", len(long))
	}
}

// fallback records what the host's own notifications would have shown.
type fallback struct {
	mu  sync.Mutex
	got []string
}

func (f *fallback) notify(title, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, title+"|"+text)
	return nil
}

func (f *fallback) shown() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// testNotifier is a Notifier with a fake fallback, a fake clock and a log
// in a buffer.
func testNotifier() (*Notifier, *fallback, *time.Time, *bytes.Buffer) {
	fb := &fallback{}
	n := NewNotifier(fb.notify)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	n.now = func() time.Time { return now }
	var buf bytes.Buffer
	n.setLog(log.New(&buf, "", 0))
	return n, fb, &now, &buf
}

// Without an app, every notification goes to the host's own, with the
// title and text it always had: a sandbox's under its own title, or
// "caboose".
func TestNotifierFallback(t *testing.T) {
	n, fb, _, _ := testNotifier()
	_ = n.For("alpha", SourceLink).Notify("caboose: a session proposes a change", "a.toml")
	_ = n.For("alpha", SourceCheckpoints).Notify("caboose (alpha)", "low space")
	_ = n.For("alpha", SourceLink).SandboxNotify("Approve request?", "done")
	_ = n.For("alpha", SourceLink).SandboxNotify("", "done")
	want := []string{"caboose: a session proposes a change|a.toml", "caboose (alpha)|low space",
		"Approve request?|done", "caboose|done"}
	if got := fb.shown(); !slices.Equal(got, want) {
		t.Errorf("shown %q, want %q", got, want)
	}
	// Attached without notifications, the same.
	n.attach(false, true)
	_ = n.For("alpha", SourceLink).Notify("t", "x")
	if got := fb.shown(); len(got) != len(want)+1 {
		t.Errorf("shown %q", got)
	}
}

// With an app holding notifications that polls, every notification lands
// in its queue, with its origin, environment and route, under a title of
// the table (a sandbox's own title only in the text), and none reaches
// the host's own.
func TestNotifierQueues(t *testing.T) {
	n, fb, _, _ := testNotifier()
	n.attach(true, true)
	_ = n.For("alpha", SourceLink).Notify("caboose: a session proposes a change", "a.toml")
	_ = n.For("alpha", SourceCheckpoints).Notify("caboose (alpha)", "low space")
	_ = n.For("", SourceDaemon).Notify("whatever", "y")
	_ = n.For("alpha", SourceLink).SandboxNotify("Approve request?", "done")
	_ = n.For("alpha", SourceLink).SandboxNotify("", "plain")
	if got := fb.shown(); len(got) != 0 {
		t.Fatalf("the host's own showed %q", got)
	}
	r := n.poll(0)
	if err := r.Validate(); err != nil {
		t.Fatalf("the result: %v", err)
	}
	type item struct{ env, origin, title, text, route string }
	want := []item{
		{"alpha", "host", "caboose: alpha", "a.toml", "env/alpha"},
		{"alpha", "host", "caboose: alpha checkpoints", "low space", "env/alpha"},
		{"", "host", "caboose", "y", ""},
		{"alpha", "sandbox", "From the alpha sandbox", "Approve request?: done", "env/alpha"},
		{"alpha", "sandbox", "From the alpha sandbox", "plain", "env/alpha"},
	}
	if len(r.Items) != len(want) || r.Next != uint64(len(want)) {
		t.Fatalf("got %+v", r)
	}
	for i, w := range want {
		g := r.Items[i]
		if (item{g.Env, g.Origin, g.Title, g.Text, g.Route}) != w || g.Seq != uint64(i+1) || g.At.IsZero() {
			t.Errorf("item %d: %+v, want %+v", i, g, w)
		}
	}
	// What was had is dropped once asked past.
	if r := n.poll(r.Next); len(r.Items) != 0 || r.Next != uint64(len(want)) {
		t.Errorf("after: %+v", r)
	}
	// A sandbox's title never becomes the title, whatever it says.
	_ = n.For("alpha", SourceLink).SandboxNotify("caboose: alpha", "x")
	if r := n.poll(0); len(r.Items) != 1 || r.Items[0].Title != "From the alpha sandbox" {
		t.Errorf("a sandbox posing as the host: %+v", r)
	}
}

// An app that stops asking for 30 s has new notifications shown by the
// host's own again, until it asks.
func TestNotifierStaleApp(t *testing.T) {
	n, fb, now, _ := testNotifier()
	n.attach(true, true)
	*now = now.Add(29 * time.Second)
	_ = n.For("alpha", SourceLink).Notify("t1", "queued")
	*now = now.Add(time.Second)
	_ = n.For("alpha", SourceLink).Notify("t2", "shown")
	if got := fb.shown(); !slices.Equal(got, []string{"t2|shown"}) {
		t.Errorf("shown %q", got)
	}
	if r := n.poll(0); len(r.Items) != 1 || r.Items[0].Text != "queued" {
		t.Errorf("queued %+v", r)
	}
	_ = n.For("alpha", SourceLink).Notify("t3", "queued again")
	if got := fb.shown(); len(got) != 1 {
		t.Errorf("shown %q after a poll", got)
	}
}

// The queue keeps the last 64; after pages through them, 32 at a time;
// an after past the last is brought back to it.
func TestNotifierRing(t *testing.T) {
	n, _, _, logBuf := testNotifier()
	n.attach(true, true)
	for range 70 {
		_ = n.For("alpha", SourceLink).Notify("t", "x")
	}
	r := n.poll(0)
	if len(r.Items) != 32 || r.Items[0].Seq != 7 || r.Next != 38 {
		t.Fatalf("first page: %d items from %d, next %d", len(r.Items), r.Items[0].Seq, r.Next)
	}
	r = n.poll(r.Next)
	if len(r.Items) != 32 || r.Items[0].Seq != 39 || r.Next != 70 {
		t.Fatalf("second page: %d items from %d, next %d", len(r.Items), r.Items[0].Seq, r.Next)
	}
	if r = n.poll(r.Next); len(r.Items) != 0 || r.Next != 70 {
		t.Errorf("past the end: %+v", r)
	}
	if r = n.poll(1000); r.Next != 70 {
		t.Errorf("an after past the last: %+v", r)
	}
	if !strings.Contains(logBuf.String(), "queue is full") {
		t.Errorf("the overflow was not logged: %q", logBuf.String())
	}
}

// An item the app never fetched in 10 minutes is dropped, and counted.
func TestNotifierDropsOld(t *testing.T) {
	n, _, now, logBuf := testNotifier()
	n.attach(true, true)
	for range 3 {
		_ = n.For("alpha", SourceLink).Notify("t", "x")
	}
	*now = now.Add(10 * time.Minute)
	if r := n.poll(0); len(r.Items) != 0 {
		t.Errorf("old items: %+v", r)
	}
	if !strings.Contains(logBuf.String(), "dropped 3") {
		t.Errorf("the drop was not logged: %q", logBuf.String())
	}
}

// The attachment ending empties the queue and sends notifications to
// the host's own; a fresh one starts empty.
func TestNotifierDetach(t *testing.T) {
	n, fb, _, _ := testNotifier()
	n.attach(true, true)
	_ = n.For("alpha", SourceLink).Notify("t", "lost")
	n.detach()
	_ = n.For("alpha", SourceLink).Notify("t", "shown")
	n.attach(true, true)
	if r := n.poll(0); len(r.Items) != 0 {
		t.Errorf("a fresh attachment: %+v", r)
	}
	if got := fb.shown(); !slices.Equal(got, []string{"t|shown"}) {
		t.Errorf("shown %q", got)
	}
}

// A daemon exiting for a replace writes replaced.json, 0600, naming the
// replacing caboose; the next daemon's start writes itself over it; a
// plain stop writes none.
func TestReplacedFile(t *testing.T) {
	d := startReplaceDaemon(t, "v1.0.0", &fakeLinks{})
	d.noLockCheck = true
	if r, err := d.mustClient(replaceProto).Replace(t.Context(), "v1.1.0", false); err != nil || !r.Exiting {
		t.Fatalf("replace: %+v %v", r, err)
	}
	d.waitStopped(5 * time.Second)
	got, err := ReadReplaced(d.dir)
	if err != nil || got.Version != "v1.1.0" || got.Protocol != replaceProto || got.At.IsZero() {
		t.Fatalf("replaced.json: %+v %v", got, err)
	}
	if fi, err := os.Lstat(filepath.Join(d.dir, ReplacedFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("replaced.json's mode: %v %v", fi.Mode(), err)
	}
	stop := runInProcess(t, Options{Dir: d.dir, Version: "v1.2.0", Idle: time.Minute, Log: testLog{t}})
	got, err = ReadReplaced(d.dir)
	if err != nil || got.Version != "v1.2.0" || got.Protocol != daemonproto.ProtocolVersion {
		t.Errorf("after the next start: %+v %v", got, err)
	}
	stop()

	// Forced, by a caboose of a newer protocol.
	f := startReplaceDaemon(t, "v1.0.0", &fakeLinks{})
	if r, err := f.mustClient(replaceProto+1).Replace(t.Context(), "v2.0.0", true); err != nil || !r.Exiting {
		t.Fatalf("forced replace: %+v %v", r, err)
	}
	f.waitStopped(5 * time.Second)
	if got, err := ReadReplaced(f.dir); err != nil || got.Version != "v2.0.0" || got.Protocol != replaceProto+1 {
		t.Errorf("forced: %+v %v", got, err)
	}

	// A stop is no replace; a start writes no file it does not find.
	h := newHarness(t)
	stop = runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}})
	stop()
	if _, err := ReadReplaced(h.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after a stop: %v", err)
	}
}

// A replaced.json over its bound, or not JSON, is an error.
func TestReadReplacedBounded(t *testing.T) {
	dir := t.TempDir()
	for _, b := range []string{strings.Repeat(" ", maxReplaced+1), "{"} {
		if err := os.WriteFile(filepath.Join(dir, ReplacedFile), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadReplaced(dir); err == nil {
			t.Errorf("read %d bytes", len(b))
		}
	}
}

// notifyLinks are a test daemon's links: ensuring one notifies, as a
// link's proposals notice and a sandbox's notify would.
type notifyLinks struct {
	noLinks
	n *Notifier
}

func (l notifyLinks) Serve(_ context.Context, method, env string) (*daemonproto.LinkResult, error) {
	_ = l.n.For(env, SourceLink).Notify("caboose: a session proposes a change", "host "+method)
	_ = l.n.For(env, SourceLink).SandboxNotify("Approve request?", "from the sandbox")
	return &daemonproto.LinkResult{Action: daemonproto.ActionUnchanged, Env: daemonproto.EnvStatus{Name: env, Sandbox: "running", Link: "running"}}, nil
}

// notifyFallback is the test daemon's host notifications: lines appended
// to the file named.
func notifyFallback(file string) func(title, text string) error {
	return func(title, text string) error {
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(title + "|" + text + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
}

// End to end, against the re-exec'd test daemon: an app attaches, a
// second is busy, notifications go to the attached app's queue and not to
// the host's own, and once it is gone they go to the host's own again and
// another app attaches.
func TestAppAgainstDaemon(t *testing.T) {
	h := newHarness(t)
	shown := filepath.Join(h.home, "shown")
	h.extraEnv = []string{testNotifyEnv + "=" + shown}
	cfg := h.config(time.Minute)
	appCfg := cfg
	appCfg.Client = daemonproto.ClientApp
	app := h.ensure(appCfg)
	if r, err := app.AppAttach(t.Context(), daemonproto.AppAttachParams{Capabilities: []string{daemonproto.CapNotifications}, Version: "v0.0.0-test"}); err != nil || !r.Attached {
		t.Fatalf("attach: %+v %v", r, err)
	}
	second, err := Connect(t.Context(), appCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.AppAttach(t.Context(), daemonproto.AppAttachParams{}); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Errorf("a second app: %v", err)
	}
	cli := h.ensure(cfg)
	st, err := cli.Status(t.Context())
	if err != nil || st.Daemon.App == nil || !st.Daemon.App.Attached {
		t.Fatalf("status: %+v %v", st, err)
	}
	if runtime.GOOS == "linux" && st.Daemon.App.PID != os.Getpid() {
		t.Errorf("the app's pid: %d, want %d", st.Daemon.App.PID, os.Getpid())
	}
	if _, err := cli.EnsureLink(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	r, err := app.Notifications(t.Context(), 0)
	if err != nil || len(r.Items) != 2 {
		t.Fatalf("notifications: %+v %v", r, err)
	}
	if i := r.Items[0]; i.Origin != daemonproto.OriginHost || i.Title != "caboose: alpha" || i.Text != "host ensure_link" || i.Route != "env/alpha" {
		t.Errorf("host item %+v", i)
	}
	if i := r.Items[1]; i.Origin != daemonproto.OriginSandbox || i.Title != "From the alpha sandbox" || i.Text != "Approve request?: from the sandbox" {
		t.Errorf("sandbox item %+v", i)
	}
	if exists(shown) {
		t.Error("the host's own showed a notification the app held")
	}
	app.Close()
	waitFor(t, 5*time.Second, "the attachment ends", func() bool {
		st, err := cli.Status(t.Context())
		return err == nil && st.Daemon.App != nil && !st.Daemon.App.Attached
	})
	if _, err := cli.EnsureLink(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(shown)
	if want := "caboose: a session proposes a change|host ensure_link\nApprove request?|from the sandbox\n"; err != nil || string(b) != want {
		t.Errorf("the host's own showed %q (%v), want %q", b, err, want)
	}
	if r, err := second.AppAttach(t.Context(), daemonproto.AppAttachParams{}); err != nil || !r.Attached {
		t.Errorf("an app after the first went away: %+v %v", r, err)
	}
}
