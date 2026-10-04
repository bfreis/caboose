package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
)

func TestIncludesDropIns(t *testing.T) {
	for conf, want := range map[string]bool{
		"Include /etc/ssh/ssh_config.d/*.conf\n\nHost *\n    SendEnv LANG LC_*\n": true, // Debian's
		"# comment\ninclude ssh_config.d/*.conf\n":                                true,
		"Include \"/etc/ssh/ssh_config.d/*.conf\"\n":                              true,
		"#Include /etc/ssh/ssh_config.d/*.conf\n":                                 false,
		"Host *\n    ForwardAgent no\n":                                           false,
		"Include /etc/ssh/other.d/*.conf\n":                                       false,
		"":                                                                        false,
	} {
		if got := includesDropIns([]byte(conf)); got != want {
			t.Errorf("includesDropIns(%q) = %v", conf, got)
		}
	}
}

// With the system's config including drop-ins, the agent writes its own:
// every host but the guest's loopback through its connect, and the
// environment is left as it was. ~/.ssh is never touched.
func TestSSHEgressDropIn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sshSystemConf), []byte("Include /etc/ssh/ssh_config.d/*.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=/root"}
	got, did, err := sshEgress(root, env, agentproto.GuestAgent, "caboose", fixedSSH)
	if err != nil || !slices.Equal(got, env) || !strings.Contains(did, SSHEgressConf) {
		t.Fatalf("env %q, did %q, err %v", got, did, err)
	}
	b, err := os.ReadFile(filepath.Join(root, SSHEgressConf))
	if err != nil {
		t.Fatal(err)
	}
	conf := string(b)
	for _, want := range []string{
		"\nHost * !localhost !*.localhost !127.* !::1 !caboose !172.16.* !172.17.* ",
		" !172.30.* !172.31.*\n",
		"\n    ProxyCommand /run/caboose/agent connect %h %p\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("drop-in lacks %q:\n%s", want, conf)
		}
	}
	for _, l := range strings.Split(conf, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "Host ") && !strings.HasPrefix(l, "    ProxyCommand ") {
			t.Errorf("unexpected line %q", l)
		}
	}
	if fi, err := os.Stat(filepath.Join(root, SSHEgressConf)); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, %v", fi.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "root/.ssh")); err == nil {
		t.Error("~/.ssh was made")
	}
	// Again at the next boot: the same file, rewritten.
	if _, _, err := sshEgress(root, env, agentproto.GuestAgent, "caboose", fixedSSH); err != nil {
		t.Fatal(err)
	}
}

// Without the include, or without an ssh_config at all, git's ssh goes
// through the proxy by GIT_SSH_COMMAND, unless the image sets one.
func TestSSHEgressFallback(t *testing.T) {
	root := t.TempDir()
	env := []string{"HOME=/root"}
	got, did, err := sshEgress(root, env, agentproto.GuestAgent, "caboose", fixedSSH)
	want := append(slices.Clone(env), "GIT_SSH_COMMAND=ssh -o 'ProxyCommand=/run/caboose/agent connect %h %p'")
	if err != nil || !slices.Equal(got, want) || !strings.Contains(did, "GIT_SSH_COMMAND") {
		t.Errorf("env %q, did %q, err %v", got, did, err)
	}
	if _, err := os.Stat(filepath.Join(root, SSHEgressConf)); err == nil {
		t.Error("a drop-in nothing reads was written")
	}
	own := []string{"GIT_SSH_COMMAND=ssh -i /k"}
	if got, did, err := sshEgress(root, own, agentproto.GuestAgent, "caboose", fixedSSH); err != nil || !slices.Equal(got, own) || did != "" {
		t.Errorf("an image's own GIT_SSH_COMMAND: env %q, did %q, err %v", got, did, err)
	}
}

const fixedSSH = "OpenSSH_10.2p1 Ubuntu-2ubuntu3.6, OpenSSL 3.5.5 27 Jan 2026\n"

