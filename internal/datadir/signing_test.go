package datadir

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHv0pK3lkF1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7 me@mac"

// 1Password's setup, as it writes it into the host's ~/.gitconfig.
func onePassword() map[string]string {
	return map[string]string{
		"gpg.format":      "ssh",
		"user.signingkey": pubKey,
		"gpg.ssh.program": "/Applications/1Password.app/Contents/MacOS/op-ssh-sign",
		"commit.gpgsign":  "true",
	}
}

func TestLiteralSSHKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pub := filepath.Join(home, ".ssh", "id_ed25519.pub")
	priv := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(pub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pub, []byte(pubKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nxx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key, want, skipped string
	}{
		{"literal", pubKey, pubKey, ""},
		{"surrounding space", "  " + pubKey + "\n", pubKey, ""},
		{"key:: prefix", "key::" + pubKey, pubKey, ""},
		{"a .pub file", pub, pubKey, ""},
		{"a .pub file under ~", "~/.ssh/id_ed25519.pub", pubKey, ""},
		{"a private key file", priv, "", "is a private key file"},
		{"a missing file", filepath.Join(home, "nope.pub"), "", "cannot be read"},
		{"a security key", "sk-ssh-ed25519@openssh.com AAAAGnNr me", "sk-ssh-ed25519@openssh.com AAAAGnNr me", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, skipped := LiteralSSHKey(tc.key)
			if key != tc.want || !strings.Contains(skipped, tc.skipped) || (tc.skipped == "") != (skipped == "") {
				t.Errorf("LiteralSSHKey = %q, %q; want key %q, skipped %q", key, skipped, tc.want, tc.skipped)
			}
		})
	}
}

