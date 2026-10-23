package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bfreis/caboose/internal/saferm"
)

// pinsName is the store's pins file: the checkpoints kept outside
// retention because a later one of their path shrank sharply.
const pinsName = "pins.json"

// The pins' bounds: per key, and in all. A key that would hold more lets
// go of its oldest pin, which is logged.
const (
	maxPinsPerKey = 8
	maxPins       = 256
)

// Pin is a checkpoint kept outside retention: its path's next checkpoint,
// By, held half or less of what it did. It stays pinned until it is
// released (ReleasePin) or a later checkpoint of its path holds at least
// its Entries and its Bytes again.
type Pin struct {
	ID      string    `json:"id"`
	Entries int64     `json:"entries"`
	Bytes   int64     `json:"bytes"`
	By      string    `json:"by"`    // the checkpoint that shrank
	Since   time.Time `json:"since"` // when it was pinned
}

// pinsDoc is pins.json.
type pinsDoc struct {
	Version int   `json:"version"`
	Pins    []Pin `json:"pins"`
}

// Pins reads the store's pins, oldest first: none when there is no pins
// file. One that cannot be used is an error, and a Prune then drops
// nothing.
func (s *Store) Pins() ([]Pin, error) {
	f, err := openRegular(filepath.Join(s.dir, pinsName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %s: %w", pinsName, err)
	}
	defer f.Close()
	b, err := readBounded(f)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %s: %w", pinsName, err)
	}
	if err := checkVersion(b); err != nil {
		return nil, fmt.Errorf("%s: %w", pinsName, err)
	}
	var d pinsDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("checkpoint: %s: %w", pinsName, err)
	}
	if len(d.Pins) > maxPins {
		return nil, fmt.Errorf("checkpoint: %s holds over %d pins", pinsName, maxPins)
	}
	for _, p := range d.Pins {
		if id, err := ParseID(p.ID); err != nil || id.Latest {
			return nil, fmt.Errorf("checkpoint: %s: bad pin %q", pinsName, p.ID)
		}
	}
	return d.Pins, nil
}

// PinnedIDs are the store's pinned checkpoints, as IDs.
func (s *Store) PinnedIDs() ([]ID, error) {
	ps, err := s.Pins()
	if err != nil {
		return nil, err
	}
	out := make([]ID, 0, len(ps))
	for _, p := range ps {
		id, _ := ParseID(p.ID) // checked by Pins
		out = append(out, id)
	}
	return out, nil
}

// testHookWritePins, when set (by tests only), stands in for a write of
// pins.json that fails with what it returns.
var testHookWritePins func() error

// writePins replaces pins.json with ps, whole: written under another name,
// synced, and renamed over it. No pins removes the file.
func (s *Store) writePins(ps []Pin) error {
	if testHookWritePins != nil {
		if err := testHookWritePins(); err != nil {
			return err
		}
	}
	p := filepath.Join(s.dir, pinsName)
	if len(ps) == 0 {
		if err := saferm.Remove(s.dir, p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("checkpoint: %w", err)
		}
		return syncDir(s.dir)
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Since.Before(ps[j].Since) })
	b, err := json.MarshalIndent(pinsDoc{Version: MetaVersion, Pins: ps}, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if len(b) > maxMetaSize {
		return fmt.Errorf("checkpoint: %s would be over %d KiB", pinsName, maxMetaSize>>10)
	}
	tmp := p + ".tmp-" + randHex()
	if err := writeExclusive(tmp, append(b, '\n')); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = saferm.Remove(s.dir, tmp)
		return fmt.Errorf("checkpoint: %w", err)
	}
	return syncDir(s.dir)
}

// updatePins reads the pins, lets f change them, and writes them back if
// f says they changed. Under the store's lock.
func (s *Store) updatePins(l *Lock, f func([]Pin) ([]Pin, bool)) error {
	if err := l.check(s); err != nil {
		return err
	}
	ps, err := s.Pins()
	if err != nil {
		return err
	}
	out, changed := f(ps)
	if !changed {
		return nil
	}
	return s.writePins(out)
}

// addPin pins p (a checkpoint of key), keeping at most maxPinsPerKey of
// the key and maxPins in all: the oldest of the key go first, then the
// oldest of all.
func (s *Store) addPin(l *Lock, key string, p Pin) error {
	return s.updatePins(l, func(ps []Pin) ([]Pin, bool) {
		for _, q := range ps {
			if q.ID == p.ID {
				return ps, false
			}
		}
		ps = append(ps, p)
		var mine []int
		for i, q := range ps {
			if id, err := ParseID(q.ID); err == nil && id.Key == key {
				mine = append(mine, i)
			}
		}
		drop := map[int]bool{}
		for i := 0; i < len(mine)-maxPinsPerKey; i++ {
			drop[mine[i]] = true
		}
		for i := 0; len(ps)-len(drop) > maxPins && i < len(ps); i++ {
			drop[i] = true
		}
		out := ps[:0:0]
		for i, q := range ps {
			if drop[i] {
				s.Logf("unpinned %s: too many pins", q.ID)
				continue
			}
			out = append(out, q)
		}
		return out, true
	})
}

// ReleasePin unpins the checkpoint id, under the store's lock, l: it is
// then kept or dropped by the retention rules at the next prune. An id
// that is not pinned is an error.
func (s *Store) ReleasePin(l *Lock, id ID) error {
	if id.Latest {
		return fmt.Errorf("checkpoint: release a checkpoint, not %s", id)
	}
	found := false
	err := s.updatePins(l, func(ps []Pin) ([]Pin, bool) {
		out := ps[:0:0]
		for _, q := range ps {
			if q.ID == id.String() {
				found = true
				continue
			}
			out = append(out, q)
		}
		return out, found
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("checkpoint: %s is not pinned", id)
	}
	s.Logf("released %s", id)
	return nil
}

// recovered unpins key's checkpoints that m, a later checkpoint of key,
// holds at least as many entries and bytes as. It returns those unpinned.
func (s *Store) recovered(l *Lock, key string, m Meta) ([]string, error) {
	var gone []string
	err := s.updatePins(l, func(ps []Pin) ([]Pin, bool) {
		out := ps[:0:0]
		for _, q := range ps {
			if id, err := ParseID(q.ID); err == nil && id.Key == key && m.Entries >= q.Entries && m.Bytes >= q.Bytes {
				gone = append(gone, q.ID)
				continue
			}
			out = append(out, q)
		}
		return out, len(gone) > 0
	})
	return gone, err
}

// forgetPins drops the pins of key whose checkpoint is not in es, the
// key's listing: one removed by hand is pinned no more.
func (s *Store) forgetPins(l *Lock, key string, es []Entry) error {
	have := map[string]bool{}
	for _, e := range es {
		have[e.ID.String()] = true
	}
	return s.updatePins(l, func(ps []Pin) ([]Pin, bool) {
		out := ps[:0:0]
		for _, q := range ps {
			if id, err := ParseID(q.ID); err == nil && id.Key == key && !have[q.ID] {
				s.Logf("unpinned %s: it is gone", q.ID)
				continue
			}
			out = append(out, q)
		}
		return out, len(out) != len(ps)
	})
}
