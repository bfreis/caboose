package agent

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/shelltest"
)

// openHost answers the link's requests as a host that opens whatever it is
// asked, or refuses with refuse, and records the URLs it was asked for.
func openHost(t *testing.T, refuse string) (socket string, urls func() []string) {
	t.Helper()
	h := startLink(t)
	waitSocket(t, h.socket)
	var mu sync.Mutex
	var got []string
	go func() {
		for b := range h.sess.Control() {
			m, _ := agentproto.Decode(b)
			if m.Type != agentproto.TypeRequest {
				continue
			}
			mu.Lock()
			got = append(got, m.URL)
			mu.Unlock()
			r := agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, OK: refuse == "", Error: refuse}
			h.sess.Send(r)
		}
	}()
	return h.socket, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func runXdgOpen(socket string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = xdgOpen(socket, args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestXdgOpenReachesHost(t *testing.T) {
	socket, urls := openHost(t, "")
	for _, u := range []string{"http://localhost:3000/cb?code=a&b=c", "https://example.com/"} {
		if code, _, stderr := runXdgOpen(socket, u); code != 0 || stderr != "" {
			t.Errorf("%s: %d %q", u, code, stderr)
		}
	}
	if got := strings.Join(urls(), " "); got != "http://localhost:3000/cb?code=a&b=c https://example.com/" {
		t.Errorf("the host was asked for %q", got)
	}
}

// What is no http(s) URL is refused here, with exit 1, and never asked.
func TestXdgOpenRefuses(t *testing.T) {
	socket, urls := openHost(t, "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"/tmp/x.html"}, `"/tmp/x.html" is a file`},
		{[]string{"./x"}, `"./x" is a file`},
		{[]string{""}, `"" is a file`},
		{[]string{"file:///etc/passwd"}, "has the scheme file"},
		{[]string{"mailto:a@example.com"}, "has the scheme mailto"},
		{[]string{"http:///x"}, "has no host"},
		{nil, "usage: xdg-open URL"},
		{[]string{"https://a/", "https://b/"}, "usage: xdg-open URL"},
	} {
		code, stdout, stderr := runXdgOpen(socket, tc.args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%q: %d %q %q, want 1 and %q", tc.args, code, stdout, stderr, tc.want)
		}
	}
	if got := urls(); len(got) != 0 {
		t.Errorf("the host was asked for %q", got)
	}
}

func TestXdgOpenHelp(t *testing.T) {
	for _, a := range []string{"--help", "-h"} {
		if code, stdout, stderr := runXdgOpen("/nonexistent", a); code != 0 || !strings.Contains(stdout, "usage: xdg-open") || stderr != "" {
			t.Errorf("%s: %d %q %q", a, code, stdout, stderr)
		}
	}
}

// A host that refuses, or none linked, is an action that failed: exit 4.
func TestXdgOpenHostError(t *testing.T) {
	socket, _ := openHost(t, "opening URLs is off (open_urls)")
	code, _, stderr := runXdgOpen(socket, "https://example.com/")
	if code != 4 || !strings.Contains(stderr, "xdg-open: opening URLs is off") {
		t.Errorf("got %d %q", code, stderr)
	}
	code, _, stderr = runXdgOpen(filepath.Join(t.TempDir(), "none"), "https://example.com/")
	if code != 4 || !strings.Contains(stderr, "no host is linked") {
		t.Errorf("no link: %d %q", code, stderr)
	}
}

// The plain open refuses a file before asking the host, too.
func TestOpenCommandRefusesFiles(t *testing.T) {
	var errb bytes.Buffer
	if code := Main([]string{"open", "/tmp/x.html"}, strings.NewReader(""), nopCloser{io.Discard}, &errb); code != 1 || !strings.Contains(errb.String(), `"/tmp/x.html" is a file`) {
		t.Errorf("got %d %q", code, errb.String())
	}
}

// The agent never runs xdg-open or reads BROWSER: the shim exists only in
// the image, and on the host (the agent's code runs there in tests, and
// the launcher links this package) it would open a browser by itself.
func TestAgentNeverRunsTheShim(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`Command("xdg-open"`, `Getenv("BROWSER")`, `LookupEnv("BROWSER")`} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %s", f, bad)
			}
		}
	}
}

// xdg-open.sh passes its arguments to the agent whole, in every shell.
func TestXdgOpenScript(t *testing.T) {
	src, err := os.ReadFile("../../xdg-open.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "caboose-agent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nfor a; do printf '[%s]' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "xdg-open")
	body := strings.ReplaceAll(string(src), "/usr/local/bin/caboose-agent", stub)
	if body == string(src) {
		t.Fatal("xdg-open.sh does not run /usr/local/bin/caboose-agent")
	}
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sh := range shelltest.Shells(t) {
		cmd := exec.Command(sh[0], append(sh[1:], script, "https://example.com/a b?x=1&y=2", "--")...)
		out, err := cmd.CombinedOutput()
		if want := "[xdg-open][https://example.com/a b?x=1&y=2][--]"; err != nil || string(out) != want {
			t.Errorf("%v: %q, %v, want %q", sh, out, err, want)
		}
	}
}
