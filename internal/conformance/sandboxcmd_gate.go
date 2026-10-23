package conformance

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/config"
)

// The sandbox-side gate's command text. It is built from a GateSpec alone
// (the nonce, the root names, the audited mounts, the pinned boot_id and
// the isolation's fixture), each a literal checked against a narrow
// pattern, never from anything the sandbox said. It runs inline, as the
// script of /bin/sh -c, so nothing on a path the sandbox can write takes
// part in it, with PATH=/usr/bin:/bin and only shell builtins. Its own
// variables and the "$@" it hands the command to are fixed text of this
// file: no value of the run is ever expanded by the shell.

// GateRefused is what the gate says on stderr, its first line, when it
// refuses; it then exits GateExit and runs nothing.
const GateRefused = "conformance: the sandbox-side gate refused"

// gateArg0 is $0 of the gated shell.
const gateArg0 = "conformance-gate"

// gatePaths are where the gate looks: fixed in GateScript, a test's
// directories in the tests.
type gatePaths struct {
	work, mountinfo, bootID string
}

var realGatePaths = gatePaths{work: "/work", mountinfo: "/proc/self/mountinfo", bootID: "/proc/sys/kernel/random/boot_id"}

var (
	// gateLiteral is every path or word the gate names: nothing a shell
	// or the kernel's octal escapes would change, nothing a case pattern
	// would read as a pattern.
	gateLiteral = regexp.MustCompile(`^/[A-Za-z0-9._@+/-]*$`)
	gateWord    = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)
	nonceRe     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	gateRootRe  = regexp.MustCompile(`^conf-[0-9a-f]{8}-(ro|dev)$`)
)

// GateScript is the gate for spec, as the script of /bin/sh -c: it checks
// that /work lists exactly the suite's two roots, each holding the nonce;
// that /proc/self/mountinfo shows the audited mounts and nothing else
// under /work, /home/agent and /etc/claude-code, with their read-only
// flags, and elsewhere only what the isolation's fixture allows; and under
// vm that the guest's boot_id is the pinned one. Then it executes its
// arguments; otherwise it says GateRefused and exits GateExit.
func GateScript(spec GateSpec) (string, error) { return gateScript(spec, realGatePaths) }

func gateScript(spec GateSpec, p gatePaths) (string, error) {
	if err := checkGateSpec(spec, p); err != nil {
		return "", err
	}
	q := func(s string) string { return "'" + s + "'" }
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("exec 2>/dev/null; PATH=/usr/bin:/bin; export PATH; LC_ALL=C; export LC_ALL")

	// (a) /work lists exactly the two roots, real directories, each with a
	// regular marker holding the nonce and nothing more.
	work := p.work
	w("for e in %s/* %s/.*; do", work, work)
	w("  case $e in")
	pats := []string{q(work + "/."), q(work + "/..")}
	for _, r := range spec.Roots {
		pats = append(pats, q(work+"/"+r))
	}
	w("  %s) ;;", strings.Join(pats, "|"))
	w("  %s) if test -e \"$e\" || test -L \"$e\"; then exit 3; fi ;;", q(work+"/*")+"|"+q(work+"/.*"))
	w("  *) exit 3 ;;")
	w("  esac")
	w("done")
	for _, r := range spec.Roots {
		d := q(work + "/" + r)
		m := q(work + "/" + r + "/" + RootMarker)
		w("test -d %s && test ! -L %s || exit 3", d, d)
		w("test -f %s && test ! -L %s || exit 3", m, m)
		w("{ IFS= read -r l && test \"$l\" = %s && ! { IFS= read -r l || test -n \"$l\"; }; } < %s || exit 3", q(spec.Nonce), m)
	}

	// (b) the mounts.
	var watchedM, otherM []AuditedMount
	for _, m := range spec.Mounts {
		if watched(m.Target) {
			watchedM = append(watchedM, m)
		} else {
			otherM = append(otherM, m)
		}
	}
	flag := func(ro bool) string {
		if ro {
			return "ro"
		}
		return "rw"
	}
	for i := range watchedM {
		w("s%d=", i)
	}
	w("while read -r a b c d t o r; do")
	w("  f=${r#*- }; f=${f%%%% *}")
	w("  case ,$o, in *,ro,*) m=\"$t ro\" ;; *) m=\"$t rw\" ;; esac")
	w("  case $t in")
	var wp []string
	for _, x := range watchedPrefixes {
		wp = append(wp, q(x), q(x)+"/*")
	}
	w("  %s)", strings.Join(wp, "|"))
	w("    case $m in")
	for i, m := range watchedM {
		w("    %s) test -z \"$s%d\" || exit 3; s%d=1 ;;", q(m.Target+" "+flag(m.ReadOnly)), i, i)
	}
	w("    *) exit 3 ;;")
	w("    esac ;;")
	w("  *)")
	var allowed []string
	for _, m := range otherM {
		allowed = append(allowed, q(m.Target+" "+flag(m.ReadOnly)))
	}
	var fixed []string
	for _, fm := range spec.Fixed {
		fixed = append(fixed, q(fm.Target+" "+fm.FSType))
	}
	w("    case \"$t $f\" in")
	if len(fixed) > 0 {
		w("    %s) ;;", strings.Join(fixed, "|"))
	}
	w("    *)")
	w("      case $f in")
	w("      %s) ;;", strings.Join(spec.Pseudo, "|"))
	w("      *)")
	w("        case $m in")
	if len(allowed) > 0 {
		w("        %s) ;;", strings.Join(allowed, "|"))
	}
	w("        *) exit 3 ;;")
	w("        esac ;;")
	w("      esac ;;")
	w("    esac ;;")
	w("  esac")
	w("done < %s", p.mountinfo)
	for i := range watchedM {
		w("test -n \"$s%d\" || exit 3", i)
	}

	// (c) under vm, the guest booted when it was pinned.
	if spec.Isolation == config.KindVM {
		w("{ IFS= read -r l && test \"$l\" = %s; } < %s || exit 3", q(spec.BootID), p.bootID)
	}
	w("exit 0")

	return "if ( " + strings.TrimSuffix(b.String(), "\n") + "\n); then exec \"$@\"; fi\n" +
		"echo " + q(GateRefused) + " >&2\nexit " + strconv.Itoa(GateExit) + "\n", nil
}

