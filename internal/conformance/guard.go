// Package conformance is caboose's conformance suite: it builds a
// throwaway environment, has its sandbox try what a sandbox must not do,
// and confirms from the host that each attempt was refused.
//
// This file and its neighbours are the suite's safety envelope. Nothing
// here trusts the disk to say who the suite is: the base directory, the
// environment's name and the nonce are kept in memory from the moment they
// are made, and the disk is read only to confirm them. Guard.Verify runs at
// start, before each check group and before every destructive check, and
// refuses unless the environment is exactly the one this run created, the
// sandbox reaches nothing but what the run made, and this process is not
// itself in a sandbox.
package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
)

// SuiteVersion is written into the marker.
const SuiteVersion = "1"

// Exit statuses the command gives.
const (
	ExitRefused = 2 // the envelope refused: nothing was run or deleted
	GateExit    = 3 // what the sandbox-side gate exits with on a mismatch
)

// The names of the throwaway environment's parts.
const (
	BasePrefix = "caboose-conf."
	MarkerFile = "home/.conformance"
	RootMarker = ".conformance-root"
	// DataMarkerName is the nonce file in the environment's data dir
	// (DataMarker).
	DataMarkerName = ".conformance-data"
	// StubDir holds the suite's stub opener, notifier and dialogs, first
	// on the PATH of the daemon and its links; StubLog is the one file
	// they write, a line per call. Both are in canary/, which no mount
	// reaches.
	StubDir     = "canary/bin"
	StubLog     = "canary/stub-calls"
	defaultTemp = "/tmp"
)

// The environment is named EnvPrefix and 8 hex digits. The name is short
// because it is in a VM's socket path twice (B/home/envs/<env>/data/vm/
// caboose-<env>/vm.sock), which on a Mac must fit in 103 bytes with B in
// /private/tmp. The worst case is 94 bytes, and TestConfLayoutFitsVMSocket
// (internal/launcher) allows 95, the limit less a margin of 8: a longer
// part anywhere in that path (BasePrefix, the env, the vm directory's
// names) fails it on purpose. EnvPattern is the name as a regular
// expression, unanchored.
const (
	EnvPrefix  = "cf-"
	EnvPattern = `cf-[0-9a-f]{8}`
	// LeftoverEnvPattern also takes the name earlier versions of the
	// suite gave the environment, conformance-<8 hex>, so that what one
	// of their runs left is still found. Never a name Setup gives.
	LeftoverEnvPattern = `(?:cf|conformance)-[0-9a-f]{8}`
)

var (
	envRe         = regexp.MustCompile(`^` + EnvPattern + `$`)
	leftoverEnvRe = regexp.MustCompile(`^` + LeftoverEnvPattern + `$`)
)

// ValidEnvName reports whether name is a name this suite gives an
// environment. It is never the name of an environment of the user's.
func ValidEnvName(name string) bool { return envRe.MatchString(name) && config.ValidEnv(name) }

// LeftoverEnvName reports whether name is a name this suite, or an
// earlier version of it, gave an environment: for listing leftovers only.
func LeftoverEnvName(name string) bool {
	return leftoverEnvRe.MatchString(name) && config.ValidEnv(name)
}

// Facts are what the envelope needs from outside the package. Each that
// is missing makes the guard refuse rather than guess.
type Facts struct {
	// TempRoot is where B is made: the physical /tmp, never $TMPDIR
	// (a Mac's is too long for the socket paths under B). "" is /tmp.
	TempRoot string
	// CabooseHome is the caller's real CABOOSE_HOME (the default
	// ~/.caboose, or what the caller's environment names), and UserHome
	// the caller's home directory. Compared as paths only: nothing in them
	// is read.
	CabooseHome, UserHome string
	// RealRoots returns the host paths of every root of every environment
	// of the caller's, from the launcher's loader in a mode that returns
	// paths and reads no contents. Required.
	RealRoots func() ([]string, error)
	// Detectors are the signs of a sandbox; nil is DefaultDetectors.
	Detectors []Detector
	// GOOS is the platform; "" is runtime.GOOS.
	GOOS string
}

