package menubar

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain":              "plain",
		"two\nlines\r\nhere": "two lines  here",
		"tab\there":          "tab here",
		"bidi \u202egnp.exe": "bidi ?gnp.exe",
		"zero\u200bwidth":    "zero?width",
		"bell\a":             "bell?",
		"bad \xff byte":      "bad ? byte",
		"sep\u2028line":      "sep line",
		"ünïcødé ✓":          "ünïcødé ✓",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("é", 100)
	got := Clip(long, 80)
	if utf8.RuneCountInString(got) != 80 || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Errorf("Clip: %q (%d)", got, utf8.RuneCountInString(got))
	}
	if Clip("short", 80) != "short" {
		t.Error("Clip cut a short title")
	}
}

func TestRequestTitle(t *testing.T) {
	long := strings.Repeat("p", 300)
	cases := []struct {
		name string
		view daemonproto.RequestView
		want string
	}{
		{"alias", daemonproto.RequestView{ID: 3, Kind: "alias", Key: "alias:deploy"}, "#3 dev alias: deploy"},
		{"egress", daemonproto.RequestView{ID: 4, Kind: "egress", Key: "egress:api.example.com"}, "#4 dev connect: api.example.com"},
		{"url", daemonproto.RequestView{ID: 5, Kind: "open_url", Key: "open_url:https://example.com", Effect: "open https://example.com/a/b in the browser"},
			"#5 dev open: https://example.com/a/b"},
		{"url root", daemonproto.RequestView{ID: 5, Kind: "open_url", Key: "open_url:https://example.com", Effect: "open https://example.com/ in the browser"},
			"#5 dev open: https://example.com"},
		{"url long path keeps the host", daemonproto.RequestView{ID: 6, Kind: "open_url", Key: "open_url:https://example.com",
			Effect: "open https://example.com/a/" + long + "/z in the browser"}, "#6 dev open: https://example.com/a/…/z"},
		{"url one long segment", daemonproto.RequestView{ID: 7, Kind: "open_url", Key: "open_url:https://example.com",
			Effect: "open https://example.com/" + long + " in the browser"}, ""},
		{"url effect of another origin", daemonproto.RequestView{ID: 8, Kind: "open_url", Key: "open_url:https://example.com",
			Effect: "open https://evil.example/x in the browser"}, "#8 dev open: https://example.com"},
		{"effect is not the title", daemonproto.RequestView{ID: 9, Kind: "alias", Key: "alias:x", Effect: "run alias x on the host\nwith " + long}, "#9 dev alias: x"},
		{"no key", daemonproto.RequestView{ID: 10, Kind: "host_exec"}, "#10 dev host command"},
		{"port", daemonproto.RequestView{ID: 1, Kind: "port", Key: "port:4321"}, "#1 dev forward: 4321"},
	}
	for _, c := range cases {
		got := RequestTitle("dev", c.view)
		if utf8.RuneCountInString(got) > MaxTitle || strings.ContainsAny(got, "\n\r\u202e") {
			t.Errorf("%s: %q is not one clean line of at most %d", c.name, got, MaxTitle)
		}
		if c.want != "" && got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if c.view.Kind == "open_url" && !strings.HasPrefix(got, "#"+strings.Fields(got)[0][1:]+" dev open: https://example.com") {
			t.Errorf("%s: the origin did not survive: %q", c.name, got)
		}
	}
	// A path with no segments to keep is cut in its middle, keeping its
	// start and its end.
	got := RequestTitle("dev", cases[5].view)
	if !strings.HasPrefix(got, "#7 dev open: https://example.com/p") || !strings.HasSuffix(got, "p") || !strings.Contains(got, "…") || utf8.RuneCountInString(got) != MaxTitle {
		t.Errorf("one long segment: %q", got)
	}
}

