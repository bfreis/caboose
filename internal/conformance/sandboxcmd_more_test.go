package conformance_test

import (
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
)

func TestManagedCanaryHasASandboxPath(t *testing.T) {
	for _, rel := range []string{"home/envs/cf-00aa11bb/data/claude-code/conf-0a1b2c3d-managed", "data/claude-code/conf-0a1b2c3d-managed"} {
		p, err := c.SandboxPath(c.Canary{Rel: rel})
		if err != nil || p.String() != "/etc/claude-code/conf-0a1b2c3d-managed" {
			t.Fatalf("%s: %v %v", rel, p, err)
		}
	}
	p, _ := c.SandboxPath(c.Canary{Rel: "data/claude-code/conf-0a1b2c3d-managed"})
	for _, rel := range []string{"data/claude-code/notes", "data/claude-code/conf-0a1b2c3d-a/b", "data/home/conf-0a1b2c3d-a", "data/claude-code",
		"home/envs/default/data/claude-code/conf-0a1b2c3d-a", "home/envs/cf-00aa11bb/data/home/conf-0a1b2c3d-a", "x/data/claude-code/conf-0a1b2c3d-a"} {
		if _, err := c.SandboxPath(c.Canary{Rel: rel}); err == nil {
			t.Errorf("%s was accepted", rel)
		}
	}
	if _, err := p.Child("x"); err != nil {
		t.Errorf("a child of a managed canary: %v", err)
	}
	if _, err := p.Root(); err == nil {
		t.Error("a managed canary is in no root")
	}
}

func TestRootAndMoreBuilders(t *testing.T) {
	p, err := c.SandboxPath(c.Canary{Rel: "roots/conf-0a1b2c3d-dev/part/conf-0a1b2c3d-wr"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := p.Root()
	if err != nil || root.String() != "/work/conf-0a1b2c3d-dev" {
		t.Fatalf("%v %v", root, err)
	}
	for name, mk := range map[string]func() (c.Cmd, error){
		"rmdir":   func() (c.Cmd, error) { return c.Rmdir(p) },
		"rename":  func() (c.Cmd, error) { return c.Rename(p) },
		"bind":    func() (c.Cmd, error) { return c.BindOver(root) },
		"dotdot":  func() (c.Cmd, error) { return c.AppendDotDot(p) },
		"procfs":  func() (c.Cmd, error) { return c.AppendProc(p) },
		"zero":    func() (c.Cmd, error) { return c.Rename(c.SPath{}) },
		"zero-pc": func() (c.Cmd, error) { return c.AppendProc(c.SPath{}) },
	} {
		cmd, err := mk()
		if strings.HasPrefix(name, "zero") {
			if err == nil {
				t.Errorf("%s: accepted a path the suite did not make", name)
			}
			continue
		}
		if err != nil || cmd.Body() == "" {
			t.Errorf("%s: %v", name, err)
		}
		if strings.ContainsAny(cmd.Body(), "*?`~$") {
			t.Errorf("%s: %q", name, cmd.Body())
		}
	}
	if cmd, _ := c.AppendDotDot(p); cmd.Body() != "echo x >> /work/conf-0a1b2c3d-dev/../conf-0a1b2c3d-dev/part/conf-0a1b2c3d-wr" {
		t.Errorf("dotdot: %q", cmd.Body())
	}
	if _, err := (c.SPath{}).Root(); err == nil {
		t.Error("the zero path has a root")
	}
}

func TestProbeMount(t *testing.T) {
	const nonce = "0a1b2c3d" + "0123456789abcdef01234567"
	cn := c.Canary{Rel: "roots/conf-0a1b2c3d-dev/part/conf-0a1b2c3d-x"}
	p, err := c.SandboxPath(cn)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := p.MountPoint()
	dev, _ := p.Root()
	inPart, _ := part.Child("conf-0a1b2c3d-x")
	cmd, err := c.ProbeMount(nonce, "umount", part)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/work/conf-0a1b2c3d-dev/part/.conformance-probe --nonce " + nonce + " --mount umount --target /work/conf-0a1b2c3d-dev/part 1>&2"; cmd.Body() != want || !cmd.Destructive() {
		t.Errorf("%q destructive=%v", cmd.Body(), cmd.Destructive())
	}
	if _, err := c.ProbeMount(nonce, "remount-bind-rw", dev); err != nil {
		t.Errorf("the dev root: %v", err)
	}
	for name, f := range map[string]func() (c.Cmd, error){
		"below a mount point": func() (c.Cmd, error) { return c.ProbeMount(nonce, "umount", inPart) },
		"another run's":       func() (c.Cmd, error) { return c.ProbeMount("ffffffff"+nonce[8:], "umount", part) },
		"an unknown op":       func() (c.Cmd, error) { return c.ProbeMount(nonce, "mount --bind", part) },
		"not a nonce":         func() (c.Cmd, error) { return c.ProbeMount("x; y", "umount", part) },
		"no path":             func() (c.Cmd, error) { return c.ProbeMount(nonce, "umount", c.SPath{}) },
	} {
		if cmd, err := f(); err == nil {
			t.Errorf("%s: %q", name, cmd.Body())
		}
	}
	if m := c.MountTable(); m.Destructive() || strings.Contains(m.Body(), "$") {
		t.Errorf("%q", m.Body())
	}
}