// Inputs are what the launcher side gives the guard once B exists. Each
// that is missing makes the check that needs it refuse.
type Inputs struct {
	// LaunchEnv is the environment the launcher side runs the launcher
	// with (built with Guard.LaunchEnv). Required for every Verify.
	LaunchEnv []string
	// Inspect reads what the sandbox was really created with. Required
	// for Verify once the sandbox exists.
	Inspect func(ctx context.Context) (Inspection, error)
	// Pin reads the sandbox's and the engine's identity. Required before
	// any destructive check.
	Pin func(ctx context.Context) (Pins, error)
	// PlannedShares are, under vm, the shares the launcher planned for B,
	// which the shares in machine.json must equal. Required under vm.
	PlannedShares []Share
	// Volumes are the names of the engine volumes the sandbox may have
	// (derived from the environment's name). Nil is none.
	Volumes []string
	// BootInputs are, under vm, the exact host paths outside B of the
	// files a VM may boot from (the kernel, the builder's disk): read-only
	// inputs, allowed in machine.json one by one, never as a directory.
	BootInputs []string
}

// Attach gives the guard the launcher side's inputs.
func (g *Guard) Attach(in Inputs) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.in = in
}

func (g *Guard) inputs() Inputs {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.in
}

// Guard is the envelope of one run. Only Setup makes a usable one.
type Guard struct {
	made      bool
	base      string // B, physical
	env       string
	nonce     string
	isolation string
	trust     string
	started   time.Time
	facts     Facts
	in        Inputs

	mu          sync.Mutex
	canaries    map[string]canaryRecord // by path relative to B
	pinned      *Pins
	pinnedAudit *AuditResult
	audited     *AuditResult
	inspectedID string // the container ID the last Verify inspected
	verifies    int
	// configHash is the content hash of config.toml as the suite last
	// rewrote it (NoteConfigWritten); "" before it has.
	configHash string
	// probeHash is the sha256 of the probe the suite placed (RecordProbe);
	// "" before it has.
	probeHash string
}

// RecordProbe records the sha256, in hex, of the probe binary the suite
// placed at ProbeRel: once.
func (g *Guard) RecordProbe(sum string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.probeHash != "" {
		return fmt.Errorf("conformance: the probe was placed already")
	}
	g.probeHash = sum
	return nil
}

// ProbeHash is what RecordProbe recorded, "" for none.
func (g *Guard) ProbeHash() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.probeHash
}

// Marker is the JSON in home/.conformance.
type Marker struct {
	SuiteVersion string `json:"suite_version"`
	Nonce        string `json:"nonce"`
	Env          string `json:"env"`
	Base         string `json:"base"`
	Isolation    string `json:"isolation"`
	Trust        string `json:"trust"`
	Started      string `json:"started"`
}

// RefusalError is the envelope refusing: Item names which check failed.
type RefusalError struct {
	Item string
	Err  error
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("conformance: refused (%s): %v", e.Item, e.Err)
}

func (e *RefusalError) Unwrap() error { return e.Err }

func refuse(item string, format string, args ...any) error {
	return &RefusalError{Item: item, Err: fmt.Errorf(format, args...)}
}

// Preflight is the start-up check that runs before anything is created:
// the platform, and that this process is not inside a sandbox.
func Preflight(f Facts) error {
	goos := f.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if err := checkPlatform(goos); err != nil {
		return &RefusalError{Item: "platform", Err: err}
	}
	if err := detectSandbox(f.Detectors); err != nil {
		return &RefusalError{Item: "sandbox", Err: err}
	}
	return nil
}

// Setup runs Preflight, then makes B, the environment's name and the
// nonce, and writes the marker and the nonce files. It refuses an
// isolation or trust it does not know. On an error after B was made, B is
// left in place and the error names it.
func Setup(f Facts, isolation, trust string) (*Guard, error) {
	if err := Preflight(f); err != nil {
		return nil, err
	}
	if !knownIsolation(isolation) {
		return nil, refuse("isolation", "unknown isolation %q", isolation)
	}
	if trust != config.TrustFallible && trust != config.TrustHostile {
		return nil, refuse("trust", "unknown trust %q", trust)
	}
	if f.CabooseHome == "" || f.UserHome == "" || f.RealRoots == nil {
		return nil, refuse("base", "the real CABOOSE_HOME, home directory and roots must be given")
	}
	tmp, err := physicalTemp(f.TempRoot)
	if err != nil {
		return nil, refuse("base", "%v", err)
	}
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	id, err := randomHex(4)
	if err != nil {
		return nil, err
	}
	made, err := os.MkdirTemp(tmp, BasePrefix)
	if err != nil {
		return nil, refuse("base", "the temp directory is not writable: %v", err)
	}
	base, err := filepath.EvalSymlinks(made)
	if err != nil {
		return nil, refuse("base", "%s: %v", made, err)
	}
	g := &Guard{
		made:      true,
		base:      base,
		env:       EnvPrefix + id,
		nonce:     nonce,
		isolation: isolation,
		trust:     trust,
		started:   time.Now().UTC(),
		facts:     f,
		canaries:  map[string]canaryRecord{},
	}
	if err := g.checkBase(); err != nil {
		return nil, fmt.Errorf("%w; %s was made and is left in place", err, base)
	}
	if err := g.writeMarkers(); err != nil {
		return nil, fmt.Errorf("conformance: setting up %s (left in place): %w", base, err)
	}
	return g, nil
}

