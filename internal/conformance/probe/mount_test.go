package probe

import (
	"bytes"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// stringLiterals are the unquoted string literals in src.
func stringLiterals(t *testing.T, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file := fset.AddFile("f.go", -1, len(src))
	var sc scanner.Scanner
	sc.Init(file, src, nil, 0)
	var out []string
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.STRING {
			if v, err := strconv.Unquote(lit); err == nil {
				out = append(out, v)
			}
		}
	}
	return out
}

func mainMount(work string, args ...string) (int, string) {
	var out, errb bytes.Buffer
	rc := Main(work, args, &out, &errb)
	return rc, out.String() + errb.String()
}

func TestMountUsage(t *testing.T) {
	w := work(t)
	ro := filepath.Join(w, "conf-01234567-ro")
	for name, args := range map[string][]string{
		"no target":        {"--nonce", nonce, "--mount", Umount},
		"no op":            {"--nonce", nonce, "--target", ro},
		"no nonce":         {"--mount", Umount, "--target", ro},
		"with a link":      {"--nonce", nonce, "--mount", Umount, "--target", ro, "--link", "/tmp/x.sock"},
		"with only":        {"--nonce", nonce, "--mount", Umount, "--target", ro, "--only", "status"},
		"unknown op":       {"--nonce", nonce, "--mount", "bind", "--target", ro},
		"work itself":      {"--nonce", nonce, "--mount", Umount, "--target", w},
		"below a root":     {"--nonce", nonce, "--mount", Umount, "--target", filepath.Join(ro, "x")},
		"another dir":      {"--nonce", nonce, "--mount", Umount, "--target", "/tmp"},
		"dotdot":           {"--nonce", nonce, "--mount", Umount, "--target", filepath.Join(w, "conf-01234567-dev", "part") + "/.."},
		"the system's own": {"--nonce", nonce, "--mount", MountBindRW, "--target", "/"},
	} {
		if rc, out := mainMount(w, args...); rc != ExitUsage {
			t.Errorf("%s: exit %d, want %d (%s)", name, rc, ExitUsage, out)
		}
	}
}

func TestMountGate(t *testing.T) {
	w := work(t)
	other := strings.Repeat("f", 32)
	rc, out := mainMount(w, "--nonce", other, "--mount", Umount, "--target", filepath.Join(w, "conf-01234567-ro"))
	if rc != ExitGate || !strings.Contains(out, "the gate refused") {
		t.Errorf("exit %d: %s", rc, out)
	}
	if err := os.WriteFile(filepath.Join(w, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rc, out := mainMount(w, "--nonce", nonce, "--mount", Umount, "--target", filepath.Join(w, "conf-01234567-ro")); rc != ExitGate {
		t.Errorf("a /work with a stray entry: exit %d: %s", rc, out)
	}
}

// Every call, on every mount point, as a user without CAP_SYS_ADMIN, is
// refused with EPERM, said in strerror's words. The temp dirs are no
// mount points: as root a call is refused all the same (EINVAL), and
// changes nothing.
func TestMountCallsAreRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the mount calls are Linux's")
	}
	w := work(t)
	for _, mp := range mountPoints(w, nonce) {
		for _, op := range MountOps() {
			rc, out := mainMount(w, "--nonce", nonce, "--mount", op, "--target", mp)
			if rc != ExitRefused {
				t.Errorf("%s %s: exit %d: %s", op, mp, rc, out)
				continue
			}
			if !strings.HasPrefix(out, "conformance-probe: "+op+" "+mp+": ") {
				t.Errorf("%s %s: %q", op, mp, out)
			}
			if os.Geteuid() != 0 && !strings.Contains(out, "Operation not permitted") {
				t.Errorf("%s %s as uid %d: %q", op, mp, os.Geteuid(), out)
			}
		}
	}
}

func TestErrnoText(t *testing.T) {
	for err, want := range map[error]string{
		syscall.EPERM:      "Operation not permitted",
		syscall.EBUSY:      "Device or resource busy",
		syscall.EINVAL:     "Invalid argument",
		syscall.Errno(200): "errno 200",
	} {
		if got := ErrnoText(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

// The mount calls are made in one file, and nowhere else.
func TestMountCallsInOneFile(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "mount_linux.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"syscall.Mount", "syscall.Unmount", "unix.Mount", "unix.Unmount", "SYS_MOUNT", "SYS_UMOUNT"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s names %s", f, bad)
			}
		}
	}
}

