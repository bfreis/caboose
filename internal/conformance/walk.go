package conformance

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
)

// The post-check walk's bounds: more entries, or deeper, is an error,
// never a walk cut short.
const (
	maxWalkEntries = 200000
	maxWalkDepth   = 64
)

// Snapshot is the identity of every path under B the post-check walk
// compares, by path relative to B.
type Snapshot map[string]Identity

// productPatterns are productWritten, and under vm vmProductWritten, with
// the environment's name filled in.
func (g *Guard) productPatterns() []string {
	return fillProductPatterns(g.isolation, g.DataRel(), g.env)
}

// fillProductPatterns are the product patterns of isolation, "<data>" and
// "<env>" replaced by data and env.
func fillProductPatterns(isolation, data, env string) []string {
	src := slices.Clone(productWritten)
	if isolation == config.KindVM {
		src = append(src, vmProductWritten...)
	}
	out := make([]string, len(src))
	for i, p := range src {
		p = strings.ReplaceAll(p, "<data>", data)
		out[i] = strings.ReplaceAll(p, "<env>", env)
	}
	return out
}

// productPath reports whether rel is one the product writes. Only paths
// under home/ can be: roots/ and canary/ are walked in full, and so is
// anything else at B's top.
func productPath(patterns []string, rel string) bool {
	if !strings.HasPrefix(rel, "home/") {
		return false
	}
	for _, p := range patterns {
		if dir, ok := strings.CutSuffix(p, "/"); ok {
			if rel == dir || strings.HasPrefix(rel, p) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		// A file or directory below a matched element (data/home/.claude/x).
		for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
			if ok, _ := path.Match(p, d); ok {
				return true
			}
		}
	}
	return false
}

// Snapshot walks B without following a symlink: roots/ and canary/ in
// full, home/ and data/ but for the subtrees the product writes. Canary
// paths inside those subtrees are recorded too, by identity.
func (g *Guard) Snapshot() (Snapshot, error) {
	if g == nil || !g.made {
		return nil, refuse("guard", "not an environment this suite created")
	}
	d := nofollow.Dir(g.base)
	pats := g.productPatterns()
	s := Snapshot{}
	err := d.Walk("", func(rel string, e fs.DirEntry) error {
		if strings.Count(rel, "/") >= maxWalkDepth {
			return fmt.Errorf("conformance: the base is deeper than %d levels at <B>/%s", maxWalkDepth, rel)
		}
		if len(s) >= maxWalkEntries {
			return fmt.Errorf("conformance: the base holds more than %d entries", maxWalkEntries)
		}
		if productPath(pats, rel) {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if slices.Contains(suiteWritten, rel) && e.Type().IsRegular() {
			return nil
		}
		id, err := identityOf(d, rel)
		if err != nil {
			return err
		}
		s[rel] = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, p := range g.canaryPaths() {
		if _, ok := s[p]; ok {
			continue
		}
		id, err := identityOf(d, p)
		if err != nil {
			continue // gone: Compare reports it
		}
		s[p] = id
	}
	return s, nil
}

// NoteConfigWritten records the environment's config.toml, as it is now,
// as the suite's own write: whatever of the suite rewrites it on purpose
// (setting the trust or the egress keys) calls it right after. The
// post-check walk then takes a change to config.toml as no touch while
// its contents are exactly those; any other contents, written by anything
// else, are a change like any other.
func (g *Guard) NoteConfigWritten() error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	id, err := identityOf(nofollow.Dir(g.base), g.ConfigRel())
	if err != nil {
		return err
	}
	if !id.Mode.IsRegular() || len(id.Hash) != 64 {
		return fmt.Errorf("conformance: %s is not a plain, readable file", g.ConfigRel())
	}
	g.mu.Lock()
	g.configHash = id.Hash
	g.mu.Unlock()
	return nil
}

// TouchedError is the suite's own sandbox having changed something it was
// not allowed to: the run ends.
type TouchedError struct{ Paths []string }

func (e *TouchedError) Error() string {
	return "ERROR: the suite's own sandbox touched <B>/" + strings.Join(e.Paths, ", <B>/")
}

// PostCheck runs after every check: it takes a new snapshot and compares
// it with before. allowed are the canaries the check was handed (each a
// path of a canary of this run; anything else is refused): a change at or
// below one of them is the check's to judge. Any other change, and any
// canary whose identity differs from before, is a *TouchedError. The new
// snapshot is returned as the next check's before.
func (g *Guard) PostCheck(before Snapshot, allowed []string) (Snapshot, error) {
	for _, a := range allowed {
		// The probe is allowed as a canary is: only a wipe step lists it
		// (Steps), since it removes the writable part the probe is in.
		if !g.IsCanary(a) && !IsProbeRel(g.Nonce8(), a) {
			return nil, fmt.Errorf("conformance: %s is not a canary of this run", a)
		}
	}
	after, err := g.Snapshot()
	if err != nil {
		return nil, err
	}
	if changed := g.compare(before, after, allowed); len(changed) > 0 {
		return after, &TouchedError{Paths: changed}
	}
	// What the check did to its own canaries is its result, not the next
	// check's: record their state now.
	g.mu.Lock()
	for p, r := range g.canaries {
		for _, a := range allowed {
			if p == a || strings.HasPrefix(p, a+"/") {
				r.id, r.gone = after[p], false
				if _, there := after[p]; !there {
					r.gone = true
				}
				g.canaries[p] = r
			}
		}
	}
	g.mu.Unlock()
	return after, nil
}

func (g *Guard) compare(before, after Snapshot, allowed []string) []string {
	ok := func(rel string) bool {
		for _, a := range allowed {
			if rel == a || strings.HasPrefix(rel, a+"/") {
				return true
			}
		}
		return false
	}
	// config.toml is no product path: a change to it is allowed only to
	// the contents the suite itself last wrote there (NoteConfigWritten).
	g.mu.Lock()
	cfgHash := g.configHash
	g.mu.Unlock()
	cfgRel := g.ConfigRel()
	suiteConfig := func(rel string, a Identity) bool {
		return rel == cfgRel && cfgHash != "" && a.Mode.IsRegular() && a.Hash == cfgHash
	}
	var changed []string
	for rel, b := range before {
		a, there := after[rel]
		if (!there || !a.sameAs(b)) && !ok(rel) && !(there && suiteConfig(rel, a)) {
			changed = append(changed, rel)
		}
	}
	for rel, a := range after {
		if _, there := before[rel]; !there && !ok(rel) && !suiteConfig(rel, a) {
			changed = append(changed, rel)
		}
	}
	// A canary not allowed must still be as it was made.
	for _, p := range g.canaryPaths() {
		if ok(p) || slices.Contains(changed, p) {
			continue
		}
		g.mu.Lock()
		r := g.canaries[p]
		g.mu.Unlock()
		a, there := after[p]
		if there == r.gone || there && !a.sameAs(r.id) {
			changed = append(changed, p)
		}
	}
	slices.Sort(changed)
	return changed
}