// End to end with the real tools: a host config in 1Password's shape, kept
// in an included file; the host's identity and key written into the
// sandbox as caboose setup writes them; then a commit signed using nothing
// but the sandbox's config and an agent holding the key -- no private key
// on disk, no op-ssh-sign.
func TestSigningRealGitSigns(t *testing.T) {
	g := FindGit()
	if g == nil {
		t.Skip("no git on PATH")
	}
	for _, tool := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	sock := filepath.Join(tmp, "agent.sock")
	agent := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })
	t.Setenv("SSH_AUTH_SOCK", sock)
	key := filepath.Join(tmp, "k")
	for _, args := range [][]string{
		{"ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "me@mac", "-f", key},
		{"ssh-add", "-q", key},
	} {
		// ssh-add may race the agent's socket coming up.
		var out []byte
		var err error
		for i := 0; i < 50; i++ {
			if out, err = exec.Command(args[0], args[1:]...).CombinedOutput(); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key); err != nil { // only the agent has it now
		t.Fatal(err)
	}

	host := filepath.Join(tmp, "host.gitconfig")
	inc := filepath.Join(tmp, "signing.inc")
	if err := os.WriteFile(host, []byte("[user]\n\tname = Me\n\temail = me@mac\n[include]\n\tpath = "+inc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inc, []byte("[gpg]\n\tformat = ssh\n[gpg \"ssh\"]\n\tprogram = /Applications/1Password.app/Contents/MacOS/op-ssh-sign\n"+
		"[user]\n\tsigningkey = "+strings.TrimSpace(string(pub))+"\n[commit]\n\tgpgsign = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", host)

	data := filepath.Join(tmp, "data")
	mkGitConfig(t, data)
	id := HostIdentity(g)
	key, skipped := HostSigningKey(g)
	if key == "" {
		t.Fatalf("HostSigningKey: %q (the include was not followed?)", skipped)
	}
	changes := append([]Change{{Key: "user.name", Value: id.Name}, {Key: "user.email", Value: id.Email}},
		append(SigningChanges(key), Change{Key: "commit.gpgsign", Value: "true"})...)
	if err := WriteSandboxGit(data, g, changes); err != nil {
		t.Fatal(err)
	}
	if got := g.Get(filepath.Join(data, GitConfig), "gpg.ssh.program"); got != "" {
		t.Errorf("gpg.ssh.program came along: %q", got)
	}

	// Now as the sandbox: its config is the global one.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(data, GitConfig))
	repo := filepath.Join(tmp, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "commit", "-q", "--allow-empty", "-m", "signed in the sandbox"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	allowed := filepath.Join(tmp, "allowed")
	if err := os.WriteFile(allowed, []byte("me@mac "+string(pub)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", "HEAD").CombinedOutput()
	if err != nil || !strings.Contains(string(out), `Good "git" signature for me@mac`) {
		t.Errorf("verify-commit: %v\n%s", err, out)
	}
}

// GetGlobal is the host's default: a plain include is followed, but an
// includeIf is not, even from inside the repo it would match -- the
// directory caboose happens to be launched in must not decide the
// sandbox's identity.
func TestGetGlobalIgnoresConditionalIncludes(t *testing.T) {
	g := FindGit()
	if g == nil {
		t.Skip("no git on PATH")
	}
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := filepath.Join(tmp, "work", "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	write := func(name, body string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plain := write("plain.inc", "[user]\n\tname = From Include\n")
	cond := write("cond.inc", "[user]\n\temail = work@example.invalid\n")
	host := write("host.gitconfig", "[user]\n\temail = default@example.invalid\n"+
		"[include]\n\tpath = "+plain+"\n"+
		"[includeIf \"gitdir:"+filepath.Join(tmp, "work")+"/\"]\n\tpath = "+cond+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", host)
	t.Chdir(repo)
	if out, _ := exec.Command("git", "config", "--includes", "--get", "user.email").Output(); strings.TrimSpace(string(out)) != "work@example.invalid" {
		t.Fatalf("fixture: the includeIf does not match in the repo (%q)", out)
	}
	if got := g.GetGlobal("user.email"); got != "default@example.invalid" {
		t.Errorf("user.email = %q: the includeIf for the cwd leaked in", got)
	}
	if got := g.GetGlobal("user.name"); got != "From Include" {
		t.Errorf("user.name = %q: the plain include was not followed", got)
	}
}

func TestHostSigningKey(t *testing.T) {
	for _, tc := range []struct {
		name         string
		global       map[string]string
		key, skipped string
	}{
		{"1Password", onePassword(), pubKey, ""},
		{"gpg, not ssh", map[string]string{"user.signingkey": "ABCD1234"}, "", ""},
		{"ssh, no key", map[string]string{"gpg.format": "ssh"}, "", ""},
		{"a private key file", map[string]string{"gpg.format": "SSH", "user.signingkey": "/nonexistent/id"}, "", "cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, skipped := HostSigningKey(&fakeGit{global: tc.global})
			if key != tc.key || !strings.Contains(skipped, tc.skipped) || (tc.skipped == "") != (skipped == "") {
				t.Errorf("HostSigningKey = %q, %q; want %q, %q", key, skipped, tc.key, tc.skipped)
			}
		})
	}
	if key, skipped := HostSigningKey(nil); key != "" || skipped != "" {
		t.Errorf("with no git: %q, %q", key, skipped)
	}
}

func TestReadSandboxSigning(t *testing.T) {
	dir := t.TempDir()
	dst := mkGitConfig(t, dir)
	g := &fakeGit{}
	if s, err := ReadSandboxSigning(dir, g); err != nil || s != (SandboxSigning{}) {
		t.Fatalf("with no config: %+v, %v", s, err)
	}
	data := "gpg.format=ssh\nuser.signingkey=key::" + pubKey + "\ncommit.gpgsign=true\n"
	if err := os.WriteFile(dst, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSandboxSigning(dir, g)
	if err != nil {
		t.Fatal(err)
	}
	if s.Format != "ssh" || s.PublicKey() != pubKey || !s.Signs() {
		t.Errorf("ReadSandboxSigning = %+v (public key %q, signs %v)", s, s.PublicKey(), s.Signs())
	}
	if got, _ := os.ReadFile(dst); string(got) != data || g.sets != 0 {
		t.Errorf("reading changed the config: %q, %d sets", got, g.sets)
	}
	if (SandboxSigning{Key: "~/.ssh/id.pub"}).PublicKey() != "" || (SandboxSigning{CommitSign: "false"}).Signs() {
		t.Error("a key path has no public key here, and false does not sign")
	}

	// A link where the config should be is not read through.
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(other, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dst); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadSandboxSigning(dir, g); err == nil || s.Key != "" {
		t.Errorf("through a symlink: %+v, %v", s, err)
	}
}

func TestSigningChanges(t *testing.T) {
	dir := t.TempDir()
	dst := mkGitConfig(t, dir)
	if err := os.WriteFile(dst, []byte("user.name=Me\ntag.gpgsign=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &fakeGit{}
	if err := WriteSandboxGit(dir, g, append(SigningChanges(pubKey), Change{Key: "commit.gpgsign", Value: "true"})); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSandboxGit(dir, g)
	if err != nil || s.Signing.Format != "ssh" || s.Signing.PublicKey() != pubKey || !s.Signing.Signs() {
		t.Fatalf("after SigningChanges: %+v, %v", s, err)
	}
	if err := WriteSandboxGit(dir, g, NoSigningChanges()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst); got != "user.name=Me\n" {
		t.Errorf("after NoSigningChanges: %q", got)
	}
}
