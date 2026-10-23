package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

func uiApp(t *testing.T, fd *fakeDaemon) (*App, *[]string, *strings.Builder) {
	t.Helper()
	a, _, _ := newCLIApp(t, fd)
	var opened []string
	a.Opener = func(u string) error { opened = append(opened, u); return nil }
	return a, &opened, nil
}

func notServed() *daemonproto.WebResult {
	return &daemonproto.WebResult{WebState: daemonproto.WebState{Addr: "127.0.0.1:7171", Error: "listen tcp 127.0.0.1:7171: bind: address already in use"}}
}

// caboose ui opens the address of a one-time code, never one with the
// token, and asks for a code each time.
func TestUIOpensCodeURL(t *testing.T) {
	fd := &fakeDaemon{}
	a, opened, _ := uiApp(t, fd)
	if err := a.UI(nil); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://127.0.0.1:7171/#code="+fakeCode {
		t.Errorf("opened %q", *opened)
	}
	if strings.Contains((*opened)[0], "token") {
		t.Errorf("a token in the address: %q", *opened)
	}
	if !slices.Equal(fd.dials, []bool{true}) || !slices.Equal(fd.webCalls, []daemonproto.WebParams{{Code: true}}) {
		t.Errorf("dials %v, web %v", fd.dials, fd.webCalls)
	}
	if err := a.UI(nil); err != nil || len(fd.webCalls) != 2 || !fd.webCalls[1].Code {
		t.Errorf("a second open: %v %v", err, fd.webCalls)
	}
}

func TestUINotListeningOpensNothing(t *testing.T) {
	fd := &fakeDaemon{WebRes: notServed()}
	a, opened, _ := uiApp(t, fd)
	err := a.UI(nil)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{"127.0.0.1:7171", "address already in use", "7172", "caboose daemon restart"} {
		if !strings.Contains(ee.Msg, want) {
			t.Errorf("message lacks %q: %s", want, ee.Msg)
		}
	}
	if len(*opened) != 0 {
		t.Errorf("opened %q", *opened)
	}
	// Even a result that says listening opens nothing without a code.
	r := notServed()
	r.Listening = true
	fd.WebRes = r
	if a.UI(nil) == nil || len(*opened) != 0 {
		t.Errorf("opened without a code: %q", *opened)
	}
	// Nor does a code URL from a daemon that says it is not listening.
	r = notServed()
	r.URL = "http://127.0.0.1:7171/"
	r.CodeURL = "http://127.0.0.1:7171/#code=" + fakeCode
	fd.WebRes = r
	if a.UI(nil) == nil || len(*opened) != 0 {
		t.Errorf("opened while not listening: %q", *opened)
	}
}

func TestUIPrintOpensNothing(t *testing.T) {
	a, opened, _ := uiApp(t, &fakeDaemon{})
	if err := a.UI([]string{"--print", "requests"}); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 0 {
		t.Errorf("opened %q", *opened)
	}
	got := a.Stdout.(interface{ String() string }).String()
	if strings.TrimSpace(got) != "http://127.0.0.1:7171/#code="+fakeCode+"&to=requests" {
		t.Errorf("printed %q", got)
	}
	if e := a.Stderr.(interface{ String() string }).String(); !strings.Contains(e, "once") || !strings.Contains(e, "60 s") {
		t.Errorf("no note of the code's one use: %q", e)
	}
}

func TestUINewTokenSendsRotate(t *testing.T) {
	fd := &fakeDaemon{}
	a, opened, _ := uiApp(t, fd)
	if err := a.UI([]string{"--new-token"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fd.webCalls, []daemonproto.WebParams{{Rotate: true, Code: true}}) || len(*opened) != 1 {
		t.Errorf("web %v, opened %q", fd.webCalls, *opened)
	}
}

func TestUIPageArgument(t *testing.T) {
	for arg, to := range map[string]string{"requests": "&to=requests", "proposals": "&to=proposals", "work": "&to=env/work"} {
		a, opened, _ := uiApp(t, &fakeDaemon{})
		if err := a.UI([]string{arg}); err != nil || len(*opened) != 1 || !strings.HasSuffix((*opened)[0], to) {
			t.Errorf("%s: %v, %q", arg, err, *opened)
		}
	}
	// caboose -e work ui
	a, opened, _ := uiApp(t, &fakeDaemon{})
	a.Cfg.Env = "work"
	if err := a.UI(nil); err != nil || !strings.HasSuffix((*opened)[0], "&to=env/work") {
		t.Errorf("-e work: %v, %q", err, *opened)
	}
	for _, bad := range [][]string{{"a/b"}, {"../x"}, {"Bad Name"}, {"--bogus"}, {"requests", "proposals"}, {"x&to=y"}} {
		fd := &fakeDaemon{}
		a, opened, _ := uiApp(t, fd)
		if err := a.UI(bad); err == nil || len(*opened) != 0 || len(fd.dials) != 0 {
			t.Errorf("%q: err %v, opened %q, dials %v", bad, err, *opened, fd.dials)
		}
	}
}

func TestUIHostNetworkWarning(t *testing.T) {
	a, _, _ := uiApp(t, &fakeDaemon{})
	a.Cfg.RunArgs = []string{"--network=host"}
	if err := a.UI([]string{"--print"}); err != nil {
		t.Fatal(err)
	}
	if e := a.Stderr.(interface{ String() string }).String(); !strings.Contains(e, "host's network") {
		t.Errorf("no warning: %q", e)
	}
	// Most useful when the page is not served: the sandbox may hold its port.
	b, _, _ := uiApp(t, &fakeDaemon{WebRes: notServed()})
	b.Cfg.RunArgs = []string{"--network", "host"}
	if b.UI(nil) == nil {
		t.Fatal("not listening: no error")
	}
	if e := b.Stderr.(interface{ String() string }).String(); !strings.Contains(e, "host's network") {
		t.Errorf("no warning when not listening: %q", e)
	}
}

func stateWith(w *daemonproto.WebState) linkView {
	return linkView{Daemon: &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{Web: w}}}
}