func knownIsolation(s string) bool {
	for _, k := range config.Kinds {
		if s == k {
			return true
		}
	}
	return false
}

func physicalTemp(t string) (string, error) {
	if t == "" {
		t = defaultTemp
	}
	p, err := filepath.EvalSymlinks(t)
	if err != nil {
		return "", fmt.Errorf("the temp directory %s: %v", t, err)
	}
	return p, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (g *Guard) writeMarkers() error {
	d := nofollow.Dir(g.base)
	for _, sub := range []string{"home", g.DataRel(), "roots", "canary"} {
		if err := mkdirNoFollow(g.base, sub); err != nil {
			return err
		}
	}
	m, err := json.Marshal(g.marker())
	if err != nil {
		return err
	}
	if err := d.WriteFile(MarkerFile, append(m, '\n'), 0o600); err != nil {
		return err
	}
	if err := d.WriteFile(g.DataMarker(), []byte(g.nonce+"\n"), 0o600); err != nil {
		return err
	}
	for _, r := range g.RootNames() {
		if err := mkdirNoFollow(g.base, "roots/"+r); err != nil {
			return err
		}
		if err := d.WriteFile("roots/"+r+"/"+RootMarker, []byte(g.nonce+"\n"), 0o644); err != nil {
			return err
		}
	}
	return mkdirNoFollow(g.base, "roots/"+g.RootNames()[1]+"/"+WritablePart)
}

func (g *Guard) marker() Marker {
	return Marker{
		SuiteVersion: SuiteVersion,
		Nonce:        g.nonce,
		Env:          g.env,
		Base:         g.base,
		Isolation:    g.isolation,
		Trust:        g.trust,
		Started:      g.started.Format(time.RFC3339),
	}
}

// WritablePart is the writable part of the suite's writable root.
const WritablePart = "part"

// Base is B, the physical path of the run's base directory.
func (g *Guard) Base() string { return g.base }

// Env is the throwaway environment's name.
func (g *Guard) Env() string { return g.env }

// Nonce is the run's nonce, 32 hex digits.
func (g *Guard) Nonce() string { return g.nonce }

// Nonce8 is the nonce's first 8 hex digits, which names the canaries.
func (g *Guard) Nonce8() string { return g.nonce[:8] }

// Started is when the run began.
func (g *Guard) Started() time.Time { return g.started }

// Isolation and Trust are what the run tests.
func (g *Guard) Isolation() string { return g.isolation }
func (g *Guard) Trust() string     { return g.trust }

// CabooseHome is B/home, the throwaway CABOOSE_HOME; DataDir is the
// environment's default data dir in it, B/home/envs/<env>/data, which the
// launcher derives itself: the suite never sets CABOOSE_DATA_DIR, with
// which the launcher would keep the link in a helper of its own instead of
// the daemon, and the daemon's checkpoints and broker would not
// serve the environment.
func (g *Guard) CabooseHome() string { return filepath.Join(g.base, "home") }
func (g *Guard) DataDir() string     { return filepath.Join(g.base, filepath.FromSlash(g.DataRel())) }

// EnvDirRel is the environment's directory, DataRel its data dir,
// StoreRel its checkpoint store and DataMarker the data dir's nonce file,
// each relative to B, slash-separated.
func (g *Guard) EnvDirRel() string  { return "home/envs/" + g.env }
func (g *Guard) DataRel() string    { return g.EnvDirRel() + "/data" }
func (g *Guard) ConfigRel() string  { return g.EnvDirRel() + "/config.toml" }
func (g *Guard) StoreRel() string   { return g.EnvDirRel() + "/checkpoints" }
func (g *Guard) DataMarker() string { return g.DataRel() + "/" + DataMarkerName }

// RootNames are the two roots' names: the read-only one, then the one
// with a writable part. Never "ro" or "dev" alone, so that nothing of the
// suite's can be taken for a user's root.
func (g *Guard) RootNames() []string {
	return []string{"conf-" + g.Nonce8() + "-ro", "conf-" + g.Nonce8() + "-dev"}
}

// RootPath is the host path of the root named name.
func (g *Guard) RootPath(name string) string { return filepath.Join(g.base, "roots", name) }

// Verifies is how many times Verify ran, for tests.
func (g *Guard) Verifies() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.verifies
}

