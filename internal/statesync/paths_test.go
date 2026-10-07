package statesync

import (
	"testing"

	"github.com/bfreis/caboose/internal/sandboxcfg"
)

func TestProjectKeyMatchesClaudeCode(t *testing.T) {
	// Shaped as Claude Code writes them: a '.' in a user name becomes '-'
	// like every other character that is not a letter or a digit.
	got := ProjectKey("/work/you.me/caboose")
	if want := "-work-you-me-caboose"; got != want {
		t.Errorf("ProjectKey = %q, want %q", got, want)
	}
}

func TestLiveTarget(t *testing.T) {
	c, err := sandboxcfg.Parse(sandboxcfg.Default([]string{"/work", "/opt/x"}))
	if err != nil {
		t.Fatal(err)
	}
	for repo, want := range map[string]struct {
		rel, stop, merge string
		inPlace, keys    bool
	}{
		"home/.claude.json":                            {".claude.json", ".claude.json", "json", true, true},
		"home/.claude/settings.json":                   {".claude/settings.json", ".claude", "json", false, false},
		"home/.claude/projects/-work-a-b/memory/x.md":  {".claude/projects/-work-a-b/memory/x.md", ".claude/projects", "text", false, false},
		"home/.claude/projects/-work/memory/MEMORY.md": {".claude/projects/-work/memory/MEMORY.md", ".claude/projects", "union", false, false},
		"home/.claude/projects/-opt-x-a/memory/x.md":   {".claude/projects/-opt-x-a/memory/x.md", ".claude/projects", "text", false, false},
		"home/.claude/skills/s/SKILL.md":               {".claude/skills/s/SKILL.md", ".claude/skills", "text", false, false},
		"home/.config/caboose/start.d/10-x":            {".config/caboose/start.d/10-x", ".config/caboose", "text", false, false},
	} {
		got, err := LiveTarget(c, repo)
		if err != nil || got.Rel != want.rel || got.Stop != want.stop || got.Rule.Merge != want.merge ||
			got.InPlace != want.inPlace || got.Keys() != want.keys {
			t.Errorf("LiveTarget(%q) = %+v, %v; want %+v", repo, got, err, want)
		}
	}
	for _, repo := range []string{
		"home/.claude/.credentials.json",
		"home/.claude/history.jsonl",
		"home/.claude/CLAUDE.md",
		"home/.claude/projects/-work-a/x.jsonl",
		"home/.claude/projects/-home-agent/memory/x.md",
		"home/.claude/projects/-workspace-a/memory/x.md",
		"home/.claude/projects/-opt-xy/memory/x.md",
		"home/.claude/projects/-opt-y/memory/x.md",
		"home/.claude/projects/-work-a/memory/../../../../.credentials.json",
		"home/.claude/skills/",
		"home/.claude/skills/.git/config",
		"home/.claude/skills/synced/a_b/docx/SKILL.md",
		"home/.config/gh/hosts.yml",
		"home/.config/git/config",
		"home//.claude/settings.json",
		"home/",
		"claude/settings.json",
		"claude.json",
		"/etc/passwd",
	} {
		if got, err := LiveTarget(c, repo); err == nil {
			t.Errorf("LiveTarget(%q) = %+v; want refused", repo, got)
		}
	}
}
