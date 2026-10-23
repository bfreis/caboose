package conformance

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/config"
)

// The sandbox-side gate runs before every destructive command, inside the
// sandbox, as an inline /bin/sh -c of literals only. Its command text is
// built elsewhere; what it compares is decided here, from the host, so
// that the gate and its tests agree: GateSpec is the expected state, and
// CompareMountinfo is the comparison of /proc/self/mountinfo it makes.

// MountEntry is one line of /proc/self/mountinfo.
type MountEntry struct {
	ID, Parent int
	Root       string
	Target     string // the mount point
	Options    []string
	FSType     string
	Source     string
	// SuperOptions is the per-superblock options field, the last field of
	// the line: it says ro/rw of the filesystem itself, which a bind
	// mount's own ro flag (Options) can differ from.
	SuperOptions []string
}

// ReadOnly reports whether the mount's own options say ro.
func (m MountEntry) ReadOnly() bool { return slices.Contains(m.Options, "ro") }

// ParseMountinfo parses /proc/self/mountinfo. A line it cannot read is an
// error: the gate never guesses.
func ParseMountinfo(r io.Reader) ([]MountEntry, error) {
	var out []MountEntry
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for s.Scan() {
		n++
		line := s.Text()
		if line == "" {
			continue
		}
		f := strings.Split(line, " ")
		sep := slices.Index(f, "-")
		if sep < 6 || len(f) < sep+4 {
			return nil, fmt.Errorf("mountinfo line %d: malformed", n)
		}
		id, err1 := strconv.Atoi(f[0])
		parent, err2 := strconv.Atoi(f[1])
		root, err3 := unescapeMount(f[3])
		target, err4 := unescapeMount(f[4])
		source, err5 := unescapeMount(f[sep+2])
		if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
			return nil, fmt.Errorf("mountinfo line %d: %v", n, err)
		}
		var super []string
		if len(f) > sep+3 {
			super = strings.Split(f[sep+3], ",")
		}
		out = append(out, MountEntry{
			ID: id, Parent: parent, Root: root, Target: target,
			Options:      strings.Split(f[5], ","),
			FSType:       f[sep+1],
			Source:       source,
			SuperOptions: super,
		})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("mountinfo is empty")
	}
	return out, nil
}

// unescapeMount undoes the kernel's octal escapes (\040 for a space, ...).
func unescapeMount(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", fmt.Errorf("bad escape in %q", s)
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("bad escape in %q", s)
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String(), nil
}

// DefaultPseudo are the filesystem types any mount outside the watched
// targets may have. An isolation's fixture adds what it shows of its own.
var DefaultPseudo = []string{"proc", "sysfs", "tmpfs", "devtmpfs", "devpts", "mqueue", "cgroup2", "cgroup", "overlay"}

// watchedPrefixes are the targets whose every mount must be one the audit
// let through, with its read-only flag.
var watchedPrefixes = []string{"/work", "/home/agent", "/etc/claude-code"}

func watched(target string) bool {
	for _, p := range watchedPrefixes {
		if target == p || strings.HasPrefix(target, p+"/") {
			return true
		}
	}
	return false
}

// FixedMount is a mount an isolation always shows outside the watched
// targets that is not a pseudo filesystem (an engine's /etc/hosts, a VM's
// root disk): allowed only at that target with that type.
type FixedMount struct {
	Target, FSType string
}

// GateFixture is what one isolation shows of its own, captured from a
// real sandbox of it. The comparison is never loosened to make one pass.
type GateFixture struct {
	Isolation string
	Pseudo    []string // added to DefaultPseudo
	Fixed     []FixedMount
}

// GateSpec is what the sandbox-side gate requires, all of it literals:
// the roots /work must list exactly, the nonce each root's marker holds,
// the audited mounts, and under vm the pinned boot_id.
type GateSpec struct {
	Isolation string
	Nonce     string
	Roots     []string
	Mounts    []AuditedMount
	BootID    string
	Pseudo    []string
	Fixed     []FixedMount
}

// GateSpec builds the gate's expected values from the pins and the audit
// they were recorded with. It refuses before the sandbox is pinned, and
// without a fixture for the run's isolation.
func (g *Guard) GateSpec(fx *GateFixture) (GateSpec, error) {
	p, a, ok := g.Pinned()
	if !ok {
		return GateSpec{}, refuse("gate", "the sandbox was never pinned")
	}
	if fx == nil || fx.Isolation != g.isolation {
		return GateSpec{}, refuse("gate", "no mountinfo fixture for %s", g.isolation)
	}
	spec := GateSpec{
		Isolation: g.isolation,
		Nonce:     g.nonce,
		Roots:     g.RootNames(),
		Mounts:    slices.Clone(a.Mounts),
		Pseudo:    append(slices.Clone(DefaultPseudo), fx.Pseudo...),
		Fixed:     slices.Clone(fx.Fixed),
	}
	if g.isolation == config.KindVM {
		spec.BootID = p.BootID
	}
	return spec, nil
}

// The bounds of a refusal that names unshown mounts: how many it names,
// and how many bytes of each field.
const (
	maxUnshown    = 20
	maxFieldBytes = 128
)

// cutField cuts a mountinfo field to maxFieldBytes.
func cutField(s string) string {
	if len(s) <= maxFieldBytes {
		return s
	}
	return s[:maxFieldBytes] + "..."
}

// CompareMountinfo is the gate's mount check: every mount at or below a
// watched target is one of spec's audited mounts, once, with its
// read-only flag, and every audited mount is there; every other mount is
// an audited one outside them (an engine volume), a fixed one of the
// isolation's, or of a pseudo filesystem type.
func CompareMountinfo(entries []MountEntry, spec GateSpec) error {
	want := map[string]bool{}
	other := map[AuditedMount]bool{}
	for _, m := range spec.Mounts {
		if watched(m.Target) {
			want[m.Target] = m.ReadOnly
		} else {
			other[m] = true
		}
	}
	seen := map[string]bool{}
	// unshown are every mount outside the watched targets the isolation
	// does not show, all of them named: a refusal that names one at a
	// time takes a run per mount to learn an isolation's set.
	var unshown []string
	for _, e := range entries {
		if watched(e.Target) {
			ro, ok := want[e.Target]
			switch {
			case !ok:
				return fmt.Errorf("%s is mounted and was not audited", e.Target)
			case seen[e.Target]:
				return fmt.Errorf("%s is mounted twice", e.Target)
			case ro != e.ReadOnly():
				return fmt.Errorf("%s: read-only is %t, audited %t", e.Target, e.ReadOnly(), ro)
			}
			seen[e.Target] = true
			continue
		}
		if other[AuditedMount{e.Target, e.ReadOnly()}] || slices.Contains(spec.Fixed, FixedMount{e.Target, e.FSType}) || slices.Contains(spec.Pseudo, e.FSType) {
			continue
		}
		unshown = append(unshown, fmt.Sprintf("%q is a %q mount (source %q)", cutField(e.Target), cutField(e.FSType), cutField(e.Source)))
	}
	if len(unshown) > 0 {
		// The sandbox writes its mount table: the text is bounded.
		more := ""
		if len(unshown) > maxUnshown {
			more = fmt.Sprintf("; and %d more", len(unshown)-maxUnshown)
			unshown = unshown[:maxUnshown]
		}
		return fmt.Errorf("%s%s, which the isolation does not show", strings.Join(unshown, "; "), more)
	}
	for t := range want {
		if !seen[t] {
			return fmt.Errorf("%s was audited and is not mounted", t)
		}
	}
	return nil
}
