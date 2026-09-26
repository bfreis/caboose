package imagecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
)

// These run the real imagecheck.sh with the local shells, the way docker
// runs it (sh -c SCRIPT caboose-probe UID GID), against a fake image: a
// CABOOSE_PROBE_ROOT holding the files the probe looks for, and a PATH of
// stubs for the commands. Removing a file or a stub simulates an image
// without it.

func probeScript(t *testing.T) string {
	t.Helper()
	b, err := assets.ProbeScript()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// shells are the POSIX shells to run the probe with: /bin/sh always, and
// whichever of the others this machine has, since images differ in theirs
// (dash on Debian, BusyBox ash on Alpine).
func shells(t *testing.T) [][]string {
	t.Helper()
	out := [][]string{{"/bin/sh"}}
	for _, s := range []string{"dash", "ash", "mksh", "yash", "posh"} {
		if p, err := exec.LookPath(s); err == nil {
			out = append(out, []string{p})
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		out = append(out, []string{p, "--posix"})
	}
	if p, err := exec.LookPath("busybox"); err == nil {
		out = append(out, []string{p, "sh"})
	}
	return out
}

type fakeImage struct {
	t         *testing.T
	root, bin string
}

func (f *fakeImage) file(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeImage) stub(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

// real puts the host's own tool on the fake PATH, for the ones the probe
// tries out rather than just finds.
func (f *fakeImage) real(name string) {
	f.t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		f.t.Skipf("%s not installed", name)
	}
	if err := os.Symlink(p, filepath.Join(f.bin, name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeImage) remove(rel string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.root, rel)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeImage) unstub(name string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.bin, name)); err != nil {
		f.t.Fatal(err)
	}
}

// newFake is a complete image: glibc on aarch64 unless changed, every tool,
// the CA bundle, and a network that answers.
func newFake(t *testing.T) *fakeImage {
	t.Helper()
	f := &fakeImage{t: t, root: t.TempDir(), bin: t.TempDir()}
	f.file("lib/aarch64-linux-gnu/libc.so.6", "")
	f.file("usr/bin/env", "")
	f.file("etc/ssl/certs/ca-certificates.crt", "-----BEGIN CERTIFICATE-----\n")
	f.file("etc/passwd", "root:x:0:0:root:/root:/bin/sh\n")
	f.file("etc/group", "root:x:0:\n")
	for _, s := range []string{"bash", "curl", "tmux", "tic", "rg", "sleep", "test", "mkdir", "chmod",
		"cut", "sed", "tr", "grep", "head", "sha256sum", "chown"} {
		f.stub(s, "exit 0")
	}
	f.stub("uname", "echo aarch64")
	f.stub("git", "echo git version 2.53.0")
	f.stub("ldd", `echo "	linux-vdso.so.1 (0x0000ffff)"`)
	for _, r := range []string{"readlink", "mktemp", "find", "rm", "rmdir"} {
		f.real(r)
	}
	return f
}

// musl makes the fake an Alpine-like image with the musl build's needs.
func (f *fakeImage) musl() {
	f.remove("lib/aarch64-linux-gnu/libc.so.6")
	f.file("lib/libc.musl-aarch64.so.1", "")
	f.file("usr/lib/libgcc_s.so.1", "")
	f.file("usr/lib/libstdc++.so.6", "")
}

func (f *fakeImage) run(shell []string, uid, gid int) string {
	f.t.Helper()
	args := append(append([]string{}, shell[1:]...), "-c", probeScript(f.t), "caboose-probe", strconv.Itoa(uid), strconv.Itoa(gid))
	cmd := exec.Command(shell[0], args...)
	cmd.Env = []string{"PATH=" + f.bin, "CABOOSE_PROBE_ROOT=" + f.root, "TMPDIR=" + f.t.TempDir()}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("%v: %v\n%s", shell, err, stderr.String())
	}
	if stderr.Len() != 0 {
		f.t.Errorf("%v wrote to stderr: %s", shell, stderr.String())
	}
	return string(out)
}

