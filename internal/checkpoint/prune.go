package checkpoint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/bfreis/caboose/internal/saferm"
)

// testHookAfterTrash, when set (by tests only), runs between a dropped
// checkpoint's rename to .trash- and its removal.
var testHookAfterTrash func()

// Prune applies r to key's checkpoints and removes those Retain drops,
// never one in pinned (a pinned KEY@latest is refused: callers pin the
// checkpoint they mean) or in the store's pins (Pins): a pins file that
// cannot be read drops nothing. Pins whose checkpoint is no longer
// listed are forgotten. It needs the store's lock, held, and refuses while
// the store is exposed. Only complete, readable checkpoints are judged: one
// whose meta.json cannot be used is left alone. Each dropped checkpoint is
// first renamed to .trash-<key>-<stamp>-<rand> in the store, so it is
// never listed again, then removed; a removal that fails leaves the .trash-
// directory for CleanLeftovers, is logged, and the others go on. It returns
// the IDs dropped, removed or not.
func (s *Store) Prune(l *Lock, key string, r Rules, pinned []ID) ([]ID, error) {
	if err := l.check(s); err != nil {
		return nil, err
	}
	if err := s.exposed(); err != nil {
		return nil, err
	}
	if !ValidKey(key) {
		return nil, fmt.Errorf("checkpoint: bad key %q", key)
	}
	var pins []time.Time
	for _, p := range pinned {
		if p.Latest {
			return nil, fmt.Errorf("checkpoint: cannot pin %s: pin a checkpoint, not latest", p)
		}
		if p.Key == key {
			pins = append(pins, p.Stamp)
		}
	}
	stored, err := s.PinnedIDs()
	if err != nil {
		s.Logf("prune of %s skipped: %v", key, err)
		return nil, err
	}
	for _, p := range stored {
		if p.Key == key {
			pins = append(pins, p.Stamp)
		}
	}
	es, err := s.List(key)
	if err != nil {
		return nil, err
	}
	if err := s.forgetPins(l, key, es); err != nil {
		s.Logf("prune of %s: pins: %v", key, err)
	}
	var stamps []time.Time
	for _, e := range es {
		if e.Err == nil {
			stamps = append(stamps, e.Stamp)
		}
	}
	_, drop := Retain(stamps, r, pins)
	var dropped []ID
	var errs []error
	for _, st := range drop {
		id := ID{Key: key, Stamp: st}
		dropped = append(dropped, id)
		if err := s.remove(id); err != nil {
			s.Logf("prune %s: %v", id, err)
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		s.Logf("pruned %s", id)
	}
	return dropped, errors.Join(errs...)
}

// remove renames the checkpoint id to a fresh .trash- name, then removes it.
func (s *Store) remove(id ID) error {
	trash := filepath.Join(s.dir, trashPrefix+id.Key+"-"+FormatStamp(id.Stamp)+"-"+randHex())
	if _, err := os.Lstat(trash); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checkpoint: %s is in the way", filepath.Base(trash))
	}
	if err := os.Rename(s.checkpointDir(id), trash); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if testHookAfterTrash != nil {
		testHookAfterTrash()
	}
	return saferm.RemoveTree(s.dir, trash)
}
