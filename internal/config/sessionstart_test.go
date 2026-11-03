package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSessionStart(t *testing.T) {
	c, err := load(t, `
[session]
tz = "Europe/Lisbon"

[session.start]
"/Volumes/work/b" = "/src/b"
"~/dev/a" = "/src/a"
"/Volumes/work/c//" = "/opt/c"
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []StartDir{
		{Host: "/Volumes/work/b", Target: "/src/b"},
		{Host: "/Volumes/work/c", Target: "/opt/c"},
		{Host: "/h/dev/a", Target: "/src/a"},
	}
	if !reflect.DeepEqual(c.SessionStart, want) {
		t.Errorf("SessionStart %+v", c.SessionStart)
	}
	if c.TZ != "Europe/Lisbon" {
		t.Errorf("tz beside [session.start] reads %q", c.TZ)
	}
	// [session.start] may come before [session] (TOML allows it).
	c, err = load(t, "[session.start]\n\"/a\" = \"/b\"\n[session]\ntmux = false\n")
	if err != nil || c.Tmux || len(c.SessionStart) != 1 {
		t.Errorf("start before session: %+v %v", c, err)
	}
	if c, err = load(t, ""); err != nil || c.SessionStart != nil {
		t.Errorf("none: %+v %v", c.SessionStart, err)
	}
}

func TestSessionStartRefusals(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`"app" = "/src/app"`, `[session.start] "app": the host directory is not an absolute path`},
		{`"" = "/src/app"`, `has an empty host directory`},
		{`"~" = "/src/app"`, `[session.start] "~"`},
		{`"/a" = "src/app"`, `[session.start] "/a": the start directory "src/app" is not an absolute sandbox path`},
		{`"/a" = "~/app"`, `the start directory "~/app" is not an absolute sandbox path`},
		{`"/a" = "/src/app/"`, `"/src/app/" is not clean; write it as "/src/app"`},
		{`"/a" = "/src/../app"`, `is not clean`},
		{`"/a" = "/src/a\tb"`, `control or invisible character`},
		{"\"/a\" = \"/src/a\u200bb\"", `control or invisible character`},
		{`"/a" = ""`, `the start directory is empty`},
		{`"/a" = "/src/#{session_name}"`, `has a #, which tmux would read`},
		{`"/a" = 3`, `[session.start] "/a" must be a string`},
		{`"/a/b" = "/x"` + "\n" + `"/a//b" = "/y"`, `name the same directory (/a/b)`},
		{`"/a" = "/x"` + "\n" + `"~/a" = "/y"`, ``}, // different once ~ is /h
		{`"/h/a" = "/x"` + "\n" + `"~/a" = "/y"`, `name the same directory (/h/a)`},
	} {
		_, err := load(t, "[session.start]\n"+tc.body+"\n")
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.body, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.body, err, tc.want)
		}
	}
	// Not a table.
	if _, err := load(t, "[session]\nstart = \"/a\"\n"); err == nil || !strings.Contains(err.Error(), "must be a table") {
		t.Errorf("start as a string: %v", err)
	}
}

// A misspelled key in [session] names [session.start] among the known ones.
func TestSessionStartListedAsKnown(t *testing.T) {
	_, err := load(t, "[session]\nstrat = 1\n")
	if err == nil || !strings.Contains(err.Error(), "[session.start]") {
		t.Errorf("unknown key message: %v", err)
	}
}

// Editing [session] settings leaves [session.start] alone and lands in
// [session], with or without the table, before or after [session.start].
func TestEditWithSessionStart(t *testing.T) {
	for _, orig := range []string{
		"[session.start]\n\"/a\" = \"/b\"\n",
		"[session.start]\n\"/a\" = \"/b\"\n\n[session]\ntmux = true\n",
		"[session]\ntmux = true\n\n[session.start]\n\"/a\" = \"/b\"\n",
		"[session.start]\n\"/a\" = \"/b\"\n\n[link]\nhost_exec = false\n",
		Template + "\n[session.start]\n\"/a\" = \"/b\"\n",
	} {
		e := Edit{Set: map[string]any{"session.tz": "Europe/Lisbon", "session.ready_timeout": 30}}
		out := EditFile([]byte(orig), e)
		if err := CheckEdit("config.toml", []byte(orig), out, e); err != nil {
			t.Errorf("%q: %v\n%s", orig, err, out)
			continue
		}
		f, err := ParseFile("config.toml", out)
		if err != nil || f.Vals["session.tz"] != "Europe/Lisbon" || f.SessionStart["/a"] != "/b" {
			t.Errorf("%q: %+v %v\n%s", orig, f, err, out)
		}
	}
	// An edit that moved a mapping would be caught.
	orig := "[session.start]\n\"/a\" = \"/b\"\n"
	if err := CheckEdit("config.toml", []byte(orig), []byte("[session.start]\n\"/a\" = \"/c\"\n"), Edit{}); err == nil ||
		!strings.Contains(err.Error(), "[session.start] would change") {
		t.Errorf("changed mapping: %v", err)
	}
}