func TestIcon(t *testing.T) {
	for _, c := range []struct {
		in   IconInput
		want IconState
	}{
		{IconInput{}, IconState{ImageIdle, "", "caboose: nothing running"}},
		{IconInput{Running: 2}, IconState{ImageRunning, "", "caboose: 2 running"}},
		{IconInput{Running: 1, Attention: 3}, IconState{ImageRunning, " 3", "caboose: 1 running; 3 things need you"}},
		// Attention alone never fills the railcar: the image says only
		// whether a sandbox runs.
		{IconInput{Attention: 1}, IconState{ImageIdle, " 1", "caboose: nothing running; 1 thing needs you"}},
		{IconInput{Attention: 9}, IconState{ImageIdle, " 9", "caboose: nothing running; 9 things need you"}},
		{IconInput{Disconnected: true, Running: 2, Attention: 3}, IconState{ImageIdle, " !", "caboose: the daemon is not answering"}},
	} {
		if got := Icon(c.in); got != c.want {
			t.Errorf("Icon(%+v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestThrottle(t *testing.T) {
	var th Throttle
	now := t0
	var got []string
	admit := func() {
		own, n := th.Admit(now)
		if own {
			got = append(got, "own")
		} else {
			got = append(got, "summary "+string(rune('0'+n)))
		}
	}
	for range 5 {
		admit()
		now = now.Add(time.Second)
	}
	if want := "own,own,own,summary 1,summary 2"; strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
	now = now.Add(ThrottleWindow)
	got = nil
	for range 4 {
		admit()
	}
	if want := "own,own,own,summary 1"; strings.Join(got, ",") != want {
		t.Errorf("after the window: got %v, want %s", got, want)
	}
}

func TestMayStart(t *testing.T) {
	for _, c := range []struct {
		name     string
		seen     bool
		replaced *daemon.Replaced
		want     bool
	}{
		{"nothing seen", false, nil, true},
		{"seen newer", true, nil, false},
		{"replaced by the same", false, &daemon.Replaced{Version: "v1.0.0", Protocol: 1}, true},
		{"replaced by an older", false, &daemon.Replaced{Version: "v0.9.0", Protocol: 1}, true},
		{"replaced by a newer version", false, &daemon.Replaced{Version: "v1.0.1", Protocol: 1}, false},
		{"replaced by a newer protocol", false, &daemon.Replaced{Version: "dev", Protocol: 2}, false},
		{"replaced by a checkout's build of the same protocol", false, &daemon.Replaced{Version: "dev", Protocol: 1}, true},
	} {
		var r daemon.Replaced
		if c.replaced != nil {
			r = *c.replaced
		}
		if got := MayStart(c.seen, r, c.replaced != nil, 1, "v1.0.0"); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSpawnExe(t *testing.T) {
	const own, cli = "/Applications/Caboose.app/Contents/MacOS/caboose", "/home/u/.caboose/versions/v1.2.0/caboose"
	for _, c := range []struct {
		name string
		f    SpawnFacts
		want string
	}{
		{"release app, newer CLI", SpawnFacts{Own: own, OwnVersion: "v1.1.0", Installed: cli, InstalledVersion: "v1.2.0"}, cli},
		{"release app, same CLI", SpawnFacts{Own: own, OwnVersion: "v1.2.0", Installed: cli, InstalledVersion: "v1.2.0"}, cli},
		{"release app, older CLI", SpawnFacts{Own: own, OwnVersion: "v1.3.0", Installed: cli, InstalledVersion: "v1.2.0"}, own},
		{"release app, no install", SpawnFacts{Own: own, OwnVersion: "v1.3.0"}, own},
		{"release app, odd tag", SpawnFacts{Own: own, OwnVersion: "v1.3.0", Installed: cli, InstalledVersion: "dev"}, own},
		{"checkout app", SpawnFacts{Checkout: true, Own: own, OwnVersion: "v1.1.0", Installed: cli, InstalledVersion: "v1.2.0"}, own},
	} {
		if got := SpawnExe(c.f); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPageURL(t *testing.T) {
	code := strings.Repeat("A", daemonproto.CodeLen)
	w := &daemonproto.WebResult{WebState: daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"},
		URL: "http://127.0.0.1:7171/", CodeURL: "http://127.0.0.1:7171/#code=" + code}
	for route, want := range map[string]string{
		"":                      w.CodeURL,
		"requests":              w.CodeURL + "&to=requests",
		"proposals":             w.CodeURL + "&to=proposals",
		"env/dev":               w.CodeURL + "&to=env/dev",
		"requests/dev":          w.CodeURL + "&to=requests/dev",
		"env/Dev":               "",
		"env/dev&x=1":           "",
		"env/../x":              "",
		"doctor":                "",
		"requests/":             "",
		"https://evil.example/": "",
	} {
		got, ok := pageURL(w, route, false)
		if ok != (want != "") || got != want {
			t.Errorf("route %q: %q %v, want %q", route, got, ok, want)
		}
	}
	off := *w
	off.Listening = false
	if u, ok := pageURL(&off, "requests", false); ok || u != "" {
		t.Errorf("not listening: %q %v", u, ok)
	}
	none := *w
	none.CodeURL = ""
	if _, ok := pageURL(&none, "", false); ok {
		t.Error("no code URL opened")
	}
	bad := *w
	bad.CodeURL = w.URL + "#token=" + code
	if _, ok := pageURL(&bad, "", false); ok {
		t.Error("a token URL opened")
	}
	// The code goes only to the web UI's own address for this build.
	if _, ok := pageURL(w, "", true); ok {
		t.Error("a checkout's app opened a release's address")
	}
	devPage := &daemonproto.WebResult{WebState: w.WebState, URL: "http://127.0.0.1:7172/", CodeURL: "http://127.0.0.1:7172/#code=" + code}
	if u, ok := pageURL(devPage, "", true); !ok || u != devPage.CodeURL {
		t.Errorf("a checkout's page: %q %v", u, ok)
	}
	for _, base := range []string{"http://127.0.0.1:7173/", "http://localhost:7171/", "http://evil.example/", "http://127.0.0.1:7171/x/", "http://127.0.0.2:7171/"} {
		other := &daemonproto.WebResult{WebState: w.WebState, URL: base, CodeURL: base + "#code=" + code}
		if err := other.Validate(); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if u, ok := pageURL(other, "", false); ok || u != "" {
			t.Errorf("%s: opened %q", base, u)
		}
	}
}

func TestDumpFormats(t *testing.T) {
	if got, want := group(263168), "263,168"; got != want {
		t.Errorf("group: %s", got)
	}
	if got := group(41200); got != "41,200" {
		t.Errorf("group: %s", got)
	}
	if got := group(999); got != "999" {
		t.Errorf("group: %s", got)
	}
	if got := gb(3328599654); got != "3.1" {
		t.Errorf("gb: %s", got)
	}
	if got := gb(8 << 30); got != "8" {
		t.Errorf("gb: %s", got)
	}
	for d, want := range map[time.Duration]string{30 * time.Minute: "up 30 min", 3 * time.Hour: "up 3 h", 72 * time.Hour: "up 3 d"} {
		if got := uptime(d); got != want {
			t.Errorf("uptime(%v) = %s", d, got)
		}
	}
}
