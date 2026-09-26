package config

import (
	"strings"
	"testing"
)

func TestParsePersist(t *testing.T) {
	for _, tc := range []struct{ name, path, rel, err string }{
		{"aws", "~/.aws", ".aws", ""},
		{"foo", "~/.config/foo", ".config/foo", ""},
		{"foo", "~/.config/foo/", ".config/foo", ""},
		{"bar", "~/.local/share/bar", ".local/share/bar", ""},
		{"baz", "~/.cache/baz", ".cache/baz", ""},
		{"x", "~/.local/x", ".local/x", ""},
		{"notes", "~/notes", "notes", ""},

		{"Bad", "~/.aws", "", "not a persist name"},
		{"aws", "/home/agent/.aws", "", "written ~/<dir>"},
		{"aws", ".aws", "", "written ~/<dir>"},
		{"aws", "~", "", "written ~/<dir>"},
		{"aws", "~/", "", "not a plain directory"},
		{"aws", "~/.", "", "not a plain directory"},
		{"aws", "~/..", "", "not a plain directory"},
		{"aws", "~/../etc", "", "not a plain directory"},
		{"aws", "~//etc", "", "not a plain directory"},
		{"aws", "~/a/../b", "", "not a plain directory"},
		{"aws", "~/.config/foo/bar", "", "too deep"},
		{"aws", "~/.aws/sso", "", "too deep"},
		{"aws", "~/.ssh", "", "caboose's own"},
		{"aws", "~/.claude", "", "caboose's own"},
		{"aws", "~/.local/state", "", "caboose's own"},
		{"aws", "~/.config/gh", "", "caboose's own"},
		{"aws", "~/.caboose-sync", "", "caboose's own"},
		{"aws", "~/.config", "", "holds ~/.config/git"},
		{"aws", "~/.local", "", "holds ~/.local/bin"},
		{"aws", "~/.cache", "", "holds ~/.cache/claude"},
		{"aws", "~/.local/share", "", "holds ~/.local/share/claude"},
	} {
		p, err := ParsePersist(tc.name, tc.path)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s = %q: err %v, want %q", tc.name, tc.path, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s = %q: %v", tc.name, tc.path, err)
			continue
		}
		if p.Rel != tc.rel || p.Container != ContainerHome+"/"+tc.rel || p.Name != tc.name || p.Path != tc.path {
			t.Errorf("%s = %q: got %+v", tc.name, tc.path, p)
		}
	}
}

func TestPersistTable(t *testing.T) {
	f, err := ParseFile("c.toml", []byte("[persist]\nfoo = \"~/.config/foo\"\naws = \"~/.aws\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Persist) != 2 || f.Persist[0].Name != "aws" || f.Persist[1].Name != "foo" {
		t.Errorf("Persist = %+v, want aws then foo", f.Persist)
	}
	c, err := Load(envOf(map[string]string{"HOME": "/h"}), fakeFS{"/h/.caboose/envs/default/config.toml": "[persist]\naws = \"~/.aws\"\n"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Persist) != 1 || c.Persist[0].Container != "/home/agent/.aws" {
		t.Errorf("Config.Persist = %+v", c.Persist)
	}
	for _, tc := range []struct{ toml, err string }{
		{"persist = \"~/.aws\"\n", "must be a table"},
		{"[persist]\naws = 1\n", "persist.aws must be a directory"},
		{"[persist]\na = \"~/.aws\"\nb = \"~/.aws/\"\n", "both name ~/.aws"},
		{"[persist]\naws = \"~/.ssh\"\n", "c.toml: persist.aws: ~/.ssh is caboose's own"},
		{"[persist]\n[persist.x]\n", "persist.x must be a directory"},
	} {
		if _, err := ParseFile("c.toml", []byte(tc.toml)); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%q: err %v, want %q", tc.toml, err, tc.err)
		}
	}
	// An empty table persists nothing, and is no error.
	if f, err := ParseFile("c.toml", []byte("[persist]\n")); err != nil || len(f.Persist) != 0 {
		t.Errorf("empty [persist]: %+v, %v", f, err)
	}
	// The error for an unknown key lists the table.
	if _, err := ParseFile("c.toml", []byte("nope = 1\n")); err == nil || !strings.Contains(err.Error(), "[persist]") {
		t.Errorf("unknown key: %v", err)
	}
}

func TestSamePersist(t *testing.T) {
	a, _ := ParsePersist("aws", "~/.aws")
	b, _ := ParsePersist("foo", "~/.config/foo")
	c, _ := ParsePersist("aws", "~/.config/aws")
	if !SamePersist([]Persist{a, b}, []Persist{b, a}) || !SamePersist(nil, nil) {
		t.Error("the same entries in another order differ")
	}
	if SamePersist([]Persist{a}, []Persist{c}) || SamePersist([]Persist{a}, nil) || SamePersist([]Persist{a, b}, []Persist{a}) {
		t.Error("different entries compare the same")
	}
	if got := DescribePersist([]Persist{a, b}); got != "~/.aws (aws), ~/.config/foo (foo)" {
		t.Errorf("DescribePersist = %q", got)
	}
	if got := DescribePersist(nil); got != "none" {
		t.Errorf("DescribePersist(nil) = %q", got)
	}
}