// probe runs the fake with every shell, checks they all agree, and parses.
func (f *fakeImage) probe(uid, gid int) *Report {
	f.t.Helper()
	var first string
	for i, sh := range shells(f.t) {
		out := f.run(sh, uid, gid)
		if i == 0 {
			first = out
		} else if out != first {
			f.t.Errorf("%v disagrees with /bin/sh:\n%s\nvs\n%s", sh, out, first)
		}
	}
	r, err := Parse(first)
	if err != nil {
		f.t.Fatalf("%v\n%s", err, first)
	}
	return r
}

func problemNames(r *Report) []string {
	var names []string
	for _, p := range r.Problems() {
		names = append(names, p[:strings.Index(p, ":")])
	}
	return names
}

func TestProbeCompleteGlibc(t *testing.T) {
	f := newFake(t)
	r := f.probe(1000, 1000)
	if !r.OK() {
		t.Errorf("problems %q", r.Problems())
	}
	if r.Libc != "glibc" || r.Platform != "linux-arm64" || r.Arch != "aarch64" {
		t.Errorf("libc %q platform %q arch %q", r.Libc, r.Platform, r.Arch)
	}
	if c, _ := r.Check("libc"); c.Detail != "glibc /lib/aarch64-linux-gnu/libc.so.6" {
		t.Errorf("libc detail %q", c.Detail)
	}
	if c, _ := r.Check("bash"); c.Detail != filepath.Join(f.bin, "bash") {
		t.Errorf("bash detail %q", c.Detail)
	}
	if r.UID != 1000 || r.UIDOwner != "" || r.GIDOwner != "" || len(r.Warnings) != 0 {
		t.Errorf("%+v", r)
	}
	// No musl checks on glibc.
	if _, ok := r.Check("libgcc"); ok {
		t.Error("libgcc checked on glibc")
	}
}

// Every check the probe prints (on musl, where it prints the most) is in the
// requirements table, and every requirement is something it prints: a name
// typo on either side would make a requirement impossible to meet, or leave
// a check nobody reads.
func TestProbeMatchesRequirements(t *testing.T) {
	f := newFake(t)
	f.musl()
	r := f.probe(1000, 1000)
	var got []string
	for _, c := range r.Checks {
		if c.Name != "net" {
			got = append(got, c.Name)
		}
	}
	var want []string
	for _, req := range requirements {
		want = append(want, req.name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("probe checks %v\nrequirements %v", got, want)
	}
}

func TestProbeMusl(t *testing.T) {
	for _, tc := range []struct{ uname, platform string }{
		{"aarch64", "linux-arm64-musl"}, {"x86_64", "linux-x64-musl"}, {"amd64", "linux-x64-musl"}, {"arm64", "linux-arm64-musl"},
	} {
		t.Run(tc.uname, func(t *testing.T) {
			f := newFake(t)
			f.musl()
			f.stub("uname", "echo "+tc.uname)
			r := f.probe(1000, 1000)
			if !r.OK() || r.Libc != "musl" || r.Platform != tc.platform {
				t.Errorf("libc %q platform %q problems %q", r.Libc, r.Platform, r.Problems())
			}
		})
	}
}

// The installer's third musl test: ldd saying so, with no
// /lib/libc.musl-*.so.1 (and a glibc-looking file that must not win).
func TestProbeMuslByLdd(t *testing.T) {
	f := newFake(t)
	f.stub("ldd", `echo "musl libc (aarch64)" >&2; exit 1`)
	r := f.probe(1000, 1000)
	if r.Libc != "musl" || r.Platform != "linux-arm64-musl" {
		t.Errorf("libc %q platform %q", r.Libc, r.Platform)
	}
	if c, _ := r.Check("libc"); c.Detail != "musl ldd /bin/ls" {
		t.Errorf("libc detail %q", c.Detail)
	}
	// And it now wants the musl libraries, which this fake lacks.
	if got := problemNames(r); !slices.Equal(got, []string{"libgcc", "libstdc++"}) {
		t.Errorf("problems %v", got)
	}
}

// alpine as it comes, modulo the stubs: BusyBox tools, the CA bundle, and
// nothing else caboose needs.
func TestProbeBareAlpine(t *testing.T) {
	f := newFake(t)
	f.musl()
	f.remove("usr/lib/libgcc_s.so.1")
	f.remove("usr/lib/libstdc++.so.6")
	for _, s := range []string{"bash", "curl", "tmux", "git", "tic", "rg"} {
		f.unstub(s)
	}
	r := f.probe(1000, 1000)
	want := []string{"bash", "curl", "tmux", "git", "libgcc", "libstdc++", "ripgrep"}
	if got := problemNames(r); !slices.Equal(got, want) {
		t.Errorf("problems %v, want %v", got, want)
	}
	// No curl: reachability is unknown, not a missing CA bundle.
	if c, _ := r.Check("cacerts"); !c.OK {
		t.Errorf("cacerts %+v", c)
	}
	if len(r.Warnings) != 2 || !strings.Contains(r.Warnings[0], "not checked: no curl") {
		t.Errorf("warnings %q", r.Warnings)
	}
}

// git is run, not just found: caboose sync needs 2.28 or later (git init -b),
// and a git that cannot report its version cannot be relied on either.
func TestProbeGitVersion(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		ok         bool
	}{
		{"echo git version 2.28.0", "/git 2.28.0", true},
		{"echo 'git version 2.39.5 (Apple Git-154)'", "/git 2.39.5", true},
		{"echo git version 3.0.1", "/git 3.0.1", true},
		{"echo git version 2.27.9", "2.27.9 is older than 2.28", false},
		{"echo git version 1.99.0", "1.99.0 is older than 2.28", false},
		{"exit 127", "does not report a version", false},
		{"echo garbage", "does not report a version", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			f := newFake(t)
			f.stub("git", tc.body)
			c, found := f.probe(1000, 1000).Check("git")
			if !found || c.OK != tc.ok || !strings.Contains(c.Detail, tc.want) {
				t.Errorf("git check %+v (found %v), want ok=%v and %q", c, found, tc.ok, tc.want)
			}
		})
	}
}