func TestSSHFixed(t *testing.T) {
	for v, want := range map[string]bool{
		fixedSSH: true,
		"OpenSSH_9.6p1 Ubuntu-3ubuntu13.5, OpenSSL 3.0.13 30 Jan 2024": true,
		"OpenSSH_9.9p1, OpenSSL 3.3.2 3 Sep 2024":                      true, // Alpine
		"OpenSSH_10.0p2 Debian-7, OpenSSL 3.5.1 1 Jul 2025":            true,
		"OpenSSH_9.2p1 Debian-2+deb12u3, OpenSSL 3.0.15 3 Sep 2024":    true,
		"OpenSSH_9.2p1 Debian-2+deb12u2, OpenSSL 3.0.11 19 Sep 2023":   true,
		"OpenSSH_9.2p1 Debian-2+deb12u1, OpenSSL 3.0.9 30 May 2023":    false,
		"OpenSSH_9.2p1 Debian-2, OpenSSL 3.0.8 7 Feb 2023":             false,
		"OpenSSH_8.9p1 Ubuntu-3ubuntu0.10, OpenSSL 3.0.2 15 Mar 2022":  true,
		"OpenSSH_8.9p1 Ubuntu-3ubuntu0.4, OpenSSL 3.0.2 15 Mar 2022":   false,
		"OpenSSH_9.5p1, OpenSSL 3.1.4 24 Oct 2023":                     false,
		"OpenSSH_8.4p1 Debian-5+deb11u3, OpenSSL 1.1.1w 11 Sep 2023":   false, // not in the table
		"OpenSSH_9.3p1 Debian-2+deb12u9":                               false, // another version's revision
		"Sun_SSH_1.1":                                                  false,
		"":                                                             false,
	} {
		if got, what := sshFixed(v); got != want || what == "" {
			t.Errorf("sshFixed(%q) = %v, %q", v, got, what)
		}
	}
}

// An ssh before 9.6 without the fix, or none, is left on the VM's own
// network: neither the drop-in nor GIT_SSH_COMMAND, and the console says
// why.
func TestSSHEgressOldSSH(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sshSystemConf), []byte("Include /etc/ssh/ssh_config.d/*.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=/root"}
	got, note, err := sshEgress(root, env, agentproto.GuestAgent, "caboose", "OpenSSH_9.2p1 Debian-2, OpenSSL 3.0.8 7 Feb 2023")
	if err != nil || !slices.Equal(got, env) || !strings.Contains(note, "CVE-2023-51385") || !strings.Contains(note, "OpenSSH 9.2 (Debian-2)") {
		t.Errorf("env %q, note %q, err %v", got, note, err)
	}
	if _, err := os.Stat(filepath.Join(root, SSHEgressConf)); err == nil {
		t.Error("a drop-in was written for an old ssh")
	}
	if got, note, err := sshEgress(t.TempDir(), env, agentproto.GuestAgent, "caboose", "OpenSSH_8.0p1"); err != nil || !slices.Equal(got, env) || !strings.Contains(note, "CVE") {
		t.Errorf("fallback for an old ssh: env %q, note %q, err %v", got, note, err)
	}
	if got, note, err := sshEgress(root, env, agentproto.GuestAgent, "caboose", ""); err != nil || !slices.Equal(got, env) || note != "" {
		t.Errorf("no ssh: env %q, note %q, err %v", got, note, err)
	}
}

// A hostname that is no plain name stays out of the drop-in.
func TestSSHEgressHosts(t *testing.T) {
	if h := sshEgressHosts("Box-1"); !strings.Contains(h, " !box-1 ") {
		t.Errorf("%s", h)
	}
	for _, bad := range []string{"", "a b", "x\nProxyCommand evil", "a*"} {
		if h := sshEgressHosts(bad); strings.Contains(h, "\n") || strings.Contains(h, "evil") || strings.Contains(h, "a*") || strings.Contains(h, "!a ") {
			t.Errorf("sshEgressHosts(%q) = %q", bad, h)
		}
	}
}

// The link is served once the entrypoint has started, before it is ready.
func TestLinkableOnceStarted(t *testing.T) {
	m := &machine{}
	if m.linkable() != nil {
		t.Fatal("linkable before any boot")
	}
	started := &agentproto.BootSpec{Hostname: "started"}
	m.started = started
	if m.linkable() != started || m.booted() != nil {
		t.Error("not linkable once started, or booted before ready")
	}
	ready := &agentproto.BootSpec{Hostname: "ready"}
	m.spec = ready
	if m.linkable() != ready {
		t.Error("the ready boot is not the linked one")
	}
}

// The image's own ssh, where there is one, says a version sshFixed reads.
func TestSSHVersion(t *testing.T) {
	v := sshVersion([]string{"PATH=" + os.Getenv("PATH")})
	if v == "" {
		t.Skip("no ssh here")
	}
	if _, what := sshFixed(v); !strings.HasPrefix(what, "OpenSSH ") {
		t.Errorf("ssh -V says %q, read as %q", v, what)
	}
	if sshVersion([]string{"PATH=/nonexistent"}) != "" {
		t.Error("an ssh found on no PATH")
	}
}
