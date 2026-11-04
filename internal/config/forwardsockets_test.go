package config

import (
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
)

func TestForwardSocketsRead(t *testing.T) {
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: `
[link]
open_urls = "off"

[link.forward_sockets]
web-2 = "/tmp/web/s.sock"
app = "/tmp/app/server.sock"
`}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []ForwardSocket{{"app", "/tmp/app/server.sock"}, {"web-2", "/tmp/web/s.sock"}}
	if len(c.ForwardSockets) != 2 || c.ForwardSockets[0] != want[0] || c.ForwardSockets[1] != want[1] {
		t.Errorf("ForwardSockets = %v, want %v", c.ForwardSockets, want)
	}
	if c.OpenURLs != "off" {
		t.Errorf("[link]'s own key: open_urls = %q", c.OpenURLs)
	}
	if got := SocketHostPath(c.EnvDir, "app"); got != c.EnvDir+"/sockets/app.sock" {
		t.Errorf("SocketHostPath = %q", got)
	}
}

func TestForwardSocketsRefused(t *testing.T) {
	long := "/" + strings.Repeat("a", agentproto.MaxSocketPath)
	ok := "/" + strings.Repeat("a", agentproto.MaxSocketPath-1)
	if _, err := ParseFile("c.toml", []byte("[link.forward_sockets]\na = \""+ok+"\"\n")); err != nil {
		t.Errorf("a path of %d bytes: %v", len(ok), err)
	}
	many := ""
	for i := range MaxForwardSockets + 1 {
		many += "a" + strings.Repeat("b", i) + " = \"/s\"\n"
	}
	for name, tc := range map[string]struct{ body, want string }{
		"upper name":      {`App = "/s"`, "not a name"},
		"underscore name": {`a_b = "/s"`, "not a name"},
		"dot name":        {`"a.b" = "/s"`, "not a name"},
		"slash name":      {`"../x" = "/s"`, "not a name"},
		"leading hyphen":  {`-a = "/s"`, "not a name"},
		"long name":       {strings.Repeat("a", 33) + ` = "/s"`, "not a name"},
		"relative path":   {`a = "tmp/s"`, "absolute, clean"},
		"tilde path":      {`a = "~/s"`, "absolute, clean"},
		"unclean path":    {`a = "/tmp/../s"`, "absolute, clean"},
		"trailing slash":  {`a = "/tmp/"`, "absolute, clean"},
		"root":            {`a = "/"`, "absolute, clean"},
		"empty":           {`a = ""`, "absolute, clean"},
		"long path":       {`a = "` + long + `"`, "at most 107"},
		"control":         {`a = "/tmp/\u001b[0m"`, "control or invisible"},
		"not a string":    {`a = 3`, "must be a string"},
		"a table":         {"[link.forward_sockets.a]\nb = 1", "must be a string"},
		"too many":        {many, "at most 16"},
	} {
		_, err := ParseFile("c.toml", []byte("[link.forward_sockets]\n"+tc.body+"\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
	if _, err := ParseFile("c.toml", []byte("[link]\nforward_sockets = 3\n")); err == nil || !strings.Contains(err.Error(), "must be a table") {
		t.Errorf("not a table: %v", err)
	}
	// A key [link] does not know is an error still.
	if _, err := ParseFile("c.toml", []byte("[link]\nforward_socket = {a = \"/s\"}\n")); err == nil || !strings.Contains(err.Error(), "forward_socket") {
		t.Errorf("an unknown key: %v", err)
	}
}

// CheckEdit's path reads the same checks, and holds an entry to what it was;
// an edit of [link]'s own keys goes in the right table.
func TestForwardSocketsEdit(t *testing.T) {
	orig := []byte("[link.forward_sockets]\napp = \"/tmp/app.sock\"\n")
	e := Edit{Set: map[string]any{"link.open_urls": "off"}}
	out := EditFile(orig, e)
	if err := CheckEdit("c.toml", orig, out, e); err != nil {
		t.Errorf("%v in:\n%s", err, out)
	}
	f, err := ParseFile("c.toml", out)
	if err != nil || f.Vals["link.open_urls"] != "off" || len(f.ForwardSockets) != 1 {
		t.Errorf("after the edit: %+v %v in:\n%s", f, err, out)
	}
	changed := []byte("[link.forward_sockets]\napp = \"/tmp/other.sock\"\n")
	if err := CheckEdit("c.toml", orig, changed, Edit{}); err == nil || !strings.Contains(err.Error(), "app in [link.forward_sockets]") {
		t.Errorf("an entry changed: %v", err)
	}
	if err := CheckEdit("c.toml", nil, []byte("[link.forward_sockets]\nBad = \"/s\"\n"), Edit{}); err == nil {
		t.Error("CheckEdit took a bad name")
	}
}
