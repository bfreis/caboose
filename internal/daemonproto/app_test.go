package daemonproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNotificationsValidate(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ok := []Notification{
		{Seq: 1, Env: "alpha", Origin: OriginHost, Title: "caboose: alpha checkpoints", Text: "low space", Route: "env/alpha", At: at},
		{Seq: 2, Origin: OriginHost, Title: "caboose", Text: "x", At: at},
		{Seq: 3, Env: "alpha", Origin: OriginSandbox, Title: "From the alpha sandbox", Text: "t: x", Route: "env/alpha", At: at},
		{Seq: 4, Env: "alpha", Origin: OriginHost, Title: "caboose: alpha", Text: "x", Route: "requests/alpha", At: at},
	}
	if err := (&NotificationsResult{Items: ok, Next: 4}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Notification){
		"no seq":              func(n *Notification) { n.Seq = 0 },
		"sandbox's title":     func(n *Notification) { n.Origin, n.Title = OriginSandbox, "Approve request?" },
		"sandbox, no env":     func(n *Notification) { n.Origin, n.Env, n.Title = OriginSandbox, "", SandboxTitle("") },
		"host, free title":    func(n *Notification) { n.Title = "caboose: alpha failed: x" },
		"host, other env":     func(n *Notification) { n.Title = "caboose: beta" },
		"origin":              func(n *Notification) { n.Origin = "agent" },
		"route":               func(n *Notification) { n.Route = "env/../x" },
		"route page":          func(n *Notification) { n.Route = "settings" },
		"env":                 func(n *Notification) { n.Env = "Alpha" },
		"text control":        func(n *Notification) { n.Text = "a\x1bb" },
		"text bidi":           func(n *Notification) { n.Text = "a\u202eb" },
		"text over its bound": func(n *Notification) { n.Text = strings.Repeat("x", MaxNotificationText+1) },
	} {
		n := ok[0]
		mod(&n)
		if err := (&NotificationsResult{Items: []Notification{n}, Next: n.Seq}).Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, n)
		}
	}
	if err := (&NotificationsResult{Items: []Notification{ok[1], ok[0]}, Next: 2}).Validate(); err == nil {
		t.Error("out of order accepted")
	}
	if err := (&NotificationsResult{Items: ok, Next: 3}).Validate(); err == nil {
		t.Error("next before the last accepted")
	}
	many := make([]Notification, MaxNotifications+1)
	for i := range many {
		many[i] = ok[1]
		many[i].Seq = uint64(i + 1)
	}
	if err := (&NotificationsResult{Items: many, Next: uint64(len(many))}).Validate(); err == nil {
		t.Error("over the bound accepted")
	}
	for _, r := range []string{"", "requests", "proposals", "requests/a", "env/a-1"} {
		if !ValidRoute(r) {
			t.Errorf("%q refused", r)
		}
	}
	for _, r := range []string{"env/", "env/A", "requests/", "overview", "/env/a", "env/a/b"} {
		if ValidRoute(r) {
			t.Errorf("%q accepted", r)
		}
	}
}

func TestAppInfoValidate(t *testing.T) {
	good := DaemonInfo{Version: "v1", App: &AppInfo{Attached: true, PID: 1, Executable: "/Applications/Caboose.app/Contents/MacOS/caboose", Capabilities: []string{CapNotifications}}}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, a := range []AppInfo{{PID: -1}, {Executable: "/a\x00"}, {Executable: "/" + strings.Repeat("a", MaxExecutable)},
		{Capabilities: []string{"A"}}, {Version: strings.Repeat("v", 65)}} {
		d := good
		d.App = &a
		if err := d.validate(); err == nil {
			t.Errorf("accepted %+v", a)
		}
	}
}

// Whatever validates of the app's messages is bounded and printable, and
// a notification's title is one of the host's.
func FuzzAppMessages(f *testing.F) {
	for _, s := range []string{
		`{"capabilities":["notifications","dialogs"],"version":"v1.0.0"}`,
		`{"attached":true,"capabilities":["dialogs"]}`,
		`{"items":[{"seq":1,"env":"alpha","origin":"sandbox","title":"From the alpha sandbox","text":"t: x","route":"env/alpha","at":"2026-10-18T12:00:00Z"}],"next":1}`,
		`{"items":[{"seq":2,"origin":"host","title":"caboose","text":"\u202e"}],"next":1}`,
		`{"daemon":{"app":{"attached":true,"pid":4242,"executable":"/x","capabilities":["notifications"]}},"envs":[]}`,
		`{"after":18446744073709551615}`, `[]`, `null`,
		`{"dialog":{"env":"alpha","id":3}}`, `{"dialog":{"env":"../x","id":0}}`, `{"dialog":null}`, `{}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var p AppAttachParams
		if json.Unmarshal(b, &p) == nil && p.Validate() == nil {
			if len(p.Capabilities) > MaxCapabilities || !printable(p.Version) {
				t.Fatalf("accepted %+v", p)
			}
		}
		var a AppAttachResult
		if json.Unmarshal(b, &a) == nil && a.Validate() == nil && len(a.Capabilities) > MaxCapabilities {
			t.Fatalf("accepted %+v", a)
		}
		var n NotificationsResult
		if json.Unmarshal(b, &n) == nil && n.Validate() == nil {
			if len(n.Items) > MaxNotifications {
				t.Fatalf("accepted %d items", len(n.Items))
			}
			for _, i := range n.Items {
				if !printable(i.Text) || len(i.Text) > MaxNotificationText || !ValidRoute(i.Route) {
					t.Fatalf("accepted %+v", i)
				}
				switch i.Origin {
				case OriginSandbox:
					if i.Title != SandboxTitle(i.Env) || !ValidEnv(i.Env) {
						t.Fatalf("accepted %+v", i)
					}
				case OriginHost:
					found := false
					for _, h := range HostTitles(i.Env) {
						found = found || h == i.Title
					}
					if !found {
						t.Fatalf("accepted %+v", i)
					}
				default:
					t.Fatalf("accepted %+v", i)
				}
			}
		}
		var d DialogsResult
		if json.Unmarshal(b, &d) == nil && d.Validate() == nil && d.Dialog != nil {
			if !ValidEnv(d.Dialog.Env) || d.Dialog.ID == 0 {
				t.Fatalf("accepted %+v", d.Dialog)
			}
		}
		var st StatusResult
		if json.Unmarshal(b, &st) == nil && st.Validate() == nil && st.Daemon.App != nil {
			if a := st.Daemon.App; a.PID < 0 || !printable(a.Executable) || len(a.Executable) > MaxExecutable || len(a.Capabilities) > MaxCapabilities {
				t.Fatalf("accepted %+v", a)
			}
		}
	})
}

func TestDialogsValidate(t *testing.T) {
	for _, r := range []DialogsResult{{}, {Dialog: &DialogRef{Env: "alpha", ID: 1}}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for _, d := range []DialogRef{{Env: "alpha"}, {Env: "", ID: 1}, {Env: "../x", ID: 1}, {Env: "Alpha", ID: 1}} {
		if err := (&DialogsResult{Dialog: &d}).Validate(); err == nil {
			t.Errorf("accepted %+v", d)
		}
	}
}
