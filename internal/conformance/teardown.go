package conformance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/bfreis/caboose/internal/saferm"
)

// Remover removes the throwaway environment's engine objects, each by the
// exact name it is given: never by a pattern, a filter or a prefix.
type Remover interface {
	RemoveContainer(ctx context.Context, name string) error
	RemoveImage(ctx context.Context, name string) error
}

// LeftError is a teardown that deleted nothing, or not everything: B is
// left for the user to remove by hand, and Why says why.
type LeftError struct {
	Base string
	Why  error
}

func (e *LeftError) Error() string {
	return fmt.Sprintf("conformance: %s was left in place and must be removed by hand: %v", e.Base, e.Why)
}

func (e *LeftError) Unwrap() error { return e.Why }

// RemoveCanary removes a canary of this run, by its recorded path, through
// saferm with B as the root.
func (g *Guard) RemoveCanary(c Canary) error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	if !g.IsCanary(c.Rel) {
		return fmt.Errorf("conformance: %s is not a canary of this run", c.Rel)
	}
	g.mu.Lock()
	rec := g.canaries[c.Rel]
	g.mu.Unlock()
	if rec.canary.Rel != c.Rel {
		return fmt.Errorf("conformance: %s is not a canary's top", c.Rel)
	}
	if err := saferm.RemoveTree(g.base, filepath.Join(g.base, filepath.FromSlash(c.Rel))); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range rec.canary.Paths {
		r := g.canaries[p]
		r.gone = true
		g.canaries[p] = r
	}
	return nil
}

// Teardown removes the environment's container and images by their exact
// names, then B, through saferm. VerifyBase runs before each part; if it
// fails, nothing more is deleted and a *LeftError says so. The mount audit
// is not repeated: the sandbox it audits is what the first part removes.
func (g *Guard) Teardown(ctx context.Context, r Remover) error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	if err := g.VerifyBase(); err != nil {
		return &LeftError{Base: g.base, Why: err}
	}
	if r == nil {
		return &LeftError{Base: g.base, Why: errors.New("no way to remove the environment's container and images")}
	}
	container, images := g.ExactNames()
	if err := r.RemoveContainer(ctx, container); err != nil {
		return &LeftError{Base: g.base, Why: fmt.Errorf("removing container %s: %w", container, err)}
	}
	for _, im := range images {
		if err := r.RemoveImage(ctx, im); err != nil {
			return &LeftError{Base: g.base, Why: fmt.Errorf("removing image %s: %w", im, err)}
		}
	}
	if err := g.VerifyBase(); err != nil {
		return &LeftError{Base: g.base, Why: err}
	}
	// The parts first, by their known names, as trees, since a check may
	// have left read-only directories; then B itself.
	for _, sub := range []string{"roots", "canary", "home"} {
		if err := saferm.RemoveTree(g.base, filepath.Join(g.base, sub)); err != nil {
			return &LeftError{Base: g.base, Why: err}
		}
	}
	if err := saferm.RemoveRoot(g.base); err != nil {
		return &LeftError{Base: g.base, Why: err}
	}
	return nil
}
