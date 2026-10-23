package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCheckpointsPaths(t *testing.T) {
	c, err := load(t, `
[checkpoints]
paths = ["/work/dev/x", "~/.claude", "/home/agent/.config/caboose", "~/.claude.json", "/opt/a:b,c"]
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/work/dev/x", "~/.claude", "~/.config/caboose", "~/.claude.json", "/opt/a:b,c"}
	if !slices.Equal(c.Checkpoints, want) {
		t.Errorf("Checkpoints = %q, want %q", c.Checkpoints, want)
	}
	if c, err := load(t, ""); err != nil || c.Checkpoints != nil {
		t.Errorf("default: %q %v, want none", c.Checkpoints, err)
	}
	if c, err := load(t, "[checkpoints]\npaths = []\n"); err != nil || len(c.Checkpoints) != 0 {
		t.Errorf("empty: %q %v", c.Checkpoints, err)
	}
}

func TestCheckpointsRefused(t *testing.T) {
	for _, tc := range []struct{ paths, want string }{
		{`"work/dev/x"`, "absolute"},
		{`"./x"`, "absolute"},
		{`"/work/dev/../x"`, "clean"},
		{`"/work/dev/x/"`, "clean"},
		{`"~/.claude/"`, "clean path under the home"},
		{`"/home/agent/.claude/"`, "clean path under the home"},
		{`"/work//dev"`, "clean"},
		{`"/"`, "whole sandbox"},
		{`"~"`, "whole home"},
		{`"~/"`, "whole home"},
		{`"/home/agent"`, "whole home"},
		{`"/home/agent/"`, "whole home"},
		{`"/home"`, "around the home"},
		{`"/home/other/x"`, "around the home"},
		{`"~other/x"`, "only ~/"},
		{`"~/../x"`, "under the home"},
		{`"~/a/./b"`, "under the home"},
		{`"/home/agent/a/../b"`, "under the home"},
		{`"/work/dev/a\tb"`, "control"},
		{`"/work/dev/a` + "\u202e" + `b"`, "invisible"},
		{`""`, "array of strings"},
		{`"/work/dev/x", "/work/dev/x"`, "twice"},
		{`"~/.claude", "/home/agent/.claude"`, "twice"},
		{`"/work/dev", "/work/dev/x"`, "inside"},
		{`"/work/dev/x/y", "/work/dev/x"`, "inside"},
		{`"~/.claude", "~/.claude/projects"`, "inside"},
		{`"/home/agent/.config/caboose/x", "~/.config"`, "inside"},
		{`"/work/dev/` + strings.Repeat("a", 1100) + `"`, "too long"},
	} {
		wantErr(t, "[checkpoints]\npaths = ["+tc.paths+"]\n", tc.want)
	}
	// Siblings, and paths sharing a prefix, are not nested.
	if _, err := load(t, "[checkpoints]\npaths = [\"/work/dev/x\", \"/work/dev/xy\", \"~/.claude\", \"~/.claude.json\"]\n"); err != nil {
		t.Errorf("siblings: %v", err)
	}
	var list []string
	for i := range MaxCheckpointPaths + 1 {
		list = append(list, fmt.Sprintf("%q", fmt.Sprintf("/work/dev/p%d", i)))
	}
	if _, err := load(t, "[checkpoints]\npaths = ["+strings.Join(list[:MaxCheckpointPaths], ", ")+"]\n"); err != nil {
		t.Errorf("%d paths: %v", MaxCheckpointPaths, err)
	}
	wantErr(t, "[checkpoints]\npaths = ["+strings.Join(list, ", ")+"]\n", "at most 32")
	wantErr(t, "[checkpoints]\npaths = \"/work/dev/x\"\n", "array of strings")
	wantErr(t, "[checkpoints]\ninterval = 10\n", "unknown setting in [checkpoints] interval")
	wantErr(t, "checkpoints = [\"/work/dev/x\"]\n", "must be a table")
}

// The parse check is ParseFile's too, so what setup or apply writes is
// checked when it is read back.
func TestCheckpointsParseFile(t *testing.T) {
	if _, err := ParseFile("c.toml", []byte("[checkpoints]\npaths = [\"rel\"]\n")); err == nil || !strings.Contains(err.Error(), "[checkpoints]") {
		t.Errorf("err %v", err)
	}
	f, err := ParseFile("c.toml", []byte("[checkpoints]\npaths = [\"/home/agent/.claude\"]\n"))
	if err != nil || !slices.Equal(f.Checkpoints, []string{"~/.claude"}) {
		t.Errorf("%v %v", f, err)
	}
}