// VerifyBase is Verify without the mount audit: the checks of B, the
// marker, the names, the nonce files, the launch environment and the
// sandbox detectors. It is what runs before the sandbox exists and at
// teardown, once it is gone.
func (g *Guard) VerifyBase() error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	if err := g.checkBase(); err != nil {
		return err
	}
	if err := g.checkMarker(); err != nil {
		return err
	}
	if !ValidEnvName(g.env) {
		return refuse("env", "%q is not a conformance environment's name", g.env)
	}
	if err := g.checkNonceFiles(); err != nil {
		return err
	}
	env := g.inputs().LaunchEnv
	if env == nil {
		return refuse("launch env", "no launch environment given")
	}
	if err := g.CheckLaunchEnv(env); err != nil {
		return err
	}
	if err := Preflight(g.facts); err != nil {
		return err
	}
	return nil
}

// Verify is the full check: VerifyBase and the mount audit. It returns a
// *RefusalError naming the item that failed.
func (g *Guard) Verify(ctx context.Context) error {
	if g != nil {
		g.mu.Lock()
		g.verifies++
		g.mu.Unlock()
	}
	if err := g.VerifyBase(); err != nil {
		return err
	}
	inspect := g.inputs().Inspect
	if inspect == nil {
		return refuse("mount audit", "no way to inspect the sandbox was given")
	}
	in, err := inspect(ctx)
	if err != nil {
		return refuse("mount audit", "inspecting the sandbox: %v", err)
	}
	res, err := Audit(in, g.expect())
	if err != nil {
		return &RefusalError{Item: "mount audit", Err: err}
	}
	g.mu.Lock()
	g.audited, g.inspectedID = &res, in.ID
	g.mu.Unlock()
	return nil
}

// checkBase checks that the base directory is where it must be and nowhere it
// must not.
func (g *Guard) checkBase() error {
	fi, err := os.Lstat(g.base)
	if err != nil {
		return refuse("base", "%v", err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return refuse("base", "%s is not a plain directory", g.base)
	}
	if p, err := filepath.EvalSymlinks(g.base); err != nil || p != g.base {
		return refuse("base", "%s does not resolve to itself", g.base)
	}
	if !strings.HasPrefix(filepath.Base(g.base), BasePrefix) {
		return refuse("base", "%s is not named %s*", g.base, BasePrefix)
	}
	tmp, err := physicalTemp(g.facts.TempRoot)
	if err != nil {
		return refuse("base", "%v", err)
	}
	if !strictlyInside(g.base, tmp) {
		return refuse("base", "%s is not inside %s", g.base, tmp)
	}
	if g.facts.CabooseHome == "" || g.facts.UserHome == "" {
		return refuse("base", "the real CABOOSE_HOME and home directory were not given")
	}
	if g.facts.RealRoots == nil {
		return refuse("base", "the real environments' roots cannot be listed")
	}
	roots, err := g.facts.RealRoots()
	if err != nil {
		return refuse("base", "listing the real environments' roots: %v", err)
	}
	others := append([]string{g.facts.CabooseHome, g.facts.UserHome}, roots...)
	for _, o := range others {
		p := physical(o)
		if p == "" {
			return refuse("base", "%q is not an absolute path", o)
		}
		if overlaps(g.base, p) {
			return refuse("base", "%s overlaps %s", g.base, p)
		}
	}
	return nil
}

// checkMarker checks that the base holds a marker file of the right kind and
// that it names this run (nonce, environment, base, isolation and trust).
func (g *Guard) checkMarker() error {
	d := nofollow.Dir(g.base)
	fi, err := d.Lstat(MarkerFile)
	if err != nil {
		return refuse("marker", "%v", err)
	}
	if !fi.Mode().IsRegular() {
		return refuse("marker", "%s is not a regular file", MarkerFile)
	}
	if fi.Mode().Perm() != 0o600 {
		return refuse("marker", "%s has mode %o, not 600", MarkerFile, fi.Mode().Perm())
	}
	if uid, ok := ownerOf(fi); !ok || uid != currentUID() {
		return refuse("marker", "%s is not owned by this user", MarkerFile)
	}
	b, _, err := d.ReadFileMax(MarkerFile, 4096)
	if err != nil {
		return refuse("marker", "%v", err)
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return refuse("marker", "%s: %v", MarkerFile, err)
	}
	want := g.marker()
	switch {
	case m.Nonce != want.Nonce:
		return refuse("marker", "the nonce differs")
	case m.Env != want.Env:
		return refuse("marker", "the environment differs")
	case m.Base != want.Base:
		return refuse("marker", "the base differs")
	case m.Isolation != want.Isolation:
		return refuse("marker", "the isolation differs")
	case m.Trust != want.Trust:
		return refuse("marker", "the trust differs")
	}
	return nil
}

// checkNonceFiles checks that each file that must carry the run's nonce does.
func (g *Guard) checkNonceFiles() error {
	d := nofollow.Dir(g.base)
	files := []string{g.DataMarker()}
	for _, r := range g.RootNames() {
		files = append(files, "roots/"+r+"/"+RootMarker)
	}
	for _, f := range files {
		b, _, err := d.ReadFileMax(f, 128)
		if err != nil {
			return refuse("nonce files", "%v", err)
		}
		if strings.TrimSuffix(string(b), "\n") != g.nonce {
			return refuse("nonce files", "%s does not hold the nonce", f)
		}
	}
	return nil
}

// LaunchEnv is the environment the launcher is run with: parent without
// any CABOOSE_ variable, DOCKER_HOST or DOCKER_CONTEXT, plus the throwaway
// environment's CABOOSE_HOME and CABOOSE_ENV, and CABOOSE_NO_AUTO_UPDATE=1.
// CABOOSE_DATA_DIR is never set: the data dir is the environment's
// default, inside CABOOSE_HOME (DataDir).
func (g *Guard) LaunchEnv(parent []string) []string {
	var out []string
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if scrubbed(k) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, g.launchVars()...)
}