func TestProbeNeitherLibc(t *testing.T) {
	f := newFake(t)
	f.remove("lib/aarch64-linux-gnu/libc.so.6")
	f.stub("uname", "echo riscv64")
	r := f.probe(1000, 1000)
	if got := problemNames(r); !slices.Equal(got, []string{"glibc or musl", "x86_64 or aarch64"}) {
		t.Errorf("problems %v", got)
	}
	if r.Libc != "" || r.Platform != "" || r.Arch != "riscv64" {
		t.Errorf("libc %q platform %q arch %q", r.Libc, r.Platform, r.Arch)
	}
}

// The other places glibc's libc.so.6 lives: /lib64, /usr/lib multiarch.
func TestProbeGlibcLocations(t *testing.T) {
	for _, p := range []string{"lib64/libc.so.6", "usr/lib/x86_64-linux-gnu/libc.so.6", "usr/lib64/libc.so.6", "lib/libc.so.6"} {
		t.Run(p, func(t *testing.T) {
			f := newFake(t)
			f.remove("lib/aarch64-linux-gnu/libc.so.6")
			f.file(p, "")
			if c, _ := f.probe(1000, 1000).Check("libc"); !c.OK || c.Detail != "glibc /"+p {
				t.Errorf("libc %+v", c)
			}
		})
	}
}

func TestProbeUIDHolders(t *testing.T) {
	f := newFake(t)
	// The last line has no newline, as hand-edited files sometimes do.
	f.file("etc/passwd", "root:x:0:0:root:/root:/bin/sh\nagent:x:1001:1001::/home/agent:/bin/sh\nnode:x:1000:1000::/home/node:/bin/bash")
	f.file("etc/group", "root:x:0:\nnode:x:1000:\nstaff:x:20:\nagent:x:1001:")
	r := f.probe(1000, 20)
	if !r.OK() {
		t.Errorf("a taken uid failed the image: %q", r.Problems())
	}
	if r.UIDOwner != "node" || r.GIDOwner != "staff" || r.AgentUID != "1001" || r.AgentGID != "1001" {
		t.Errorf("uid %q gid %q agent %q %q", r.UIDOwner, r.GIDOwner, r.AgentUID, r.AgentGID)
	}
	// A UID that is only some entry's GID is free.
	r = f.probe(20, 4242)
	if r.UIDOwner != "" || r.GIDOwner != "" {
		t.Errorf("uid %q gid %q", r.UIDOwner, r.GIDOwner)
	}
	// No passwd at all: everything is free.
	f.remove("etc/passwd")
	if r := f.probe(1000, 20); r.UIDOwner != "" || r.AgentUID != "" {
		t.Errorf("uid %q agent %q", r.UIDOwner, r.AgentUID)
	}
}