// mount_linux.go is the one probe file that writes. Every absolute-path
// string literal in it is one of the three /proc self maps; the witness,
// target and part it writes to or acts on come as arguments, validated by
// Mount and UserNS before the namespace is entered.
func TestMountLinuxHardcodedPaths(t *testing.T) {
	b, err := os.ReadFile("mount_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"/proc/self/exe": true}
	for _, lit := range stringLiterals(t, b) {
		if strings.HasPrefix(lit, "/") && !allowed[lit] {
			t.Errorf("mount_linux.go names the absolute path %q", lit)
		}
	}
}

func TestUserNSUsage(t *testing.T) {
	w := work(t)
	ro := filepath.Join(w, "conf-01234567-ro")
	part := filepath.Join(w, "conf-01234567-dev", "part")
	wit := filepath.Join(ro, "canary")
	for name, args := range map[string][]string{
		"no part":        {"--nonce", nonce, "--mount", MountUserNS, "--target", ro, "--witness", wit},
		"no witness":     {"--nonce", nonce, "--mount", MountUserNS, "--target", ro, "--part", part},
		"wrong target":   {"--nonce", nonce, "--mount", MountUserNS, "--target", part, "--part", part, "--witness", wit},
		"wrong part":     {"--nonce", nonce, "--mount", MountUserNS, "--target", ro, "--part", ro, "--witness", wit},
		"witness beside": {"--nonce", nonce, "--mount", MountUserNS, "--target", ro, "--part", part, "--witness", filepath.Join(part, "x")},
		"with a link":    {"--nonce", nonce, "--mount", MountUserNS, "--target", ro, "--part", part, "--witness", wit, "--link", "/tmp/x.sock"},
	} {
		if rc, out := mainMount(w, args...); rc != ExitUsage {
			t.Errorf("%s: exit %d, want %d (%s)", name, rc, ExitUsage, out)
		}
	}
}

func TestUserNSGate(t *testing.T) {
	w := work(t)
	ro := filepath.Join(w, "conf-01234567-ro")
	part := filepath.Join(w, "conf-01234567-dev", "part")
	wit := filepath.Join(ro, "canary")
	rc, out := mainMount(w, "--nonce", strings.Repeat("f", 32), "--mount", MountUserNS, "--target", ro, "--part", part, "--witness", wit)
	if rc != ExitGate || !strings.Contains(out, "the gate refused") {
		t.Errorf("exit %d: %s", rc, out)
	}
}

// Whatever this host's kernel does with an unprivileged user namespace,
// the probe's exit and the witness agree: exit 0 (something succeeded)
// iff the witness file was written. On plain directories the remount and
// unmount can only fail, so the witness write alone decides.
func TestUserNSRunInvariant(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("user namespaces are Linux's")
	}
	w := work(t)
	ro := filepath.Join(w, "conf-01234567-ro")
	part := filepath.Join(w, "conf-01234567-dev", "part")
	wit := filepath.Join(ro, "canary")
	if err := os.WriteFile(wit, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, out := mainMount(w, "--nonce", nonce, "--mount", MountUserNS, "--target", ro, "--part", part, "--witness", wit)
	if rc != 0 && rc != ExitRefused {
		t.Fatalf("exit %d: %s", rc, out)
	}
	b, err := os.ReadFile(wit)
	if err != nil {
		t.Fatal(err)
	}
	wrote := string(b) != "a"
	if (rc == 0) != wrote {
		t.Errorf("exit %d but witness written=%v: %s", rc, wrote, out)
	}
	if !strings.Contains(out, "namespace: ") {
		t.Errorf("no namespace line: %s", out)
	}
}

// mount_linux.go may start a program only to run the probe itself again,
// in its child mode: ForkExec appears once, with selfExe and
// userNSChildArg first.
func TestMountLinuxExecsOnlyItself(t *testing.T) {
	b, err := os.ReadFile("mount_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, "ForkExec("); n != 1 {
		t.Fatalf("ForkExec appears %d times", n)
	}
	call := src[strings.Index(src, "ForkExec("):]
	if !strings.HasPrefix(call, "ForkExec(selfExe, []string{selfExe, userNSChildArg, work, ") {
		t.Errorf("ForkExec runs something other than the probe's child mode: %.80s", call)
	}
	if !strings.Contains(src, `const selfExe = "/proc/self/exe"`) {
		t.Error("selfExe is not /proc/self/exe")
	}
}

// Only a policy refusal of the namespace is held; EINVAL (what a
// multi-threaded unshare gets) and any other error prove nothing.
func TestNSRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("user namespaces are Linux's")
	}
	for e, want := range map[error]bool{
		syscall.EPERM: true, syscall.EACCES: true, syscall.ENOSPC: true, syscall.EUSERS: true,
		syscall.EINVAL: false, syscall.ENOSYS: false, syscall.ENOMEM: false, syscall.EFAULT: false,
	} {
		if got := nsRefused(e); got != want {
			t.Errorf("%v: held %v, want %v", e, got, want)
		}
	}
}