// checkGateSpec refuses a spec any part of which is not a literal the gate
// may name.
func checkGateSpec(s GateSpec, p gatePaths) error {
	bad := func(what string, v any) error { return fmt.Errorf("conformance: the gate cannot name %s %q", what, v) }
	for _, x := range []string{p.work, p.mountinfo, p.bootID} {
		if !gateLiteral.MatchString(x) {
			return bad("path", x)
		}
	}
	if !knownIsolation(s.Isolation) {
		return bad("isolation", s.Isolation)
	}
	if !nonceRe.MatchString(s.Nonce) {
		return bad("nonce", s.Nonce)
	}
	if len(s.Roots) != 2 || !gateRootRe.MatchString(s.Roots[0]) || !gateRootRe.MatchString(s.Roots[1]) || s.Roots[0] == s.Roots[1] {
		return bad("roots", s.Roots)
	}
	for _, r := range s.Roots {
		if !strings.HasPrefix(r, "conf-"+s.Nonce[:8]+"-") {
			return bad("root", r)
		}
	}
	if s.Isolation == config.KindVM {
		if !bootIDRe.MatchString(s.BootID) {
			return bad("boot_id", s.BootID)
		}
	} else if s.BootID != "" {
		return bad("boot_id", s.BootID)
	}
	if len(s.Mounts) == 0 {
		return errors.New("conformance: the gate needs the audited mounts")
	}
	seen := map[string]bool{}
	for _, m := range s.Mounts {
		if !gateLiteral.MatchString(m.Target) || m.Target == "/" || seen[m.Target] {
			return bad("mount", m.Target)
		}
		seen[m.Target] = true
	}
	if len(s.Pseudo) == 0 {
		return errors.New("conformance: the gate needs the pseudo filesystem types")
	}
	for _, t := range s.Pseudo {
		if !gateWord.MatchString(t) {
			return bad("filesystem type", t)
		}
	}
	for _, f := range s.Fixed {
		if !gateLiteral.MatchString(f.Target) || !gateWord.MatchString(f.FSType) {
			return bad("fixed mount", f)
		}
	}
	return nil
}

// Fixtures are what each isolation shows of its own outside the watched
// targets: the engine's bind mounts of /etc/hosts and its neighbours (and
// of its init under docker run --init), a VM's root and docker disks.
// They are the expected state, to be confirmed against a real sandbox of
// each isolation; a mount they do not name makes the gate refuse, which
// is an ERROR, never a pass.
var Fixtures = []GateFixture{
	{Isolation: config.KindContainer, Fixed: engineFiles("ext4", "btrfs", "xfs")},
	// gVisor's sentry serves /dev from a filesystem of its own, of type
	// "dev" (runsc's devtmpfs stand-in), where runc mounts a tmpfs: it is
	// allowed at /dev and nowhere else. Its gofer's shares, of type "9p"
	// or "gofer", are allowed only at runsc's root (when it is not an
	// overlay, which every isolation allows) and at the engine's files:
	// one anywhere else is a share no audit saw.
	{Isolation: config.KindGVisor, Fixed: append(engineFiles("9p", "gofer"), FixedMount{"/", "9p"}, FixedMount{"/", "gofer"}, FixedMount{"/dev", "dev"})},
	// A vm guest's init mounts securityfs where the kernel says its
	// lockdown mode, at /sys/kernel/security and nowhere else. Its agent
	// detaches the staging mount of the virtio-fs share once the binds are
	// made, so no virtiofs mount is let through outside the watched ones.
	{Isolation: config.KindVM, Fixed: []FixedMount{{"/", "ext4"}, {"/var/lib/docker", "ext4"}, {"/sys/kernel/security", "securityfs"}}},
}

// engineFiles are the engine's files, each allowed with any of types.
// docker run --init mounts its init at /sbin/docker-init, which on a base
// with a merged /usr (Wolfi's, so every apko base) resolves through the
// /sbin link and is listed at /usr/bin or /usr/sbin: those two places of
// that one file, and no others.
func engineFiles(types ...string) []FixedMount {
	var out []FixedMount
	for _, t := range []string{"/etc/resolv.conf", "/etc/hostname", "/etc/hosts", "/sbin/docker-init", "/usr/sbin/docker-init", "/usr/bin/docker-init"} {
		for _, ty := range types {
			out = append(out, FixedMount{t, ty})
		}
	}
	return out
}

// FixtureFor is the fixture of isolation, or nil.
func FixtureFor(isolation string) *GateFixture {
	i := slices.IndexFunc(Fixtures, func(f GateFixture) bool { return f.Isolation == isolation })
	if i < 0 {
		return nil
	}
	f := Fixtures[i]
	return &f
}