// The uid holder's home and shell come along, from the fields themselves --
// an empty one included, and on a last line with no newline.
func TestProbeUIDHolderAccount(t *testing.T) {
	f := newFake(t)
	f.file("etc/passwd", "root:x:0:0:root:/root:/bin/sh\n_apt:x:100:65534::/nonexistent:/usr/sbin/nologin\nnode:x:1000:1000::/home/node:")
	r := f.probe(100, 20)
	if r.UIDOwner != "_apt" || r.UIDHome != "/nonexistent" || r.UIDShell != "/usr/sbin/nologin" || !r.SystemAccount() || !r.OK() {
		t.Errorf("%+v, problems %q", r, r.Problems())
	}
	r = f.probe(1000, 20)
	if r.UIDOwner != "node" || r.UIDHome != "/home/node" || r.UIDShell != "" || r.SystemAccount() {
		t.Errorf("%+v", r)
	}
}

// What layer-user.sh edits and creates, checked up front: the account files
// must be regular files, and /home/agent and the mountpoints in it
// directories (or absent), never symlinks.
func TestProbeUserSetup(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fakeImage)
		want  map[string]string // check -> detail, for the failing ones
	}{
		{"complete", func(*fakeImage) {}, nil},
		{"home exists", func(f *fakeImage) { f.file("home/agent/.claude/x", "") }, nil},
		{"no group", func(f *fakeImage) { f.remove("etc/group") }, map[string]string{"etc:group": "no /etc/group"}},
		{"passwd a dir", func(f *fakeImage) {
			f.remove("etc/passwd")
			if err := os.MkdirAll(filepath.Join(f.root, "etc/passwd"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, map[string]string{"etc:passwd": "/etc/passwd is not a regular file"}},
		{"passwd a symlink", func(f *fakeImage) {
			f.file("etc/passwd.real", "root:x:0:0:root:/root:/bin/sh\n")
			f.remove("etc/passwd")
			if err := os.Symlink("passwd.real", filepath.Join(f.root, "etc/passwd")); err != nil {
				t.Fatal(err)
			}
		}, map[string]string{"etc:passwd": "/etc/passwd is a symlink"}},
		{"home a symlink", func(f *fakeImage) {
			f.file("srv/agent/.keep", "")
			if err := os.MkdirAll(filepath.Join(f.root, "home"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../srv/agent", filepath.Join(f.root, "home/agent")); err != nil {
				t.Fatal(err)
			}
		}, map[string]string{"home": "/home/agent is a symlink"}},
		{"a mountpoint a file", func(f *fakeImage) { f.file("home/agent/.local", "") },
			map[string]string{"home": "/home/agent/.local is not a directory"}},
		{"home a file", func(f *fakeImage) { f.file("home", "") }, map[string]string{"home": "/home is not a directory"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			tc.setup(f)
			r := f.probe(1000, 1000)
			for _, name := range []string{"etc:passwd", "etc:group", "home"} {
				c, ok := r.Check(name)
				detail, bad := tc.want[name]
				if !ok || c.OK == bad || (bad && c.Detail != detail) {
					t.Errorf("%s: %+v (reported %v), want failing %v %q", name, c, ok, bad, detail)
				}
			}
		})
	}
}

func TestProbeTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*fakeImage)
		want   string // the tools part of the checklist
	}{
		{"readlink without -f", func(f *fakeImage) {
			f.unstub("readlink")
			f.stub("readlink", `[ "$1" = -f ] && exit 1; exit 0`)
		}, "missing readlink -f (no -f)"},
		{"find without -delete", func(f *fakeImage) {
			f.unstub("find")
			f.stub("find", "exit 1")
		}, "missing find -delete (no -mindepth/-delete)"},
		{"mktemp broken", func(f *fakeImage) {
			f.unstub("mktemp")
			f.stub("mktemp", "exit 1")
		}, "missing mktemp (mktemp -d failed)"},
		{"no env at /usr/bin/env", func(f *fakeImage) { f.remove("usr/bin/env") }, "missing /usr/bin/env (not at /usr/bin/env)"},
		{"no sha256sum or test", func(f *fakeImage) { f.unstub("sha256sum"); f.unstub("test") }, "missing test, sha256sum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			tc.break_(f)
			r := f.probe(1000, 1000)
			var tools string
			for _, row := range r.Checklist() {
				if row.Label == "tools" {
					tools = row.Value
				}
			}
			if tools != tc.want {
				t.Errorf("tools = %q, want %q", tools, tc.want)
			}
			if r.OK() {
				t.Error("passed")
			}
		})
	}
}

