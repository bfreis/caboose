//go:build unix

package conformance

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/saferm"
)

// testHookDiscard, when set (by tests only), runs in DiscardRestore after
// the canary was opened and checked and before anything is removed, so a
// test can swap the canary's path for a link.
var testHookDiscard func()

// DiscardRestore removes what a restore wrote at t, which must be a name
// inside a directory canary this guard made (TargetIn; a canary itself is
// never a target to discard). The canary may sit where a sandbox writes,
// so nothing here goes by path: B is opened, then each component of the
// canary's path below it with O_NOFOLLOW (a link anywhere on the way is
// refused), the canary's descriptor is checked against the device and
// inode the guard recorded, and saferm removes t's name relative to that
// held descriptor, its last component unlinked, never followed. Whatever
// the sandbox renames or links afterwards, the removal stays inside the
// directory the guard made. Nothing at t is not an error.
func (g *Guard) DiscardRestore(t RestoreTarget) error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	if !t.valid() || t.name == "" {
		return errors.New("conformance: only a name a restore was given inside a directory canary is discarded")
	}
	g.mu.Lock()
	rec, ok := g.canaries[t.canary]
	g.mu.Unlock()
	switch {
	case !ok || rec.canary.Rel != t.canary:
		return fmt.Errorf("conformance: %s is not a canary of this run", t.canary)
	case rec.canary.Kind != Dir:
		return fmt.Errorf("conformance: %s is not a directory canary", t.canary)
	}
	dir, err := os.OpenFile(g.base, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("conformance: the base: %w", err)
	}
	defer func() { dir.Close() }()
	for _, c := range strings.Split(t.canary, "/") {
		fd, err := unix.Openat(int(dir.Fd()), c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("conformance: canary %s: %s: %w", t.canary, c, err)
		}
		next := os.NewFile(uintptr(fd), dir.Name()+"/"+c)
		dir.Close()
		dir = next
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &st); err != nil {
		return fmt.Errorf("conformance: canary %s: %w", t.canary, err)
	}
	if uint64(st.Dev) != rec.id.Dev || uint64(st.Ino) != rec.id.Ino {
		return fmt.Errorf("conformance: canary %s is no longer the directory the suite made", t.canary)
	}
	if testHookDiscard != nil {
		testHookDiscard()
	}
	return saferm.RemoveTreeAt(dir, t.name)
}
