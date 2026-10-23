package conformance

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/bfreis/caboose/internal/nofollow"
)

// GuardHost is the real Host: the Guard's canaries, read from the host
// side without following a symlink. It changes nothing but by MakeCanary.
type GuardHost struct {
	G *Guard

	mu  sync.Mutex
	log []Mutation
}

func (h *GuardHost) MakeCanary(parent, role string, kind Kind) (Canary, error) {
	c, err := h.G.MakeCanary(parent, role, kind)
	if err != nil {
		return c, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range c.Paths {
		h.log = append(h.log, Mutation{"make", p})
	}
	return c, nil
}

func (h *GuardHost) Canary(role string) (Canary, error) {
	if h.G == nil || !h.G.made {
		return Canary{}, refuse("guard", "not an environment this suite created")
	}
	h.G.mu.Lock()
	defer h.G.mu.Unlock()
	for _, r := range h.G.canaries {
		if r.canary.Role == role {
			return r.canary, nil
		}
	}
	return Canary{}, fmt.Errorf("conformance: no canary %q was made", role)
}

// inCanary reports whether rel is a canary path or below one.
func (h *GuardHost) inCanary(rel string) bool {
	h.G.mu.Lock()
	defer h.G.mu.Unlock()
	for p := range h.G.canaries {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// Canaries are every canary the guard made, each once, sorted by Rel.
func (h *GuardHost) Canaries() []Canary {
	if h.G == nil || !h.G.made {
		return nil
	}
	return h.G.Canaries()
}

// Recorded is what the guard last recorded of the canary path rel.
func (h *GuardHost) Recorded(rel string) (Identity, bool, error) {
	if h.G == nil || !h.G.made {
		return Identity{}, false, refuse("guard", "not an environment this suite created")
	}
	id, gone, ok := h.G.CanaryRecord(rel)
	if !ok {
		return Identity{}, false, fmt.Errorf("conformance: %s is not a canary of this run", rel)
	}
	return id, gone, nil
}

// Nonce is the run's nonce, "" without a guard.
func (h *GuardHost) Nonce() string {
	if h.G == nil {
		return ""
	}
	return h.G.nonce
}

// ProbeHash is the sha256 of the probe the suite placed, "" for none.
func (h *GuardHost) ProbeHash() string {
	if h.G == nil {
		return ""
	}
	return h.G.ProbeHash()
}

// Identity reads the identity of a canary path, a path below one, or the
// suite's probe, which the suite places in the writable part beside the
// canaries (ProbeRel) and the daemon checks require in place.
func (h *GuardHost) Identity(rel string) (Identity, error) {
	if h.G == nil || !h.G.made {
		return Identity{}, refuse("guard", "not an environment this suite created")
	}
	if !h.inCanary(rel) && !IsProbeRel(h.G.Nonce8(), rel) {
		return Identity{}, fmt.Errorf("conformance: %s is not a canary of this run", rel)
	}
	return identityOf(nofollow.Dir(h.G.base), rel)
}

func (h *GuardHost) Snapshot(c Canary) (map[string]Identity, error) {
	if h.G == nil || !h.G.made {
		return nil, refuse("guard", "not an environment this suite created")
	}
	if !h.G.IsCanary(c.Rel) {
		return nil, fmt.Errorf("conformance: %s is not a canary of this run", c.Rel)
	}
	d := nofollow.Dir(h.G.base)
	out := map[string]Identity{}
	id, err := identityOf(d, c.Rel)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out[c.Rel] = id
	if !id.Mode.IsDir() {
		return out, nil
	}
	err = d.Walk(c.Rel, func(rel string, e fs.DirEntry) error {
		if len(out) >= maxWalkEntries {
			return fmt.Errorf("conformance: canary %s holds too many entries", c.Role)
		}
		i, err := identityOf(d, rel)
		if err != nil {
			return err
		}
		out[rel] = i
		return nil
	})
	return out, err
}

func (h *GuardHost) Mutations() []Mutation {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Mutation(nil), h.log...)
}
