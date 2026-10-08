package apkobuild

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/build"
	"chainguard.dev/apko/pkg/build/types"
	pkglock "chainguard.dev/apko/pkg/lock"
	"chainguard.dev/apko/pkg/tarfs"
)

// ApkoVersion is the apko module's version, mixed into every lock's hash
// since a new apko can build a different image from the same lock. Bump it
// with go.mod.
const ApkoVersion = "v1.4.8"

// The repository and signing key packages come from unless Options says
// otherwise.
const (
	WolfiRepository = "https://packages.wolfi.dev/os"
	WolfiKey        = "https://packages.wolfi.dev/os/wolfi-signing.rsa.pub"
)

// inputPrefix marks the lock's config name as caboose's: the spec it was
// resolved for and the package list that spec stood for then (lockMeta, as
// JSON), which apko's format has no fields of its own for.
const inputPrefix = "caboose: "

// lockMeta is what a lock's config name holds after inputPrefix.
type lockMeta struct {
	// Spec is the profile's own spec the lock was resolved for: what the
	// user asked for, apart from what caboose's groups made of it.
	Spec *lockSpec `json:"spec"`
	// Input is the package list Spec stood for, which was resolved.
	Input []string `json:"input"`
}

type lockSpec struct {
	Packages []string `json:"packages"`
	Defaults bool     `json:"defaults"`
}

// ConfigName is the config name of a lock resolved for spec from input,
// the package list spec stood for: what Resolve writes, for a lock written
// by other means (a test's).
func ConfigName(spec Spec, input []string) string {
	n := spec.Normal()
	b, err := json.Marshal(lockMeta{Spec: &lockSpec{Packages: n.Packages, Defaults: n.Defaults}, Input: input})
	if err != nil {
		panic(err) // strings and a bool always encode
	}
	return inputPrefix + string(b)
}

// parseConfigName reads a config name ConfigName wrote.
func parseConfigName(name string) (lockMeta, bool) {
	var m lockMeta
	rest, ok := strings.CutPrefix(name, inputPrefix)
	if !ok || json.Unmarshal([]byte(rest), &m) != nil || m.Spec == nil || len(m.Input) == 0 {
		return lockMeta{}, false
	}
	return m, true
}

// epoch is the time every image is built at, so the same lock gives the same
// bytes.
var epoch = time.Unix(0, 0)

// Options says where packages come from and how they are fetched.
type Options struct {
	// Arch is the apk architecture to resolve or build for: "aarch64" or
	// "x86_64" (see ArchFor).
	Arch string
	// CacheDir keeps downloaded packages and indexes between runs; empty
	// keeps nothing on disk.
	CacheDir string
	// Repositories and Keyring are Wolfi's when nil.
	Repositories []string
	Keyring      []string
	// Transport carries every request; nil is the default transport.
	Transport http.RoundTripper
	// IgnoreSignatures skips signature checks, for tests only.
	IgnoreSignatures bool
}

// ArchFor maps a Go architecture name to the apk architecture.
func ArchFor(goarch string) (string, error) {
	switch goarch {
	case "arm64":
		return "aarch64", nil
	case "amd64":
		return "x86_64", nil
	}
	return "", fmt.Errorf("architecture %q is not supported: packages exist for arm64 and amd64", goarch)
}

func (o Options) check() error {
	switch o.Arch {
	case "aarch64", "x86_64":
		return nil
	case "":
		return fmt.Errorf("no architecture given: want aarch64 or x86_64")
	}
	return fmt.Errorf("architecture %q is not supported: want aarch64 or x86_64", o.Arch)
}