func scrubbed(k string) bool {
	return strings.HasPrefix(k, "CABOOSE_") || k == "DOCKER_HOST" || k == "DOCKER_CONTEXT"
}

func (g *Guard) launchVars() []string {
	return []string{
		"CABOOSE_HOME=" + g.CabooseHome(),
		"CABOOSE_ENV=" + g.env,
		"CABOOSE_NO_AUTO_UPDATE=1",
	}
}

// CheckLaunchEnv refuses an environment that is not LaunchEnv's: each of
// the three variables exactly once with its value, and no other CABOOSE_
// variable (CABOOSE_DATA_DIR above all), DOCKER_HOST or DOCKER_CONTEXT.
func (g *Guard) CheckLaunchEnv(env []string) error {
	want := map[string]bool{}
	for _, kv := range g.launchVars() {
		want[kv] = false
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !scrubbed(k) {
			continue
		}
		seen, ok := want[kv]
		if !ok || seen {
			return refuse("launch env", "the launcher's environment has %s", k)
		}
		want[kv] = true
	}
	for kv, seen := range want {
		if !seen {
			k, _, _ := strings.Cut(kv, "=")
			return refuse("launch env", "the launcher's environment lacks %s", k)
		}
	}
	return nil
}

// ExactNames are the container and image names derived from the
// environment's name: the only engine objects the suite ever removes.
func (g *Guard) ExactNames() (container string, images []string) {
	return config.ContainerFor(g.env), []string{config.ImageFor(g.env), config.BaseImageFor(g.env)}
}

// physical is p resolved through symlinks, or, for a path that does not
// exist, p cleaned; "" for a relative path.
func physical(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// strictlyInside reports whether p is below dir, both clean and absolute.
func strictlyInside(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// overlaps reports whether a and b are equal or one is inside the other.
func overlaps(a, b string) bool {
	return a == b || strictlyInside(a, b) || strictlyInside(b, a)
}

// MakeDir makes the directory rel (relative to B, slash-separated) and
// its parents, one element at a time, never following a symlink: what the
// launcher side makes of the throwaway environment's own layout.
func (g *Guard) MakeDir(rel string) error {
	if g == nil || !g.made {
		return refuse("guard", "not an environment this suite created")
	}
	return mkdirNoFollow(g.base, rel)
}
