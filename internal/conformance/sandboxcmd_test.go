package conformance_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
)

// every command the builders can emit, for sample canaries of both roots.
func allCommands(t *testing.T) []c.Cmd {
	t.Helper()
	f, _ := newWorld(t, isoContainer)
	var out []c.Cmd
	add := func(cmd c.Cmd, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, cmd)
	}
	path := func(role string) c.SPath {
		cn, err := f.Canary(role)
		if err != nil {
			t.Fatal(err)
		}
		p, err := c.SandboxPath(cn)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, role := range []string{"ro-file", "wr-file", "parent-file", "ro-dir", "wr-dir", "ro-tree", "wr-tree"} {
		p := path(role)
		for _, b := range []func(c.SPath) (c.Cmd, error){c.Append, c.Truncate, c.Touch, c.Chmod, c.Read, c.List, c.Remove, c.RemoveTree} {
			add(b(p))
		}
	}
	for _, role := range []string{"ro-dir", "wr-dir"} {
		for _, b := range []func(c.SPath) (c.Cmd, error){c.MkdirIn, c.SymlinkIn} {
			add(b(path(role)))
		}
	}
	for _, role := range []string{"ro-tree", "wr-tree"} {
		for _, b := range []func(c.SPath) (c.Cmd, error){c.HardlinkIn, c.MoveIn} {
			add(b(path(role)))
		}
	}
	mp, err := path("wr-dir").MountPoint()
	if err != nil {
		t.Fatal(err)
	}
	add(c.Umount(mp))
	add(c.RemountRW(mp))
	add(c.AppendThrough(path("ro-file"), path("wr-link-s")))
	add(c.UmountThenAppend(mp, path("parent-file")))
	nested, err := c.Append(path("wr-file"))
	if err != nil {
		t.Fatal(err)
	}
	add(c.ShellOf(nested))
	return out
}

var literalPath = regexp.MustCompile(`^/work/conf-[0-9a-f]{8}-(ro|dev)(/part)?(/conf-[0-9a-f]{8}-[a-z0-9-]+(/[a-z0-9][a-z0-9.-]*)*)?$`)

func TestCommandStrings(t *testing.T) {
	cmds := allCommands(t)
	if len(cmds) < 60 {
		t.Fatalf("only %d commands", len(cmds))
	}
	for _, cmd := range cmds {
		argv := cmd.Argv()
		script := argv[2]
		body := cmd.Body()
		// The wrapper's rc=$? is the one dollar sign.
		if n := strings.Count(script, "$"); n != 1 || strings.Contains(body, "$") {
			t.Errorf("%q: %d dollar signs", script, n)
		}
		for _, bad := range []string{"*", "?", "[", "]", "`", "~", "{", "}", "|", ";", "\\", "\"", "\n"} {
			if strings.Contains(strings.ReplaceAll(body, "&&", ""), bad) {
				t.Errorf("%q: contains %q", body, bad)
			}
		}
		for _, tok := range strings.Fields(strings.NewReplacer("'", " ").Replace(body)) {
			if strings.HasPrefix(tok, "/") && !literalPath.MatchString(tok) {
				t.Errorf("%q: %q is not a literal canary path", body, tok)
			}
		}
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("sh -n rejected %q: %v %s", script, err, out)
		}
		if b, ok := c.Unwrap(argv); !ok || b != body {
			t.Errorf("Unwrap(%q) = %q, %v", script, b, ok)
		}
	}
}

// No builder a check may call emits a bare "/" argument or the wipe's
// flags: the wipe is built only inside the package (sandboxcmd_wipe_test.go).
func TestNoBuilderEmitsTheWipe(t *testing.T) {
	for _, cmd := range allCommands(t) {
		for _, tok := range strings.Fields(cmd.Body()) {
			if tok == "/" || tok == "--no-preserve-root" || tok == "-rf" {
				t.Errorf("%q has %q", cmd.Body(), tok)
			}
		}
	}
}

func TestBuildersRefuseForgedPaths(t *testing.T) {
	if _, err := c.Remove(c.SPath{}); err == nil {
		t.Error("an empty path was accepted")
	}
	f, _ := newWorld(t, isoContainer)
	for _, rel := range []string{"canary/outside", "roots/dev/x", "roots/conf-0a1b2c3d-ro/../../x", "roots/conf-0a1b2c3d-ro/conf-0a1b2c3d-a b",
		"roots/conf-0a1b2c3d-ro/conf-0a1b2c3d-$x", "home/x", "roots/conf-0a1b2c3d-ro/*"} {
		if p, err := c.SandboxPath(c.Canary{Rel: rel}); err == nil {
			t.Errorf("%q gave %q", rel, p)
		}
	}
	cn, _ := f.Canary("ro-file")
	p, _ := c.SandboxPath(cn)
	for _, name := range []string{"", "../x", "a b", "$x", "*", "a/b/../c", ".x"} {
		if q, err := p.Child(name); err == nil && !literalPath.MatchString(q.String()) {
			t.Errorf("child %q gave %q", name, q)
		}
	}
	if _, err := p.MountPoint(); err == nil {
		t.Error("a mount point for a path in a read-only root")
	}
}

func TestSandboxPaths(t *testing.T) {
	f, _ := newWorld(t, isoContainer)
	cn, _ := f.Canary("wr-file")
	p, err := c.SandboxPath(cn)
	if err != nil || p.String() != "/work/conf-0a1b2c3d-dev/part/conf-0a1b2c3d-wr-file" {
		t.Errorf("%v %v", p, err)
	}
	if _, err := c.SandboxPath(c.Canary{Rel: "canary/conf-0a1b2c3d-outside"}); err == nil {
		t.Error("a canary outside every root has a sandbox path")
	}
}