// buildOptions are the apko options both resolving and building start from.
func (o Options) buildOptions(pkgs []string) ([]build.Option, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	ic := types.ImageConfiguration{}
	ic.Contents.Repositories = o.Repositories
	ic.Contents.Keyring = o.Keyring
	if o.Repositories == nil {
		ic.Contents.Repositories = []string{WolfiRepository}
	}
	if o.Keyring == nil {
		ic.Contents.Keyring = []string{WolfiKey}
	}
	ic.Contents.Packages = pkgs
	opts := []build.Option{
		build.WithImageConfiguration(ic),
		build.WithArch(types.ParseArchitecture(o.Arch)),
		build.WithSourceDateEpoch(epoch),
		build.WithIgnoreSignatures(o.IgnoreSignatures),
	}
	if o.CacheDir != "" {
		opts = append(opts, build.WithCache(o.CacheDir, false, apk.NewCache(true)))
	} else {
		opts = append(opts, build.WithoutDiskCache())
	}
	if o.Transport != nil {
		opts = append(opts, build.WithTransport(o.Transport))
	}
	return opts, nil
}

// Lock is a package list resolved to exact packages, in apko's own lock file
// format. The list it was resolved from, and the spec that list stood for,
// are kept in the config name.
type Lock struct {
	l    pkglock.Lock
	meta lockMeta
}

// PackageVersion is a locked package.
type PackageVersion struct {
	Name, Version string
}

// Resolve resolves the spec's packages and their dependencies to exact
// versions, downloading the indexes and the packages' metadata but unpacking
// nothing.
func Resolve(ctx context.Context, spec Spec, o Options) (*Lock, error) {
	input, err := spec.List()
	if err != nil {
		return nil, err
	}
	return resolveList(ctx, spec, input, o)
}

// resolveList resolves input, the already checked package list spec stands
// for.
func resolveList(ctx context.Context, spec Spec, input []string, o Options) (*Lock, error) {
	opts, err := o.buildOptions(input)
	if err != nil {
		return nil, err
	}
	unlock, err := LockCache(o.CacheDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	tmp, err := os.MkdirTemp("", "caboose-apko-*")
	if err != nil {
		return nil, fmt.Errorf("preparing to resolve packages: %w", err)
	}
	defer os.RemoveAll(tmp)
	opts = append(opts, build.WithTempDir(tmp))
	bc, err := build.New(ctx, tarfs.New(), opts...)
	if err != nil {
		return nil, fmt.Errorf("preparing to resolve packages: %w", err)
	}
	resolved, err := bc.ResolveWithBase(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving packages: %w", err)
	}

	ic := bc.ImageConfiguration()
	lock := pkglock.Lock{
		Version: "v1",
		Config:  &pkglock.Config{Name: ConfigName(spec, input)},
		Contents: pkglock.LockContents{
			Keyrings:                []pkglock.LockKeyring{},
			BuildRepositories:       []pkglock.LockRepo{},
			RuntimeOnlyRepositories: []pkglock.LockRepo{},
			Repositories:            []pkglock.LockRepo{},
			Packages:                make([]pkglock.LockPkg, 0, len(resolved)),
		},
	}
	for _, k := range ic.Contents.Keyring {
		lock.Contents.Keyrings = append(lock.Contents.Keyrings, pkglock.LockKeyring{Name: k, URL: k})
	}
	for _, r := range ic.Contents.Repositories {
		repo := apk.Repository{URI: r + "/" + o.Arch}
		lock.Contents.Repositories = append(lock.Contents.Repositories, pkglock.LockRepo{
			Name: r, URL: repo.IndexURI(), Architecture: o.Arch,
		})
	}
	for _, r := range resolved {
		p := pkglock.LockPkg{
			Name:         r.Package.Name,
			URL:          r.Package.URL(),
			Architecture: r.Package.Arch,
			Version:      r.Package.Version,
			Control: pkglock.LockPkgRangeAndChecksum{
				Range:    fmt.Sprintf("bytes=%d-%d", r.SignatureSize, r.SignatureSize+r.ControlSize-1),
				Checksum: "sha1-" + base64.StdEncoding.EncodeToString(r.ControlHash),
			},
			Data: pkglock.LockPkgRangeAndChecksum{
				Range:    fmt.Sprintf("bytes=%d-%d", r.SignatureSize+r.ControlSize, r.SignatureSize+r.ControlSize+r.DataSize-1),
				Checksum: "sha256-" + base64.StdEncoding.EncodeToString(r.DataHash),
			},
			Checksum: r.Package.ChecksumString(),
		}
		if r.SignatureSize != 0 {
			p.Signature = pkglock.LockPkgRangeAndChecksum{
				Range:    fmt.Sprintf("bytes=0-%d", r.SignatureSize-1),
				Checksum: "sha1-" + base64.StdEncoding.EncodeToString(r.SignatureHash),
			}
		}
		lock.Contents.Packages = append(lock.Contents.Packages, p)
	}
	return newLock(lock)
}

// newLock is l with its config name read.
func newLock(l pkglock.Lock) (*Lock, error) {
	if l.Config == nil {
		return nil, fmt.Errorf("the lock has no config")
	}
	m, ok := parseConfigName(l.Config.Name)
	if !ok {
		return nil, fmt.Errorf("the lock records no package spec of caboose's")
	}
	return &Lock{l: l, meta: m}, nil
}

// ReadLock reads a lock file written by Write.
func ReadLock(path string) (*Lock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading lock: %w", err)
	}
	var l pkglock.Lock
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("reading lock %s: %w", path, err)
	}
	if len(l.Contents.Packages) == 0 {
		return nil, fmt.Errorf("lock %s holds no packages", path)
	}
	lock, err := newLock(l)
	if err != nil {
		return nil, fmt.Errorf("lock %s was not made by this caboose (%v): resolve the packages again", path, err)
	}
	return lock, nil
}

