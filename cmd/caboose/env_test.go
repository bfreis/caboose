package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkEnv makes environment name's dir by hand, as caboose -e NAME setup
// would, with an empty config.toml.
func mkEnv(t *testing.T, home, name string) string {
	t.Helper()
	dir := filepath.Join(home, ".caboose", "envs", name)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEnvCommand(t *testing.T) {
	fakeDocker(t)
	home := sandboxEnv(t) // env needs no repo root
	mkEnv(t, home, "work")

	_, out, _ := runIt("env")
	if !strings.Contains(out, "* default          container caboose-default (") || !strings.Contains(out, "  work             container caboose-work (") {
		t.Errorf("list:\n%s", out)
	}
	_, out, _ = runIt("-e", "work", "env", "list")
	if !strings.Contains(out, "* work ") || !strings.Contains(out, "  default ") {
		t.Errorf("list from work:\n%s", out)
	}
	// Creating is setup's now.
	for _, argv := range [][]string{{"env", "create", "other"}, {"env", "rm", "work"}} {
		if code, _, errs := runIt(argv...); code != 1 || !strings.Contains(errs, "usage: caboose env [list] ('caboose -e NAME setup' creates an environment)") {
			t.Errorf("%v: exit %d\n%s", argv, code, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".caboose", "envs", "other")); err == nil {
		t.Error("env create made an environment")
	}
}

// noTerminal makes stdin something that is not a terminal, whatever the
// test runs under.
func noTerminal(t *testing.T) {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

// Setup asks questions, so with no terminal it refuses before it creates
// or writes anything -- a new environment included, which it runs for
// though CheckEnv would refuse it.
func TestSetupNeedsATerminal(t *testing.T) {
	log := fakeDocker(t)
	home := sandboxEnv(t) // setup git needs no repo root
	noTerminal(t)
	for _, argv := range [][]string{{"setup"}, {"-e", "work", "setup"}, {"setup", "git"}} {
		code, _, errs := runIt(argv...)
		if code != 1 || !strings.Contains(errs, "caboose setup asks questions, and there is no terminal to ask them on") ||
			!strings.Contains(errs, "Nothing was changed.") {
			t.Errorf("%v: exit %d\n%s", argv, code, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".caboose")); err == nil {
		t.Error("setup wrote into CABOOSE_HOME")
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("docker was run: %q", b)
	}
}

func TestSetupArgs(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	noTerminal(t)
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"setup", "--env", "work"}, "the environment goes before the command: 'caboose -e work setup'"},
		{[]string{"setup", "-e", "work", "git"}, "the environment goes before the command: 'caboose -e work setup'"},
		{[]string{"setup", "--env=work"}, "the environment goes before the command: 'caboose -e work setup'"},
		{[]string{"setup", "--env"}, "the environment goes before the command: 'caboose -e NAME setup'"},
		{[]string{"setup", "login"}, "usage: caboose setup [roots | image | isolation | git | sync]... ('login' is not a section)"},
		{[]string{"setup", "git", "git"}, "('git' is named twice)"},
	} {
		if code, _, errs := runIt(tc.argv...); code != 1 || !strings.Contains(errs, tc.want) {
			t.Errorf("%v: exit %d\n%s", tc.argv, code, errs)
		}
	}
}

// A mistyped --env must not start a new, empty sandbox.
func TestUnknownEnvIsRefused(t *testing.T) {
	log := fakeDocker(t)
	sandboxEnv(t)
	for _, argv := range [][]string{{"--env", "wrok", "status"}, {"-e", "wrok"}, {"--env=wrok", "claude", "-p", "hi"}} {
		code, _, errs := runIt(argv...)
		if code != 1 || !strings.Contains(errs, "there is no environment 'wrok'") || !strings.Contains(errs, "create it with 'caboose -e wrok setup'") {
			t.Errorf("%v: exit %d\n%s", argv, code, errs)
		}
	}
	t.Setenv("CABOOSE_ENV", "wrok")
	if code, _, errs := runIt("status"); code != 1 || !strings.Contains(errs, "there is no environment 'wrok'") {
		t.Errorf("CABOOSE_ENV: exit %d\n%s", code, errs)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("docker was run: %q", b)
	}
}

func TestStatusShowsTheEnv(t *testing.T) {
	fakeDocker(t)
	home := sandboxEnv(t)
	mkEnv(t, home, "work")
	_, out, _ := runIt("--env", "work", "status")
	if !strings.HasPrefix(out, "env       : work\ncontainer : caboose-work (") {
		t.Errorf("stdout:\n%s", out)
	}
	// The environment's config.toml is the one read.
	if !strings.Contains(out, "\nconfig    : "+filepath.Join(home, ".caboose", "envs", "work", "config.toml")+"\n") {
		t.Errorf("stdout:\n%s", out)
	}
}
