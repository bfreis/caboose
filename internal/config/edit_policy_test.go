package config

import (
	"slices"
	"strings"
	"testing"
)

// Every edit the policy keys need, applied in turn to the template, reads
// back as asked and loses none of the template's comments.
func TestEditPolicyKeysOnTemplate(t *testing.T) {
	data := []byte(Template)
	for _, e := range []Edit{
		{Set: map[string]any{"trust": "hostile"}},
		{SetRoots: true, Roots: map[string]FileRoot{
			"dev":   {Host: "~/dev", Writable: []string{"org/a", "org/b"}},
			"tools": {Host: "~/src/tools"},
			"opt":   {Host: "~/opt", Path: "/opt/x", Writable: []string{"."}},
		}},
		{Set: map[string]any{"isolation": "vm.default"}, Tables: []string{"vm.default"}},
		{Set: map[string]any{"vm.default.egress_allow": []string{"github.com"}}},
		{Set: map[string]any{"vm.default.egress_allow": []string{"github.com", "api.anthropic.com"}, "vm.default.egress_deny": []string{"*.ads.example"}}},
		{Set: map[string]any{"link.open_urls_allow": []string{"https://github.com"}}},
		{Set: map[string]any{"link.open_urls_allow": []string{"https://github.com", "https://example.com:8443"}}},
		{Set: map[string]any{"alias.caffeinate.command": []string{"/usr/bin/caffeinate", "-i"}, "alias.caffeinate.policy": "ask"}},
		{Set: map[string]any{"alias.caffeinate.policy": "allow"}},
		{Set: map[string]any{"alias.build.command": []string{"/usr/bin/make", "{target}"}, "alias.build.params.target": []string{"all", "test"}}},
	} {
		prev := data
		data = EditFile(data, e)
		if err := CheckEdit("config.toml", prev, data, e); err != nil {
			t.Fatalf("%+v: %v\n%s", e, err, data)
		}
	}
	for _, l := range strings.Split(Template, "\n") {
		if strings.HasPrefix(l, "# ") && !strings.Contains(string(data), l) {
			t.Errorf("lost %q", l)
		}
	}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{cfgPath: string(data)}, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if c.Trust != TrustHostile || !slices.Equal(c.EgressAllow, []string{"github.com", "api.anthropic.com"}) ||
		!slices.Equal(c.EgressDeny, []string{"*.ads.example"}) || len(c.OpenURLsAllow) != 2 {
		t.Errorf("%+v\n%s", c, data)
	}
	var caff, build Alias
	for _, a := range c.Aliases {
		switch a.Name {
		case "caffeinate":
			caff = a
		case "build":
			build = a
		}
	}
	if caff.Policy != AliasAllow || len(caff.Command) != 2 || !slices.Equal(build.Params["target"], []string{"all", "test"}) {
		t.Errorf("%+v %+v\n%s", caff, build, data)
	}
	byName := map[string]Root{}
	for _, r := range c.Roots {
		byName[r.Name] = r
	}
	if !slices.Equal(byName["dev"].Writable, []string{"org/a", "org/b"}) || !byName["opt"].WholeWritable() || byName["opt"].Container != "/opt/x" || len(byName["tools"].Writable) != 0 {
		t.Errorf("%+v\n%s", c.Roots, data)
	}
}

func TestEditRootsKeepsCommentsAndWritable(t *testing.T) {
	in := "# my settings\nformat = 1\n\n# the roots\n[roots]\n# a note\ndev = \"~/dev\"\n\n# trailing\n"
	e := Edit{SetRoots: true, Roots: map[string]FileRoot{"dev": {Host: "~/dev", Writable: []string{"a b", "c"}}}}
	out := EditFile([]byte(in), e)
	if err := CheckEdit("config.toml", []byte(in), out, e); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, keep := range []string{"# my settings", "# the roots", "# a note", "# trailing", `dev = { host = "~/dev", writable = ["a b", "c"] }`} {
		if !strings.Contains(string(out), keep) {
			t.Errorf("lost %q:\n%s", keep, out)
		}
	}
	// A long form with a table is rewritten inline, its other roots kept.
	in = "[roots.dev]\nhost = \"~/dev\"\nwritable = [\"x\"]\n\n[roots.other]\nhost = \"~/o\"\n"
	e = Edit{SetRoots: true, Roots: map[string]FileRoot{"dev": {Host: "~/dev", Writable: []string{"x", "y"}}, "other": {Host: "~/o"}}}
	out = EditFile([]byte(in), e)
	if err := CheckEdit("config.toml", []byte(in), out, e); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Short and long forms in the snippet.
	snip := e.Snippet()
	if !strings.Contains(snip, `dev = { host = "~/dev", writable = ["x", "y"] }`) || !strings.Contains(snip, `other = "~/o"`) {
		t.Errorf("snippet:\n%s", snip)
	}
}

// CheckEdit catches an edit that lost or changed a writable list, or any
// of the policy keys, which an edit that touched something else must keep.
func TestCheckEditPolicy(t *testing.T) {
	orig := "[roots]\ndev = { host = \"~/dev\", writable = [\"a\"] }\n\n[alias.x]\ncommand = [\"/bin/true\"]\n"
	for _, tc := range []struct {
		data string
		e    Edit
		want string
	}{
		{orig, Edit{Set: map[string]any{"session.tmux": true}}, "tmux"},
		{strings.Replace(orig, `writable = ["a"]`, `writable = ["b"]`, 1), Edit{Set: map[string]any{"session.tmux": true}}, "[roots] would change"},
		{strings.Replace(orig, `, writable = ["a"]`, ``, 1), Edit{Set: map[string]any{"session.tmux": true}}, "[roots] would change"},
		{strings.Replace(orig, `/bin/true`, `/bin/false`, 1), Edit{Set: map[string]any{"session.tmux": true}}, "command in [alias.x] would change"},
		{strings.Replace(orig, `, writable = ["a"]`, ``, 1), Edit{SetRoots: true, Roots: map[string]FileRoot{"dev": {Host: "~/dev", Writable: []string{"a"}}}}, "[roots] reads back as"},
	} {
		data := tc.data
		if tc.e.Set != nil {
			data += "\n[session]\ntmux = true\n"
			if tc.want == "tmux" {
				tc.want = ""
			}
		}
		err := CheckEdit("config.toml", []byte(orig), []byte(data), tc.e)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q: %v, want %q", tc.data, err, tc.want)
		}
	}
}
