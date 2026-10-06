package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		argv []string
		want Invocation
		err  string
	}{
		// Nothing: start or attach the session, nothing for claude.
		{nil, Invocation{}, ""},
		// claude: everything after it is claude's, untouched.
		{[]string{"claude"}, Invocation{Command: "claude"}, ""},
		{[]string{"claude", "-p", "hi", "status"}, Invocation{Command: "claude", Args: []string{"-p", "hi", "status"}}, ""},
		{[]string{"claude", "--help"}, Invocation{Command: "claude", Args: []string{"--help"}}, ""},
		{[]string{"claude", "-e", "x", "--session", "y"}, Invocation{Command: "claude", Args: []string{"-e", "x", "--session", "y"}}, ""},
		{[]string{"-e", "work", "claude", "--resume"}, Invocation{Env: "work", Command: "claude", Args: []string{"--resume"}}, ""},
		// Subcommands, with their arguments.
		{[]string{"logs", "--tail", "50"}, Invocation{Command: "logs", Args: []string{"--tail", "50"}}, ""},
		{[]string{"shell", "-c", "true"}, Invocation{Command: "shell", Args: []string{"-c", "true"}}, ""},
		{[]string{"shell", "-c", "echo --help"}, Invocation{Command: "shell", Args: []string{"-c", "echo --help"}}, ""},
		{[]string{"build", "--no-cache"}, Invocation{Command: "build", Args: []string{"--no-cache"}}, ""},
		{[]string{"check-image", "alpine"}, Invocation{Command: "check-image", Args: []string{"alpine"}}, ""},
		{[]string{"sync", "--remote", "u"}, Invocation{Command: "sync", Args: []string{"--remote", "u"}}, ""},
		{[]string{"status"}, Invocation{Command: "status"}, ""},
		// Help, in every spelling.
		{[]string{"help"}, Invocation{Command: "help"}, ""},
		{[]string{"-h"}, Invocation{Command: "help"}, ""},
		{[]string{"--help"}, Invocation{Command: "help"}, ""},
		{[]string{"help", "sync"}, Invocation{Command: "help", Args: []string{"sync"}}, ""},
		{[]string{"--help", "sync"}, Invocation{Command: "help", Args: []string{"sync"}}, ""},
		{[]string{"sync", "--help"}, Invocation{Command: "help", Args: []string{"sync"}}, ""},
		{[]string{"sync", "--remote", "u", "-h"}, Invocation{Command: "help", Args: []string{"sync"}}, ""},
		{[]string{"status", "-h"}, Invocation{Command: "help", Args: []string{"status"}}, ""},
		{[]string{"logs", "--help"}, Invocation{Command: "help", Args: []string{"logs"}}, ""},
		// ...but a pass-through command's -h anywhere else is its program's.
		{[]string{"logs", "--tail", "5", "-h"}, Invocation{Command: "logs", Args: []string{"--tail", "5", "-h"}}, ""},
		{[]string{"--version"}, Invocation{Command: "version"}, ""},
		// Flags first, in any order, in every spelling.
		{[]string{"--env", "work", "status"}, Invocation{Env: "work", Command: "status"}, ""},
		{[]string{"--env=work"}, Invocation{Env: "work"}, ""},
		{[]string{"--session", "two", "detach"}, Invocation{Session: "two", Command: "detach"}, ""},
		{[]string{"--session=two", "-e", "w", "claude", "-e"}, Invocation{Session: "two", Env: "w", Command: "claude", Args: []string{"-e"}}, ""},
		// Only before the command: after it, they are its arguments.
		{[]string{"shell", "-e", "x"}, Invocation{Command: "shell", Args: []string{"-e", "x"}}, ""},

		// Nothing else reaches claude.
		{[]string{"foo"}, Invocation{}, "unknown command 'foo'"},
		{[]string{"fix the bug"}, Invocation{}, "unknown command 'fix the bug'"},
		{[]string{"statsu"}, Invocation{}, "unknown command 'statsu' -- did you mean 'status'?"},
		{[]string{"-p", "hi"}, Invocation{}, "unknown flag -p"},
		{[]string{"-e", "work", "-p", "hi"}, Invocation{}, "unknown flag -p"},
		{[]string{"--", "status"}, Invocation{}, "unknown flag --"},
		{[]string{"status", "x"}, Invocation{}, "status takes no arguments (got x)"},
		{[]string{"version", "--json"}, Invocation{}, "version takes no arguments (got --json)"},
		{[]string{"--version", "x"}, Invocation{}, "--version takes nothing after it (got x)"},
		{[]string{"help", "nope"}, Invocation{}, "unknown command 'nope'"},
		{[]string{"help", "a", "b"}, Invocation{}, "help takes at most one command (got a b)"},
		{[]string{"--env"}, Invocation{}, "--env needs a name"},
		{[]string{"-e", ""}, Invocation{}, "--env needs a name"},
		{[]string{"--env="}, Invocation{}, "--env needs a name"},
		{[]string{"--session"}, Invocation{}, "--session needs a name"},
		{[]string{"--session="}, Invocation{}, "--session needs a name"},
	}
	for _, tc := range cases {
		got, err := Parse(tc.argv)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("Parse(%q) err = %v, want %q", tc.argv, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) err = %v", tc.argv, err)
			continue
		}
		if len(got.Args) == 0 && len(tc.want.Args) == 0 {
			got.Args, tc.want.Args = nil, nil
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.argv, got, tc.want)
		}
	}
}

// A word caboose refuses is offered to claude, spelled as a shell needs it.
func TestUnknownOffersClaude(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	code, out, errs := runIt("-e", "default", "-p", "it's here")
	want := "caboose: unknown flag -p\n  for claude: caboose claude -p 'it'\\''s here'\n  see 'caboose --help'\n"
	if code != 2 || out != "" || errs != want {
		t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant:\n%s", code, out, errs, want)
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	u := Usage()
	for _, c := range Commands {
		if !strings.Contains(u, "\n  "+c.Name) {
			t.Errorf("usage lacks %s:\n%s", c.Name, u)
		}
	}
}

func TestHelp(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	code, out, errs := runIt("help")
	if code != 0 || errs != "" || out != Usage() {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", code, errs, out)
	}
	// Help needs no configuration: not even one that cannot be read.
	home := sandboxEnv(t)
	dir := filepath.Join(home, ".caboose", "envs", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("nope = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"help", "claude"}, {"status", "--help"}} {
		code, out, errs := runIt(argv...)
		if code != 0 || errs != "" || !strings.Contains(out, "Usage") {
			t.Errorf("%q: exit %d, stderr %q, stdout:\n%s", argv, code, errs, out)
		}
	}
	if _, out, _ := runIt("claude", "--help"); strings.Contains(out, "Usage: caboose") {
		t.Errorf("claude --help was caboose's:\n%s", out)
	}
}

// Every command has a help of its own, naming it.
func TestCommandUsage(t *testing.T) {
	for _, c := range Commands {
		u := CommandUsage(c.Name)
		if !strings.HasPrefix(u, "Usage: caboose [FLAGS] "+c.Name) {
			t.Errorf("%s:\n%s", c.Name, u)
		}
		for _, l := range strings.Split(u+Usage(), "\n") {
			if len([]rune(l)) > 80 {
				t.Errorf("wider than 80 columns: %q", l)
			}
		}
	}
}
