package statesync

import (
	"strings"
	"testing"
)

func TestProjectKeyMatchesClaudeCode(t *testing.T) {
	// Shaped as Claude Code writes them: a '.' in a user name becomes '-'
	// like every other character that is not a letter or a digit.
	got := ProjectKey("/work/you.me/caboose")
	if want := "-work-you-me-caboose"; got != want {
		t.Errorf("ProjectKey = %q, want %q", got, want)
	}
}

func TestSyncedKey(t *testing.T) {
	for _, k := range []string{
		"-work",
		"-work-bfreis-caboose",
		"-work-dev-bfreis-caboose--claude-worktrees-rebrand",
		"-work-" + strings.Repeat("a", 249),
	} {
		if !SyncedKey(k) {
			t.Errorf("SyncedKey(%q) = false", k)
		}
	}
	for _, k := range []string{
		"",
		"-home-agent",
		"-tmp-x",
		"-workx", // shares the prefix, not the separator
		"work-x",
		"bfreis-caboose", // a repo-relative key, as sync once stored them
		"@root",
		"-work-a.b",
		"-work-a b",
		"-work-..",
		"-work-" + strings.Repeat("a", 250),
	} {
		if SyncedKey(k) {
			t.Errorf("SyncedKey(%q) = true", k)
		}
	}
}

func TestLiveTarget(t *testing.T) {
	for repo, want := range map[string]Target{
		"claude.json":          {Rel: ".claude.json", ClaudeJSON: true},
		"claude/settings.json": {Rel: ".claude/settings.json"},
		"claude/projects/-work-a-b/memory/MEMORY.md": {Rel: ".claude/projects/-work-a-b/memory/MEMORY.md"},
		"claude/projects/-work/memory/x.md":          {Rel: ".claude/projects/-work/memory/x.md"},
		"claude/skills/s/SKILL.md":                   {Rel: ".claude/skills/s/SKILL.md"},
		"claude/agents/a.md":                         {Rel: ".claude/agents/a.md"},
		"claude/commands/deep/c.md":                  {Rel: ".claude/commands/deep/c.md"},
	} {
		got, err := LiveTarget(repo)
		if err != nil || got != want {
			t.Errorf("LiveTarget(%q) = %+v, %v; want %+v", repo, got, err, want)
		}
	}
	for _, repo := range []string{
		"claude/.credentials.json",
		"claude/history.jsonl",
		"claude/CLAUDE.md",
		"claude/projects/a/x.jsonl",
		"claude/projects/a/b/memory/x.md",
		"claude/projects/-work-a/b/memory/x.md",
		"claude/projects/a-b/memory/x.md",
		"claude/projects/@root/memory/x.md",
		"claude/projects/-home-agent/memory/x.md",
		"claude/projects/../memory/x.md",
		"claude/projects/a/memory/../../../../.credentials.json",
		"claude/skills/",
		"claude/skills/.git/config",
		"claude/skills/synced/manifest.json",
		"claude/skills/synced/a_b/docx/SKILL.md",
		"/etc/passwd",
		"claude//settings.json",
		"dot_config/gh/hosts.yml",
	} {
		if got, err := LiveTarget(repo); err == nil {
			t.Errorf("LiveTarget(%q) = %+v; want refused", repo, got)
		}
	}
}