func TestWebLine(t *testing.T) {
	for _, tc := range []struct {
		v    linkView
		want string
	}{
		{stateWith(&daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"}), "http://127.0.0.1:7171/ (listening)"},
		{stateWith(&daemonproto.WebState{Addr: "127.0.0.1:7171", Error: "listen tcp 127.0.0.1:7171: bind: address already in use"}), "not listening: 127.0.0.1:7171 is in use"},
		{stateWith(&daemonproto.WebState{Addr: "127.0.0.1:7172", Error: "boom"}), "not listening: 127.0.0.1:7172: boom"},
		{stateWith(nil), "unknown (this daemon does not say"},
		{linkView{}, "unknown (the daemon is not running"},
	} {
		if got := webLine(tc.v); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
}

func TestStatusWebLine(t *testing.T) {
	for name, tc := range map[string]struct {
		web  *daemonproto.WebState
		want string
	}{
		"listening":     {&daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"}, "http://127.0.0.1:7171/ (listening)"},
		"not listening": {&daemonproto.WebState{Addr: "127.0.0.1:7171", Error: "address already in use"}, "not listening: 127.0.0.1:7171 is in use"},
		"no web info":   {nil, "unknown (this daemon does not say"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
			b.daemon.Stat = &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 99, Version: "v9", Started: time.Now(), Web: tc.web}}
			if err := b.Status(); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			out, _ := b.said()
			d, w := strings.Index(out, "\ndaemon    : "), strings.Index(out, "\nweb       : "+tc.want)
			if d < 0 || w < d {
				t.Errorf("no web line after the daemon line:\n%s", out)
			}
			for _, start := range b.daemon.dials {
				if start {
					t.Error("status started the daemon")
				}
			}
		})
	}
}

func webRows(c *checkup, label string) []finding {
	var r []finding
	for _, f := range c.rows {
		if f.label == label {
			r = append(r, f)
		}
	}
	return r
}

func TestDoctorWebListener(t *testing.T) {
	a, _, _ := newCLIApp(t, &fakeDaemon{})
	c := &checkup{}
	a.doctorWeb(c, stateWith(&daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"}))
	if r := webRows(c, "web"); len(r) != 1 || r[0].level != levelOK {
		t.Errorf("%+v", c.rows)
	}
	c = &checkup{}
	a.doctorWeb(c, stateWith(&daemonproto.WebState{Addr: "127.0.0.1:7171", Error: "address already in use"}))
	r := webRows(c, "web")
	if len(r) != 1 || r[0].level != levelProblem || !strings.Contains(r[0].fix, "7171") && !strings.Contains(r[0].fix, "ss -ltnp") {
		t.Errorf("%+v", c.rows)
	}
	c = &checkup{}
	a.doctorWeb(c, linkView{})
	if r := webRows(c, "web"); len(r) != 1 || r[0].level != levelUnchecked {
		t.Errorf("%+v", c.rows)
	}
	if p := webRows(c, "web ports"); len(p) != 1 || !strings.Contains(p[0].text, "forward_ports") {
		t.Errorf("ports row %+v", p)
	}
}

func TestDoctorWebToken(t *testing.T) {
	a, _, _ := newCLIApp(t, &fakeDaemon{})
	a.Cfg.CabooseHome = t.TempDir()
	dir := a.daemonDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(dir, daemon.TokenFile)
	level := func() finding {
		c := &checkup{}
		a.doctorWebToken(c)
		r := webRows(c, "web token")
		if len(r) != 1 {
			t.Fatalf("%+v", c.rows)
		}
		if strings.Contains(r[0].text, "SECRETSECRET") {
			t.Error("row shows the token")
		}
		return r[0]
	}
	if f := level(); f.level != levelNote {
		t.Errorf("missing: %+v", f)
	}
	if err := os.WriteFile(tok, []byte("SECRETSECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := level(); f.level != levelOK {
		t.Errorf("0600: %+v", f)
	}
	if err := os.Chmod(tok, 0o644); err != nil {
		t.Fatal(err)
	}
	if f := level(); f.level != levelProblem || !strings.Contains(f.text, "0644") {
		t.Errorf("0644: %+v", f)
	}
	// A link is not followed.
	if err := os.Remove(tok); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "real")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, tok); err != nil {
		t.Fatal(err)
	}
	if f := level(); f.level != levelProblem {
		t.Errorf("symlink: %+v", f)
	}
}