// Write saves the lock at path, through a temporary file renamed over it so a
// reader never sees half of one.
func (l *Lock) Write(path string) error {
	b, err := json.MarshalIndent(l.l, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding lock: %w", err)
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".lock-*")
	if err != nil {
		return fmt.Errorf("writing lock: %w", err)
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing lock %s: %w", path, err)
	}
	return nil
}

// Input returns the package list the lock was resolved from.
func (l *Lock) Input() []string {
	return slices.Clone(l.meta.Input)
}

// Spec returns the spec the lock was resolved for, normal (Spec.Normal):
// what the profile asked for then, which Input is what caboose's groups
// made of.
func (l *Lock) Spec() Spec {
	return Spec{Packages: slices.Clone(l.meta.Spec.Packages), Defaults: l.meta.Spec.Defaults}.Normal()
}

// WithSpec returns a copy of the lock recording spec instead, which must
// stand for the same package list: a profile changed in a way that asks for
// the same packages (one the defaults already have, say) needs no new
// resolve.
func (l *Lock) WithSpec(spec Spec) (*Lock, error) {
	input, err := spec.List()
	if err != nil {
		return nil, err
	}
	if !slices.Equal(input, l.meta.Input) {
		return nil, fmt.Errorf("the spec stands for other packages than the lock was resolved from")
	}
	c := l.l
	cfg := *c.Config
	cfg.Name = ConfigName(spec, input)
	c.Config = &cfg
	return newLock(c)
}

// Packages returns every locked package, dependencies included, in the order
// they install.
func (l *Lock) Packages() []PackageVersion {
	out := make([]PackageVersion, len(l.l.Contents.Packages))
	for i, p := range l.l.Contents.Packages {
		out[i] = PackageVersion{p.Name, p.Version}
	}
	return out
}

// Arch returns the apk architecture the lock was resolved for.
func (l *Lock) Arch() string {
	return l.l.Contents.Packages[0].Architecture
}

// Hash returns a hex sha256 that names what the lock builds: the apko version,
// the architecture, the input and every locked package with its checksums.
// The same lock always has the same hash.
func (l *Lock) Hash() string {
	h := sha256.New()
	field := func(s string) {
		fmt.Fprintf(h, "%d:%s\n", len(s), s)
	}
	field("caboose-apkobuild-1")
	field(ApkoVersion)
	field(l.Arch())
	for _, s := range l.Input() {
		field("in")
		field(s)
	}
	for _, r := range l.l.Contents.Repositories {
		field("repo")
		field(r.URL)
	}
	for _, p := range l.l.Contents.Packages {
		for _, s := range []string{"pkg", p.Name, p.Version, p.Architecture, p.URL, p.Checksum, p.Control.Checksum, p.Data.Checksum} {
			field(s)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