// With mktemp missing, find is still found, just not tried.
func TestProbeFindUnverified(t *testing.T) {
	f := newFake(t)
	f.unstub("mktemp")
	c, _ := f.probe(1000, 1000).Check("tool:find")
	if !c.OK || !strings.HasSuffix(c.Detail, "(-delete not verified: no temp dir)") {
		t.Errorf("find %+v", c)
	}
}

// CA certificates and reachability are separate: no network is not "no CA
// certificates", and a trusted https answer vouches for certificates the
// probe could not find a bundle for.
func TestProbeCACertsAndNetwork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bundle  string // "" for none
		curl    string // stub body
		cacerts bool
		detail  string
		warn    string
	}{
		{"bundle, online", "etc/ssl/certs/ca-certificates.crt", "exit 0", true, "/etc/ssl/certs/ca-certificates.crt", ""},
		{"bundle, offline", "etc/ssl/certs/ca-certificates.crt", "exit 6", true, "/etc/ssl/certs/ca-certificates.crt",
			"net https://claude.ai unreachable (curl exit 6)"},
		{"fedora bundle", "etc/pki/tls/certs/ca-bundle.crt", "exit 0", true, "/etc/pki/tls/certs/ca-bundle.crt", ""},
		{"alpine cert.pem", "etc/ssl/cert.pem", "exit 0", true, "/etc/ssl/cert.pem", ""},
		{"no bundle, verified online", "", "exit 0", true, "no bundle at the usual paths, but https://downloads.claude.ai verified", ""},
		{"no bundle, TLS fails", "", "exit 60", false, "no bundle, and https://downloads.claude.ai failed TLS",
			"net https://claude.ai TLS failed (curl exit 60)"},
		{"no bundle, offline", "", "exit 7", false, "no bundle at the usual paths", "net https://claude.ai unreachable (curl exit 7)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.remove("etc/ssl/certs/ca-certificates.crt")
			if tc.bundle != "" {
				f.file(tc.bundle, "cert\n")
			}
			f.stub("curl", tc.curl)
			r := f.probe(1000, 1000)
			if c, _ := r.Check("cacerts"); c.OK != tc.cacerts || c.Detail != tc.detail {
				t.Errorf("cacerts %+v", c)
			}
			if tc.warn == "" && len(r.Warnings) != 0 || tc.warn != "" && (len(r.Warnings) != 2 || r.Warnings[0] != tc.warn) {
				t.Errorf("warnings %q", r.Warnings)
			}
		})
	}
	// An empty bundle file is no bundle.
	f := newFake(t)
	f.file("etc/ssl/certs/ca-certificates.crt", "")
	f.stub("curl", "exit 7")
	if c, _ := f.probe(1000, 1000).Check("cacerts"); c.OK {
		t.Errorf("an empty bundle passed: %+v", c)
	}
}

// The probe on this machine, as it is, with only the network stubbed out:
// well-formed, and right about what is plainly true here.
func TestProbeHere(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe describes a linux image")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", probeScript(t), "caboose-probe", strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()))
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(string(out))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch != "" && !strings.HasPrefix(r.Platform, "linux-"+arch) {
		t.Errorf("platform %q on %s", r.Platform, runtime.GOARCH)
	}
	if r.Libc == "" {
		t.Error("no libc found here")
	}
	if r.UID != os.Getuid() || r.GID != os.Getgid() {
		t.Errorf("uid %d gid %d", r.UID, r.GID)
	}
	if _, err := exec.LookPath("bash"); err == nil {
		if c, _ := r.Check("bash"); !c.OK {
			t.Errorf("bash %+v", c)
		}
	}
	if c, _ := r.Check("curl"); c.Detail != filepath.Join(bin, "curl") {
		t.Errorf("curl %+v", c)
	}
	t.Logf("\n%s", out)
}
