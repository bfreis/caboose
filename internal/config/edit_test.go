package config

import (
	"strings"
	"testing"
)

func TestEditFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		e    Edit
		want string
	}{
		{"replaces an active line, keeping the rest",
			"# mine\nisolation = \"vm.a\"  # was\nformat = 1\n",
			Edit{Set: map[string]any{"isolation": "vm.b"}},
			"# mine\nisolation = \"vm.b\"\nformat = 1\n"},
		{"uncomments the template's top-level line",
			"# Which.\n#isolation = \"gvisor.default\"\n",
			Edit{Set: map[string]any{"isolation": "vm.default"}},
			"# Which.\nisolation = \"vm.default\"\n"},
		{"adds a missing top-level key at the end",
			"format = 1\n",
			Edit{Set: map[string]any{"isolation": "vm.a"}},
			"format = 1\nisolation = \"vm.a\"\n"},
		{"adds to an empty file",
			"",
			Edit{Set: map[string]any{"isolation": "vm.a"}},
			"isolation = \"vm.a\"\n"},
		{"adds a top-level key above the first table and its comments, never uncommenting below it",
			"format = 1\n\n# The roots.\n[roots]\ndev = \"~/dev\"\n#isolation = \"x\"\n",
			Edit{Set: map[string]any{"isolation": "vm.a"}},
			"format = 1\n\nisolation = \"vm.a\"\n\n# The roots.\n[roots]\ndev = \"~/dev\"\n#isolation = \"x\"\n"},
		{"a commented-out header that would capture the settings above it is not uncommented",
			"[container.default]\n#[gvisor.default]\nrun_args = [\"--x\"]\n",
			Edit{Set: map[string]any{"gvisor.default.engine_socket": true}},
			"[container.default]\n#[gvisor.default]\nrun_args = [\"--x\"]\n\n[gvisor.default]\nengine_socket = true\n"},
		{"a commented-out table counts as the first",
			"format = 1\n\n#[session]\n#tmux = true\n",
			Edit{Set: map[string]any{"isolation": "vm.a"}},
			"format = 1\n\nisolation = \"vm.a\"\n\n#[session]\n#tmux = true\n"},
		{"a key of the same name in a table is not the top-level one",
			"format = 1\n\n[vm.a]\nisolation = 1\n",
			Edit{Set: map[string]any{"isolation": "vm.a"}},
			"format = 1\n\nisolation = \"vm.a\"\n\n[vm.a]\nisolation = 1\n"},
		{"replaces a key in its table, not in another",
			"[session]\ntmux = true\n\n[link]\ntmux = true\n",
			Edit{Set: map[string]any{"link.tmux": false}},
			"[session]\ntmux = true\n\n[link]\ntmux = false\n"},
		{"adds a key to an existing table, after its last setting",
			"[session]\n# mine\ntz = \"UTC\"\n\n[link]\nhost_exec = false\n",
			Edit{Set: map[string]any{"session.auto_sync": true}},
			"[session]\n# mine\ntz = \"UTC\"\nauto_sync = true\n\n[link]\nhost_exec = false\n"},
		{"adds a key to a table that has only comments, after its header",
			"[session]\n# mine\n",
			Edit{Set: map[string]any{"session.tmux": false}},
			"[session]\ntmux = false\n# mine\n"},
		{"uncomments the template's table and key",
			"format = 1\n\n#[session]\n# Tmux?\n#tmux = true\n# TZ\n#tz = \"X\"\n\n#[link]\n#host_exec = false\n",
			Edit{Set: map[string]any{"session.tmux": false}},
			"format = 1\n\n[session]\n# Tmux?\ntmux = false\n# TZ\n#tz = \"X\"\n\n#[link]\n#host_exec = false\n"},
		{"never uncomments a key of the next, commented-out table",
			"[container.default]\nengine_socket = true\n\n#[gvisor.default]\n#run_args = [\"--init\"]\n",
			Edit{Set: map[string]any{"container.default.run_args": "x"}},
			"[container.default]\nengine_socket = true\nrun_args = \"x\"\n\n#[gvisor.default]\n#run_args = [\"--init\"]\n"},
		{"creates a missing table at the end",
			"format = 1\n",
			Edit{Set: map[string]any{"vm.default.cpus": 4}},
			"format = 1\n\n[vm.default]\ncpus = 4\n"},
		{"creates an empty table",
			"format = 1\n\n[gvisor.a]\n",
			Edit{Tables: []string{"gvisor.a", "vm.default"}},
			"format = 1\n\n[gvisor.a]\n\n[vm.default]\n"},
		{"finds a header with spaces and a comment",
			"[ vm.default ]  # mine\ncpus = 2\n",
			Edit{Set: map[string]any{"vm.default.cpus": 4}},
			"[ vm.default ]  # mine\ncpus = 4\n"},
		{"comments a key out, only in its table",
			"isolation = \"vm.a\"\n[vm.a]\nisolation = 1\n",
			Edit{Unset: []string{"isolation"}},
			"#isolation = \"vm.a\"\n[vm.a]\nisolation = 1\n"},
		{"comments a table's key out",
			"[ref.a]\nimage = \"x\"\n[build]\nauto_build = true\n",
			Edit{Unset: []string{"ref.a.image"}},
			"[ref.a]\n#image = \"x\"\n[build]\nauto_build = true\n"},
		{"unsetting in a missing table changes nothing",
			"format = 1\n",
			Edit{Unset: []string{"ref.a.image"}},
			"format = 1\n"},
		{"puts [roots] in place of the template's commented one",
			"#[roots]\n#dev = \"~/dev\"\n\n#[build]\n",
			Edit{SetRoots: true, Roots: map[string]FileRoot{"work": {Host: "~/work"}, "dev": {Host: "~/dev"}}},
			"[roots]\ndev = \"~/dev\"\nwork = \"~/work\"\n#dev = \"~/dev\"\n\n#[build]\n"},
		{"adds [roots] at the end",
			"format = 1\n",
			Edit{SetRoots: true, Roots: map[string]FileRoot{"dev": {Host: "/d"}}},
			"format = 1\n\n[roots]\ndev = \"/d\"\n"},
		{"replaces [roots], keeping its comments and the tables after it, the long form inline",
			"[roots]\n# my roots\nold = \"/old\"\n\n[roots.t]\nhost = \"/t\"\npath = \"/opt/t\"\n\n[other]\nx = 1\n",
			Edit{SetRoots: true, Roots: map[string]FileRoot{"dev": {Host: "/dev"}, "t": {Host: "/t", Path: "/opt/t"}}},
			"[roots]\ndev = \"/dev\"\nt = { host = \"/t\", path = \"/opt/t\" }\n# my roots\n\n#[roots.t]\n#host = \"/t\"\n#path = \"/opt/t\"\n\n[other]\nx = 1\n"},
		{"removes [roots] by commenting it out",
			"format = 1\n[roots]\n# mine\ndev = \"/d\"\nw = \"/w\"\n",
			Edit{SetRoots: true},
			"format = 1\n#[roots]\n# mine\n#dev = \"/d\"\n#w = \"/w\"\n"},
		{"quotes what needs quoting",
			"",
			Edit{Set: map[string]any{"ref.a.image": "a \"b\"\\c\td"}},
			"[ref.a]\nimage = \"a \\\"b\\\"\\\\c\\u0009d\"\n"},
		{"writes a list of strings",
			"[apko.default]\n",
			Edit{Set: map[string]any{"apko.default.packages": []string{"jq", "a\"b"}}},
			"[apko.default]\npackages = [\"jq\", \"a\\\"b\"]\n"},
		{"writes an empty list",
			"",
			Edit{Set: map[string]any{"apko.default.packages": []string{}}},
			"[apko.default]\npackages = []\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(EditFile([]byte(tc.in), tc.e))
			if got != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// The template, edited as setup edits it, reads back as meant, and keeps
// every comment.
func TestEditTemplateReadsBack(t *testing.T) {
	data := []byte(Template)
	for _, e := range []Edit{
		{SetRoots: true, Roots: map[string]FileRoot{"src": {Host: "~/src"}}},
		{SetRoots: true, Roots: map[string]FileRoot{"dev": {Host: "~/dev"}, "work": {Host: "/w k"}, "t": {Host: "/t", Path: "/opt/t"}}},
		{Set: map[string]any{"session.auto_sync": true}},
		{Set: map[string]any{"session.auto_sync": false}},
		{Set: map[string]any{"image": "ref.default", "ref.default.image": "node:22"}},
		{Set: map[string]any{"image": "dockerfile.default"}, Tables: []string{"dockerfile.default"}},
		{Set: map[string]any{"image": "apko.default", "apko.default.defaults": false, "apko.default.packages": []string{"jq", "go-1.26"}}},
		{Unset: []string{"apko.default.defaults"}},
		{Set: map[string]any{"build.auto_build": false}},
		{Set: map[string]any{"isolation": "gvisor.default"}, Tables: []string{"gvisor.default"}},
		{Set: map[string]any{"isolation": "vm.default"}, Tables: []string{"vm.default"}},
		{Set: map[string]any{"vm.default.cpus": 4, "link.host_exec": true}},
		{SetRoots: true, Roots: map[string]FileRoot{"one": {Host: "~/one", Path: "/work"}}},
		{Set: map[string]any{"format": 1}},
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
	if err != nil || c.Profile != "vm.default" || c.VMCPUs != 4 || !c.HostExec || c.AutoSync || c.AutoBuild ||
		c.ImageProfile.String() != "apko.default" || !c.ImageProfile.Defaults || len(c.ImageProfile.Packages) != 2 ||
		len(c.Roots) != 1 || c.Roots[0].Container != "/work" {
		t.Errorf("%+v %v\n%s", c, err, data)
	}
}

func TestCheckEdit(t *testing.T) {
	for _, tc := range []struct {
		orig, data string
		e          Edit
		want       string
	}{
		{"[container.default]\n#[gvisor.default]\nrun_args = [\"--x\"]\n", "[container.default]\n[gvisor.default]\nrun_args = [\"--x\"]\n\n",
			Edit{Tables: []string{"gvisor.default"}}, "run_args in [container.default] would change"},
		{"", "[session]\nauto_sync = true\n", Edit{Set: map[string]any{"session.auto_sync": true}}, ""},
		{"", "", Edit{Set: map[string]any{"session.auto_sync": true}}, "auto_sync in [session] reads back as <nil>, not true"},
		{"", "[session]\nauto_sync = true\n", Edit{Set: map[string]any{"session.auto_sync": false}}, "reads back as true, not false"},
		{"", "[ref.a]\nimage = \"/x\"\n", Edit{Unset: []string{"ref.a.image"}}, "image in [ref.a] is still set, to /x"},
		{"", "[apko.a]\npackages = [\"jq\"]\n", Edit{Set: map[string]any{"apko.a.packages": []string{"jq"}}}, ""},
		{"", "[apko.a]\npackages = [\"jq\"]\n", Edit{Set: map[string]any{"apko.a.packages": []string{"go"}}}, "packages in [apko.a] reads back as [jq], not [go]"},
		{"", "", Edit{Tables: []string{"dockerfile.a"}}, "[dockerfile.a] is not there"},
		{"[dockerfile.a]\n", "", Edit{}, "[dockerfile.a] would be lost"},
		{"", "[roots]\na = \"/a\"\n", Edit{SetRoots: true, Roots: map[string]FileRoot{"b": {Host: "/b"}}}, "[roots] reads back as"},
		{"", "[roots]\na = { host = \"/a\", path = \"/opt/a\" }\n", Edit{SetRoots: true, Roots: map[string]FileRoot{"a": {Host: "/a", Path: "/opt/a"}}}, ""},
		{"", "isolation = \n", Edit{}, "config.toml:"},
		{"", "format = 1\n", Edit{Set: map[string]any{"format": 1}}, ""},
		{"", "", Edit{Set: map[string]any{"format": 1}}, "format reads back as <nil>, not 1"},
		{"", "[vm.a]\ncpus = 4\n", Edit{Set: map[string]any{"vm.a.cpus": 4}, Tables: []string{"vm.a"}}, ""},
		{"", "", Edit{Tables: []string{"vm.a"}}, "[vm.a] is not there"},
	} {
		err := CheckEdit("config.toml", []byte(tc.orig), []byte(tc.data), tc.e)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q %+v: %v, want %q", tc.data, tc.e, err, tc.want)
		}
	}
}

func TestSnippet(t *testing.T) {
	e := Edit{Set: map[string]any{"isolation": "vm.default", "vm.default.cpus": 4}, Tables: []string{"vm.default", "gvisor.x"},
		Unset: []string{"ref.a.image"}, SetRoots: true,
		Roots: map[string]FileRoot{"dev": {Host: "~/dev"}, "t": {Host: "/t", Path: "/opt/t"}}}
	want := "isolation = \"vm.default\"\n[gvisor.x]\n[vm.default]\ncpus = 4\n# (remove image in [ref.a])\n" +
		"[roots]\ndev = \"~/dev\"\nt = { host = \"/t\", path = \"/opt/t\" }\n"
	if got := e.Snippet(); got != want {
		t.Errorf("got\n%s", got)
	}
}

// A multi-line array is one value: set replaces all its lines, unset
// comments out all of them, and the key after it is left as it was.
func TestEditMultiLineArray(t *testing.T) {
	const in = "[apko.default]\n" +
		"packages = [\n" +
		"  \"jq\",       # a comment, with a ] in it\n" +
		"  # a whole-line comment [\n" +
		"  \"go-1.26\", \"a]b\",\n" +
		"]\n" +
		"defaults = false\n" +
		"\n" +
		"[session]\n" +
		"tmux = false\n"
	for _, tc := range []struct {
		name string
		e    Edit
		want string
	}{
		{"set", Edit{Set: map[string]any{"apko.default.packages": []string{"git"}}},
			"[apko.default]\npackages = [\"git\"]\ndefaults = false\n\n[session]\ntmux = false\n"},
		{"unset", Edit{Unset: []string{"apko.default.packages"}},
			"[apko.default]\n" +
				"#packages = [\n" +
				"#  \"jq\",       # a comment, with a ] in it\n" +
				"#  # a whole-line comment [\n" +
				"#  \"go-1.26\", \"a]b\",\n" +
				"#]\n" +
				"defaults = false\n\n[session]\ntmux = false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(EditFile([]byte(in), tc.e))
			if got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			if err := CheckEdit("config.toml", []byte(in), []byte(got), tc.e); err != nil {
				t.Error(err)
			}
		})
	}
}
