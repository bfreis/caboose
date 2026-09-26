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
			"# mine\nrepo_root = \"~/old\"  # was\nkeep_versions = 3\n",
			Edit{Set: map[string]any{"repo_root": "~/dev"}},
			"# mine\nrepo_root = \"~/dev\"\nkeep_versions = 3\n"},
		{"uncomments the template's line",
			"# The dir.\n#repo_root = \"~/dev\"\n\n#auto_sync = true\n",
			Edit{Set: map[string]any{"repo_root": "/src", "auto_sync": true}},
			"# The dir.\nrepo_root = \"/src\"\n\nauto_sync = true\n"},
		{"adds a missing key at the end",
			"keep_versions = 3\n",
			Edit{Set: map[string]any{"auto_sync": false}},
			"keep_versions = 3\nauto_sync = false\n"},
		{"adds to an empty file",
			"",
			Edit{Set: map[string]any{"auto_sync": true}},
			"auto_sync = true\n"},
		{"adds a top-level key above the first table, never uncommenting below it",
			"keep_versions = 3\n\n[roots]\ndev = \"~/dev\"\n#auto_sync = true\n",
			Edit{Set: map[string]any{"auto_sync": true}},
			"keep_versions = 3\n\nauto_sync = true\n\n[roots]\ndev = \"~/dev\"\n#auto_sync = true\n"},
		{"a key of the same name in a table is not the top-level one",
			"keep_versions = 3\n\n[roots]\nrepo_root = \"/r\"\n",
			Edit{Set: map[string]any{"repo_root": "~/dev"}},
			"keep_versions = 3\n\nrepo_root = \"~/dev\"\n\n[roots]\nrepo_root = \"/r\"\n"},
		{"comments a key out, only at the top level",
			"repo_root = \"/a\"\n[roots]\nrepo_root = \"/r\"\n",
			Edit{Unset: []string{"repo_root"}},
			"#repo_root = \"/a\"\n[roots]\nrepo_root = \"/r\"\n"},
		{"comments a key out",
			"repo_root = \"~/dev\"\nkeep_versions = 3\n",
			Edit{Unset: []string{"repo_root"}},
			"#repo_root = \"~/dev\"\nkeep_versions = 3\n"},
		{"adds [roots] at the end, after the template's commented one",
			"#[roots]\n#dev = \"~/dev\"\n\n#auto_sync = true\n",
			Edit{SetRoots: true, Roots: map[string]string{"work": "~/work", "dev": "~/dev"}},
			"#[roots]\n#dev = \"~/dev\"\n\n#auto_sync = true\n\n[roots]\ndev = \"~/dev\"\nwork = \"~/work\"\n"},
		{"replaces [roots], keeping its comments and the tables after it",
			"[roots]\n# my roots\nold = \"/old\"\n\n[other]\nx = 1\n",
			Edit{SetRoots: true, Roots: map[string]string{"dev": "/dev"}},
			"[roots]\ndev = \"/dev\"\n# my roots\n\n[other]\nx = 1\n"},
		{"removes [roots] by commenting it out",
			"keep_versions = 3\n[roots]\n# mine\ndev = \"/d\"\nw = \"/w\"\n",
			Edit{SetRoots: true},
			"keep_versions = 3\n#[roots]\n# mine\n#dev = \"/d\"\n#w = \"/w\"\n"},
		{"quotes what needs quoting",
			"",
			Edit{Set: map[string]any{"repo_root": "/a \"b\"\\c\td"}},
			"repo_root = \"/a \\\"b\\\"\\\\c\\u0009d\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(EditFile([]byte(tc.in), tc.e))
			if got != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// The template, edited as setup edits it, reads back as meant: a single
// root, then several in its place, then auto_sync.
func TestEditTemplateReadsBack(t *testing.T) {
	data := []byte(Template)
	for _, e := range []Edit{
		{Set: map[string]any{"repo_root": "~/src"}, SetRoots: true},
		{Unset: []string{"repo_root"}, SetRoots: true, Roots: map[string]string{"dev": "~/dev", "work": "/w k"}},
		{Set: map[string]any{"auto_sync": true}},
		{Set: map[string]any{"auto_sync": false}},
		{Set: map[string]any{"repo_root": "~/one"}, SetRoots: true},
		{Unset: []string{"repo_root"}, SetRoots: true, Roots: map[string]string{"dev": "~/dev", "src": "/src"}},
		{Set: map[string]any{"format": 1}},
	} {
		data = EditFile(data, e)
		if err := CheckEdit("config.toml", data, e); err != nil {
			t.Fatalf("%+v: %v\n%s", e, err, data)
		}
	}
	// The template's comments are all still there.
	for _, l := range strings.Split(Template, "\n") {
		if strings.HasPrefix(l, "# ") && !strings.Contains(string(data), l) {
			t.Errorf("lost %q", l)
		}
	}
}

func TestCheckEdit(t *testing.T) {
	for _, tc := range []struct {
		data string
		e    Edit
		want string
	}{
		{"auto_sync = true\n", Edit{Set: map[string]any{"auto_sync": true}}, ""},
		{"", Edit{Set: map[string]any{"auto_sync": true}}, `auto_sync reads back as "", not "1"`},
		{"auto_sync = true\n", Edit{Set: map[string]any{"auto_sync": false}}, `auto_sync reads back as "1", not ""`},
		{"repo_root = \"/x\"\n", Edit{Unset: []string{"repo_root"}}, `repo_root is still set, to "/x"`},
		{"[roots]\na = \"/a\"\n", Edit{SetRoots: true, Roots: map[string]string{"b": "/b"}}, "[roots] reads back as"},
		{"repo_root = \n", Edit{}, "config.toml:"},
		{"format = 1\n", Edit{Set: map[string]any{"format": 1}}, ""},
		{"", Edit{Set: map[string]any{"format": 1}}, "format reads back as 0"},
	} {
		err := CheckEdit("config.toml", []byte(tc.data), tc.e)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q %+v: %v, want %q", tc.data, tc.e, err, tc.want)
		}
	}
}

func TestSnippet(t *testing.T) {
	e := Edit{Set: map[string]any{"auto_sync": true}, Unset: []string{"repo_root"}, SetRoots: true, Roots: map[string]string{"dev": "~/dev"}}
	want := "auto_sync = true\n# (remove repo_root)\n[roots]\ndev = \"~/dev\"\n"
	if got := e.Snippet(); got != want {
		t.Errorf("got\n%s", got)
	}
}
