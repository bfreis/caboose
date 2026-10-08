package apkobuild

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/tarfs"
)

// CheckResult is what a spec resolves to, as the repository indexes say.
type CheckResult struct {
	// Packages is how many packages it resolves to, dependencies included.
	Packages int
	// InstalledBytes is what they take installed: the sum of the indexes'
	// installed sizes.
	InstalledBytes int64
}

// ResolutionError is a package list the repository indexes, once read,
// cannot satisfy: a name they do not have, or dependencies that conflict.
// Unlike a failure to fetch or read them, it is about the list itself.
type ResolutionError struct{ Err error }

func (e *ResolutionError) Error() string { return e.Err.Error() }
func (e *ResolutionError) Unwrap() error { return e.Err }

// maxSuggestions is the most close names an unknown package's error offers.
const maxSuggestions = 3

// Check resolves the spec's packages and their dependencies against the
// repository indexes alone: it fetches the signing keys and each
// repository's signed APKINDEX, checks its signature, and resolves in
// memory, downloading no package -- where Resolve also reads every
// package's control section for the lock. It is for saying quickly whether
// a package list would build, and how large it would be.
//
// A name no package in the indexes has, nor provides, is an error that
// names it, with up to three close names the index does have. That, and
// a list the indexes cannot satisfy, is a *ResolutionError: the list is
// wrong. Any other error (the network, the indexes, the options) says
// nothing about the list.
func Check(ctx context.Context, spec Spec, o Options) (CheckResult, error) {
	input, err := spec.List()
	if err != nil {
		return CheckResult{}, err
	}
	return checkList(ctx, input, o)
}

func checkList(ctx context.Context, input []string, o Options) (CheckResult, error) {
	if err := o.check(); err != nil {
		return CheckResult{}, err
	}
	unlock, err := LockCache(o.CacheDir)
	if err != nil {
		return CheckResult{}, err
	}
	defer unlock()
	repos, keyring := o.Repositories, o.Keyring
	if repos == nil {
		repos = []string{WolfiRepository}
	}
	if keyring == nil {
		keyring = []string{WolfiKey}
	}
	opts := []apk.Option{
		apk.WithFS(tarfs.New()),
		apk.WithArch(o.Arch),
		apk.WithIgnoreMknodErrors(true),
		apk.WithIgnoreIndexSignatures(o.IgnoreSignatures),
		apk.WithTransport(o.Transport),
	}
	if o.CacheDir != "" {
		opts = append(opts, apk.WithCache(o.CacheDir, false, apk.NewCache(true)))
	}
	a, err := apk.New(ctx, opts...)
	if err != nil {
		return CheckResult{}, fmt.Errorf("preparing to check packages: %w", err)
	}
	// No build repositories given to InitDB: it would discover keys for
	// them over the network. The keyring is given outright.
	if err := a.InitDB(ctx); err != nil {
		return CheckResult{}, fmt.Errorf("preparing to check packages: %w", err)
	}
	if err := a.InitKeyring(ctx, keyring, nil); err != nil {
		return CheckResult{}, fmt.Errorf("fetching the repositories' signing keys: %w", err)
	}
	if err := a.SetRepositories(ctx, repos); err != nil {
		return CheckResult{}, fmt.Errorf("preparing to check packages: %w", err)
	}
	indexes, err := a.GetRepositoryIndexes(ctx, o.IgnoreSignatures)
	if err != nil {
		return CheckResult{}, fmt.Errorf("reading the repository indexes: %w", err)
	}
	if err := unknownNames(indexes, input); err != nil {
		return CheckResult{}, &ResolutionError{Err: err}
	}
	pkgs, _, err := apk.NewPkgResolver(ctx, indexes).GetPackagesWithDependencies(ctx, input, nil)
	if err != nil {
		return CheckResult{}, &ResolutionError{Err: fmt.Errorf("resolving packages: %w", err)}
	}
	r := CheckResult{Packages: len(pkgs)}
	for _, p := range pkgs {
		r.InstalledBytes += int64(p.InstalledSize)
	}
	return r, nil
}

// unknownNames is an error naming every name in input that no package in
// indexes has or provides, each with the close names there are.
func unknownNames(indexes []apk.NamedIndex, input []string) error {
	known := map[string]bool{}
	var names []string
	for _, idx := range indexes {
		for _, p := range idx.Packages() {
			if !known[p.Name] {
				names = append(names, p.Name)
			}
			known[p.Name] = true
			for _, pr := range p.Provides {
				if i := strings.IndexAny(pr, "=<>~"); i >= 0 {
					pr = pr[:i]
				}
				known[pr] = true
			}
		}
	}
	var errs []error
	for _, in := range input {
		if known[in] {
			continue
		}
		msg := fmt.Sprintf("no package named %q", in)
		if s := Suggest(in, names); len(s) > 0 {
			msg += fmt.Sprintf(" (did you mean %s?)", strings.Join(s, ", "))
		}
		errs = append(errs, errors.New(msg))
	}
	return errors.Join(errs...)
}

// Suggest returns up to three names from names close to name, closest
// first: within two edits of it, or the same name with another version
// suffix (nodejs-24 for nodejs-22).
func Suggest(name string, names []string) []string {
	type cand struct {
		name string
		d    int
	}
	stem := versionStem(name)
	var cs []cand
	for _, n := range names {
		if n == name {
			continue
		}
		d := editDistance(name, n, 2)
		if d > 2 {
			if stem == "" || versionStem(n) != stem {
				continue
			}
			d = 3 // after every close spelling
		}
		cs = append(cs, cand{n, d})
	}
	slices.SortFunc(cs, func(a, b cand) int {
		if a.d != b.d {
			return a.d - b.d
		}
		return strings.Compare(a.name, b.name)
	})
	var out []string
	for _, c := range cs {
		if len(out) == maxSuggestions {
			break
		}
		if !slices.Contains(out, c.name) {
			out = append(out, c.name)
		}
	}
	return out
}

// versionStem is name without a version suffix -- "nodejs" for nodejs-22,
// "python" for python-3.13 -- or "" when it has none.
func versionStem(name string) string {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	for _, c := range name[i+1:] {
		if (c < '0' || c > '9') && c != '.' {
			return ""
		}
	}
	return name[:i]
}

// editDistance is the Levenshtein distance between a and b, or max+1 once
// it is known to exceed max.
func editDistance(a, b string, max int) int {
	if d := len(a) - len(b); d > max || -d > max {
		return max + 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		low := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			low = min(low, cur[j])
		}
		if low > max {
			return max + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
