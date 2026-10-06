// Package config resolves the launcher's settings: the CABOOSE_* variables,
// the environment's config.toml, the environment itself (CABOOSE_ENV,
// --env) and the data dir that goes with it.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Settings are the settings read from CABOOSE_ variables, by the name after
// CABOOSE_.
var Settings = []string{
	"IMAGE", "CONTAINER", "DATA_DIR", "REPO_ROOT", "READY_TIMEOUT",
	"KEEP_VERSIONS", "DOCKER_SOCK", "TZ", "SESSION", "PROJECT", "NO_TMUX",
	"NO_AUTO_BUILD", "BASE_IMAGE", "AUTO_SYNC", "DOCKER_RUN_ARGS",
	"FORWARD_PORTS", "OPEN_URLS", "ISOLATION", "VM_CPUS", "VM_MEMORY",
	"EGRESS_PROXY", "EGRESS_PORTS", "EGRESS_ALLOW", "SSH_AGENT",
	"HOST_EXEC", "HOSTNAME",
}

// Env looks up an environment variable; "" means unset or empty, which is
// all the launcher ever distinguishes.
type Env func(string) string

// FS is the slice of the filesystem config needs, so tests can fake it.
type FS interface {
	// IsDir reports whether path is a directory, like [ -d ].
	IsDir(path string) bool
	// ReadFile reads a file, as os.ReadFile.
	ReadFile(path string) ([]byte, error)
}

// OSFS is the real filesystem.
type OSFS struct{}

// IsDir implements FS.
func (OSFS) IsDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// ReadFile implements FS.
func (OSFS) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

// DefaultEnv is the environment used when none is named.
const DefaultEnv = "default"

// Config is the resolved configuration.
type Config struct {
	// Env is the environment: a whole caboose of its own -- data dir (and
	// so Claude login and state), container, image. DefaultEnv unless
	// --env or CABOOSE_ENV names another.
	Env string
	// CabooseHome is CABOOSE_HOME, default ~/.caboose: where every
	// environment lives, as envs/<name>/.
	CabooseHome string
	// EnvDir is CabooseHome/envs/<Env>.
	EnvDir string

	Image     string
	Container string
	DataDir   string
	// Roots are the host directories mounted into the container, as
	// configured; ResolveRoots makes them physical. One root (repo_root,
	// CABOOSE_REPO_ROOT, or the default) is mounted at /work itself; a
	// [roots] table in the config file mounts each at /work/<name>.
	Roots []Root
	// RootsFrom names what set Roots: CABOOSE_REPO_ROOT, or the config
	// file by its path, or "" when it is the default. Every message about
	// the roots says which, because someone who never set them has no
	// reason to know they exist.
	RootsFrom    string
	ReadyTimeout string
	// KeepVersions is the number of installed Claude Code versions to retain
	// (~224MB each). The updater never deletes the version it replaced, so
	// without this the data dir grows forever. Passed through as a string,
	// exactly as the container's entrypoint receives it.
	KeepVersions string
	DockerSock   string
	TZ           string
	Session      string
	Project      string
	NoTmux       string
	// NoAutoBuild is CABOOSE_NO_AUTO_BUILD: any non-empty value, as for
	// NoTmux, makes a missing image an error again instead of something the
	// first launch builds.
	NoAutoBuild string
	// AutoSync is CABOOSE_AUTO_SYNC: any non-empty value has a launch that
	// finds nothing running in the container sync first, when a sync
	// remote is set. Off unless asked for.
	AutoSync string
	// BaseImage is CABOOSE_BASE_IMAGE: the image to build the sandbox on,
	// "" for the embedded Dockerfile's (see Base). The image caboose builds
	// and runs is still Image.
	BaseImage string
	// ImageDir is the environment's image/ dir, when it has one: the build
	// context of its own base, with a Dockerfile that caboose setup wrote
	// and the user owns. "" when there is none, and the embedded
	// Dockerfile is built instead. It and BaseImage exclude each other
	// (CheckImages).
	ImageDir string
	// DockerRunArgs are arguments of the user's own for `docker run`, as
	// the container is created: CABOOSE_DOCKER_RUN_ARGS split at
	// whitespace, else docker_run_args in the config file, a list (or a
	// string, split as the variable is). The launcher checks them
	// (launcher/runargs.go); nil when there are none.
	DockerRunArgs []string
	// DockerRunArgsFrom names what set DockerRunArgs, for messages:
	// CABOOSE_DOCKER_RUN_ARGS, or the config file by its path.
	DockerRunArgsFrom string
	// SSHAgent is CABOOSE_SSH_AGENT: the host's SSH agent socket to give
	// the sandbox, whatever $SSH_AUTH_SOCK says ("none" for none); "" is
	// the one ssh on the host would use. A ~ in the file's is expanded.
	// SSHAgentFrom names what set it: CABOOSE_SSH_AGENT, or the file.
	SSHAgent, SSHAgentFrom string
	// ForwardPorts is CABOOSE_FORWARD_PORTS: the ports listening in the
	// sandbox that the link helper forwards to this machine's localhost,
	// as ports and ranges ("3000-3999 5173"), or "none". The launcher
	// parses it (launcher/link.go).
	ForwardPorts string
	// OpenURLs is CABOOSE_OPEN_URLS: whether the sandbox may open URLs in
	// this machine's browser, "ask", "allow" or "off".
	OpenURLs string
	// Isolation is CABOOSE_ISOLATION: what keeps the sandbox from the
	// host, "docker" (runc, the default), "gvisor" (runsc) or "vm" (a VM of
	// caboose's own, on a Mac). The launcher checks it
	// (launcher/isolation.go); never the sandbox config's, since a session
	// writes that.
	Isolation string
	// VMCPUs and VMMemory are CABOOSE_VM_CPUS and CABOOSE_VM_MEMORY: the
	// vm isolation's size, a number of CPUs and an amount of memory ("8G",
	// "4096M", or MiB), "" for the launcher's defaults (launcher/vm.go).
	// The host's keys, as the isolation is.
	VMCPUs, VMMemory string
	// Hostname is CABOOSE_HOSTNAME: the sandbox's hostname, "" for the
	// default the launcher derives from this machine's name
	// (launcher/hostname.go). Checked at load (CheckHostname).
	Hostname string
	// EgressProxy is CABOOSE_EGRESS_PROXY: "on" (the default) or "off",
	// whether under vm the sandbox's outbound connections are dialled
	// from this machine, through the link, so that its VPN routes and
	// resolver apply; ignored under docker and gvisor, whose engines dial
	// from the host already. EgressPorts (CABOOSE_EGRESS_PORTS) are the
	// ports it reaches, as forward_ports is written; EgressAllow
	// (CABOOSE_EGRESS_ALLOW) the names, *.suffix patterns and CIDRs it may
	// reach although they are private. The launcher checks them
	// (launcher/link.go, CheckEgressProxy); the host's keys, never the
	// sandbox config's.
	EgressProxy, EgressPorts, EgressAllow string
	// HostExec is CABOOSE_HOST_EXEC: whether sessions may run commands on
	// this machine, as the user, through the link ("on", "off", or ""
	// for off; CheckHostExec reads it), and HostExecFrom what set it. The
	// host's key, never the sandbox config's: it is the hole in the wall.
	HostExec, HostExecFrom string

	// Home is $HOME, as the rest of the launcher sees it.
	Home string
	// Getenv reads any other variable (TERM, TZ, SSH_AUTH_SOCK, ...).
	Getenv Env
	// File is the environment's config.toml, nil when it has none.
	File *File
}

// Base is the image the sandbox's layer is built on, and whether it is the
// user's own (CABOOSE_BASE_IMAGE) rather than one caboose builds -- from
// the environment's ImageDir, or else the embedded Dockerfile -- and tags
// DefaultBaseTag(Image).
func (c *Config) Base() (ref string, byo bool) {
	if c.BaseImage != "" {
		return c.BaseImage, true
	}
	return DefaultBaseTag(c.Image), false
}

// DefaultBaseTag is the tag the embedded Dockerfile's image is built under:
// image's repository with -base appended, its tag (if any) kept, so caboose
// gives caboose-base, caboose:dev gives caboose-base:dev, and
// ghcr.io/x/caboose:1 gives ghcr.io/x/caboose-base:1. Only the last path
// component can hold the tag -- a colon before the last slash is a
// registry's port (localhost:5000/caboose). A digest (@sha256:...) is
// dropped: nothing can be built under one, the image included.
func DefaultBaseTag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	slash := strings.LastIndex(image, "/")
	if i := strings.LastIndex(image, ":"); i > slash {
		return image[:i] + "-base" + image[i:]
	}
	return image + "-base"
}

// NormalizeImage spells an image reference the way docker resolves it, so
// two spellings of one image compare equal: a missing tag is :latest (a
// digest is left as it is), and Docker Hub's implied registry and library/
// namespace are dropped -- alpine, alpine:latest, library/alpine and
// docker.io/library/alpine:latest are all alpine:latest. Only the last path
// component can hold the tag; a colon before the last slash is a registry's
// port.
func NormalizeImage(ref string) string {
	for _, hub := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		ref = strings.TrimPrefix(ref, hub)
	}
	if rest, ok := strings.CutPrefix(ref, "library/"); ok && !strings.Contains(rest, "/") {
		ref = rest
	}
	if strings.Contains(ref, "@") {
		return ref
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref
	}
	return ref + ":latest"
}

// SameImage reports whether two references name the same image, as
// NormalizeImage spells them.
func SameImage(a, b string) bool { return NormalizeImage(a) == NormalizeImage(b) }

// CheckImages refuses a CABOOSE_BASE_IMAGE that is CABOOSE_IMAGE itself.
// The layer is built FROM the base and tagged CABOOSE_IMAGE, so the first
// build would move that tag off the base onto the layer, and every build
// after it would stack one more layer on the last one -- while the base
// looked "changed" each time, so the image was never current. Naming the
// default base's own tag (caboose-base, for CABOOSE_IMAGE=caboose) is fine:
// that is a separate image.
func (c *Config) CheckImages() error {
	if c.BaseImage != "" && c.ImageDir != "" {
		return fmt.Errorf("%w: the environment has %s, and CABOOSE_BASE_IMAGE (or base_image) names '%s'.\n"+
			"       Keep one: remove base_image from %s/%s (or unset CABOOSE_BASE_IMAGE) to build\n"+
			"       from the dir, or move the dir away to build on '%s'", ErrTwoBases, c.ImageDir, c.BaseImage, c.EnvDir, FileName, c.BaseImage)
	}
	if c.BaseImage != "" && SameImage(c.BaseImage, c.Image) {
		return fmt.Errorf("CABOOSE_BASE_IMAGE ('%s') names the same image as CABOOSE_IMAGE ('%s'): caboose builds its\n"+
			"       layer FROM the base and tags the result CABOOSE_IMAGE, so it would build over its own base,\n"+
			"       and stack another layer on every rebuild. Give the sandbox a tag of its own, e.g. unset\n"+
			"       CABOOSE_IMAGE (the default is caboose), and keep CABOOSE_BASE_IMAGE for the image to build on",
			c.BaseImage, c.Image)
	}
	return nil
}

// ErrTwoBases is CheckImages' error for an environment with both an image/
// dir and CABOOSE_BASE_IMAGE: which to build on would be a guess.
var ErrTwoBases = errors.New("two bases")

// Load resolves the configuration.
//
// The error is an unset (or empty) HOME when a default needs it: the repo
// root and the data dir both default under it, and quietly putting them at
// /dev and /.caboose instead is the last thing anyone wants.
//
// env is the --env flag's value, "" when it was not given; it wins over
// CABOOSE_ENV.
// Machine is the part of the configuration that is the machine's, not an
// environment's: HOME, CABOOSE_HOME and the variables. It is what caboose
// update runs on when the environment's cannot be loaded: updating is
// machine-wide, and the fix for a config.toml a caboose cannot read may be
// the newer caboose that wrote it.
func Machine(getenv Env) *Config {
	home := getenv("HOME")
	cabooseHome := getenv("CABOOSE_HOME")
	if cabooseHome == "" && home != "" {
		cabooseHome = home + "/.caboose"
	}
	return &Config{Env: DefaultEnv, Home: home, CabooseHome: cabooseHome, Getenv: getenv}
}

func Load(getenv Env, fsys FS, env string) (*Config, error) {
	env = or(env, or(getenv("CABOOSE_ENV"), DefaultEnv))
	if !ValidEnv(env) {
		return nil, fmt.Errorf("'%s' is not an environment name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", env)
	}
	home := getenv("HOME")
	cabooseHome := getenv("CABOOSE_HOME")
	if cabooseHome == "" && home != "" {
		cabooseHome = home + "/.caboose"
	}
	// With neither, CABOOSE_DATA_DIR is the only place anything is kept, and
	// there is no config file.
	var envDir string
	var file *File
	if cabooseHome != "" {
		envDir = EnvDirFor(cabooseHome, env)
		var err error
		if file, err = readFile(envDir+"/"+FileName, fsys); err != nil {
			return nil, err
		}
	}

	// Each setting: its CABOOSE_ variable, else the config file, else the
	// default below.
	vals, from := map[string]string{}, map[string]string{}
	for _, v := range Settings {
		if vals[v] = getenv("CABOOSE_" + v); vals[v] != "" {
			from[v] = "CABOOSE_" + v
		}
	}
	if file != nil {
		for k, v := range file.Vals {
			if vals[k] == "" {
				vals[k], from[k] = v, file.Path
			}
		}
	}
	// A path in the file may start with ~, as it would in a shell.
	for _, k := range []string{"REPO_ROOT", "DOCKER_SOCK", "SSH_AGENT"} {
		if file != nil && from[k] == file.Path {
			vals[k] = expandTilde(vals[k], home)
		}
	}

	// The file's [roots] stand in for repo_root, which it cannot also set
	// (readFile refuses both); CABOOSE_REPO_ROOT still wins over them.
	fileRoots := file != nil && len(file.Roots) > 0 && from["REPO_ROOT"] != "CABOOSE_REPO_ROOT"

	// The data dir defaults under CABOOSE_HOME, and that under HOME.
	if home == "" && (vals["REPO_ROOT"] == "" && !fileRoots || vals["DATA_DIR"] == "" && getenv("CABOOSE_HOME") == "") {
		return nil, fmt.Errorf("HOME is not set (needed for the default %s)", homeDefaults(vals, fileRoots, getenv))
	}
	roots, rootsFrom := []Root{{Host: or(vals["REPO_ROOT"], home+"/"+DefaultRepoRoot), Container: WorkDir}}, from["REPO_ROOT"]
	if fileRoots {
		roots, rootsFrom = nil, file.Path
		for _, name := range sortedKeys(file.Roots) {
			roots = append(roots, Root{Name: name, Host: expandTilde(file.Roots[name], home), Container: WorkDir + "/" + name})
		}
	}
	// The default environment's container and image are plain caboose;
	// another's are caboose-<env> (ContainerFor).
	name := ContainerFor(env)
	c := &Config{
		Env:          env,
		CabooseHome:  cabooseHome,
		EnvDir:       envDir,
		File:         file,
		Image:        or(vals["IMAGE"], name),
		Container:    or(vals["CONTAINER"], name),
		Roots:        roots,
		RootsFrom:    rootsFrom,
		ReadyTimeout: or(vals["READY_TIMEOUT"], "600"),
		KeepVersions: or(vals["KEEP_VERSIONS"], "2"),
		DockerSock:   vals["DOCKER_SOCK"],
		TZ:           vals["TZ"],
		Session:      vals["SESSION"],
		Project:      vals["PROJECT"],
		NoTmux:       vals["NO_TMUX"],
		NoAutoBuild:  vals["NO_AUTO_BUILD"],
		AutoSync:     vals["AUTO_SYNC"],
		BaseImage:    vals["BASE_IMAGE"],
		ForwardPorts: or(vals["FORWARD_PORTS"], DefaultForwardPorts),
		OpenURLs:     or(vals["OPEN_URLS"], "ask"),
		Isolation:    or(vals["ISOLATION"], "docker"),
		VMCPUs:       vals["VM_CPUS"],
		VMMemory:     vals["VM_MEMORY"],
		Hostname:     vals["HOSTNAME"],
		EgressProxy:  or(vals["EGRESS_PROXY"], "on"),
		EgressPorts:  or(vals["EGRESS_PORTS"], DefaultEgressPorts),
		EgressAllow:  vals["EGRESS_ALLOW"],
		Home:         home,
		Getenv:       getenv,
	}
	if err := CheckHostname(c.Hostname); err != nil {
		if f := from["HOSTNAME"]; f == "CABOOSE_HOSTNAME" {
			return nil, fmt.Errorf("%v (CABOOSE_HOSTNAME)", err)
		} else if f != "" {
			return nil, fmt.Errorf("%v (in %s)", err, f)
		}
	}
	c.DataDir = c.resolveDataDir(vals["DATA_DIR"])
	c.DockerRunArgs, c.DockerRunArgsFrom = strings.Fields(vals["DOCKER_RUN_ARGS"]), from["DOCKER_RUN_ARGS"]
	c.SSHAgent, c.SSHAgentFrom = vals["SSH_AGENT"], from["SSH_AGENT"]
	c.HostExec, c.HostExecFrom = vals["HOST_EXEC"], from["HOST_EXEC"]
	if file != nil && file.RunArgs != nil && c.DockerRunArgsFrom == "" {
		c.DockerRunArgs, c.DockerRunArgsFrom = file.RunArgs, file.Path
	}
	if len(c.DockerRunArgs) == 0 {
		c.DockerRunArgs, c.DockerRunArgsFrom = nil, ""
	}
	if envDir != "" && fsys.IsDir(envDir+"/"+ImageDirName) {
		c.ImageDir = envDir + "/" + ImageDirName
	}
	return c, nil
}

var envName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidEnv reports whether name can be an environment's: it becomes a
// directory name and part of a container's and an image's.
func ValidEnv(name string) bool { return envName.MatchString(name) }

// DefaultForwardPorts are the ports forwarded when forward_ports is not
// set: the ranges dev servers tend to use.
const DefaultForwardPorts = "3000-3999 5173 8000-8999"

// DefaultEgressPorts are the ports the outbound proxy reaches when
// egress_ports is not set: ssh (git), http and https.
const DefaultEgressPorts = "22 80 443"

// CheckHostname reads hostname: "" is unset, anything else must be one
// DNS label, which is what a hostname is to docker and to the guest.
func CheckHostname(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 63 || !hostnameLabel.MatchString(v) {
		return fmt.Errorf("hostname: %q is not a hostname: 1 to 63 lowercase letters, digits and -, not starting or ending with -", v)
	}
	return nil
}

var hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// CheckEgressProxy reads egress_proxy: whether it is on, or why the value
// is neither "on" nor "off".
func CheckEgressProxy(v string) (bool, error) {
	switch v {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fmt.Errorf(`egress_proxy: %q is not "on" or "off"`, v)
}

// CheckHostExec reads host_exec: whether it is on, or why the value is
// none it takes. "" is off, the default. A variable's "0" or "false" is
// off too, never set-and-so-on as some of the others read.
func CheckHostExec(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "1", "yes":
		return true, nil
	case "", "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf(`host_exec: %q is not true or false ("on" or "off")`, v)
}

// ImageDirName is an environment's own image's build context, in its EnvDir.
const ImageDirName = "image"

// EnvDirFor is where environment name lives under cabooseHome.
func EnvDirFor(cabooseHome, name string) string { return cabooseHome + "/envs/" + name }

// ContainerFor is environment name's container (and image) name, when
// CABOOSE_CONTAINER (CABOOSE_IMAGE) does not set it: caboose for the
// default one, as it always was, caboose-<name> for the others.
func ContainerFor(name string) string {
	if name == DefaultEnv {
		return "caboose"
	}
	return "caboose-" + name
}

// CheckEnv refuses an environment other than the default that has not been
// created: a mistyped --env would otherwise quietly start a new, empty
// sandbox -- a container, an image build, a Claude login -- under the typo.
func (c *Config) CheckEnv(fsys FS) error {
	if c.Env == DefaultEnv || c.EnvDir != "" && fsys.IsDir(c.EnvDir) {
		return nil
	}
	return fmt.Errorf("there is no environment '%s' (no %s); create it with 'caboose -e %s setup'", c.Env, c.EnvDir, c.Env)
}

// ExpandTilde expands a leading ~ in a path, as a shell would, and as the
// config file's paths are read; with no home it is left alone.
func ExpandTilde(p, home string) string { return expandTilde(p, home) }

// expandTilde expands a leading ~ in a path from the config file, as a
// shell would; with no HOME it is left alone.
func expandTilde(p, home string) string {
	if home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		return home + strings.TrimPrefix(p, "~")
	}
	return p
}

// homeDefaults names the settings whose defaults need HOME, for the error.
func homeDefaults(vals map[string]string, fileRoots bool, getenv Env) string {
	var need []string
	if vals["DATA_DIR"] == "" && getenv("CABOOSE_HOME") == "" {
		need = append(need, "CABOOSE_DATA_DIR")
	}
	if vals["REPO_ROOT"] == "" && !fileRoots {
		need = append(need, "CABOOSE_REPO_ROOT")
	}
	return strings.Join(need, " and ")
}

// resolveDataDir picks the data dir: CABOOSE_DATA_DIR when set, else the
// environment's EnvDir/data. It is OUTSIDE any checkout on purpose: inside
// one, the live OAuth credential and every session transcript would sit in a
// working tree guarded only by a tracked .gitignore, which a history rewrite
// can drop, letting the VCS take the state and then delete it.
func (c *Config) resolveDataDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return c.EnvDir + "/data"
}

// DefaultRepoRoot is the repo root used when neither CABOOSE_REPO_ROOT nor
// the config file sets one, relative to $HOME: a common place to keep
// repos, and nothing more principled than that. No default fits every
// layout, so a missing one is an error that explains the setting rather than
// a guess.
const DefaultRepoRoot = "dev"

// WorkDir is where the roots are mounted in the container: a single root at
// WorkDir itself, each of several at WorkDir/<name>. The path is the same on
// every machine, whatever the user name or where the projects live on the
// host, so the project keys Claude Code derives from a cwd are too -- which
// is what lets its per-project state sync unchanged.
const WorkDir = "/work"

// Root is a host directory mounted into the container.
type Root struct {
	// Name is the root's name in [roots], "" for a single root.
	Name string
	// Host is the host path: as configured until ResolveRoots makes it
	// physical, or as docker reports the source of a mount.
	Host string
	// Container is where the container sees it.
	Container string
}

// ContainerPath is where the container sees host path p, a physical path,
// under whichever of roots holds it; false when none does.
func ContainerPath(roots []Root, p string) (string, bool) {
	for _, r := range roots {
		if !Within(p, r.Host) {
			continue
		}
		rel := p
		if r.Host != "/" {
			rel = strings.TrimPrefix(p, r.Host)
		}
		if rel == "/" {
			rel = ""
		}
		return r.Container + rel, true
	}
	return "", false
}

// HostPath is ContainerPath the other way: where this machine has
// container path p, under whichever of roots the container mounts it
// from (the deepest, were one inside another); false when p is relative
// or under none. p is cleaned first, so a ".." cannot climb out of a
// root; symlinks are not looked at, here or on the host.
func HostPath(roots []Root, p string) (string, bool) {
	if !path.IsAbs(p) {
		return "", false
	}
	p = path.Clean(p)
	best, found := -1, ""
	for _, r := range roots {
		if r.Container == "" || !Within(p, r.Container) || len(r.Container) <= best {
			continue
		}
		rest := p
		if r.Container != "/" {
			rest = strings.TrimPrefix(p, r.Container)
		}
		best, found = len(r.Container), path.Join(r.Host, rest)
	}
	return found, best >= 0
}

// SameRoots reports whether a and b mount the same host paths at the same
// container paths, in any order.
func SameRoots(a, b []Root) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[[2]string]bool{}
	for _, r := range a {
		m[[2]string{r.Host, r.Container}] = true
	}
	for _, r := range b {
		if !m[[2]string{r.Host, r.Container}] {
			return false
		}
	}
	return true
}

// DescribeRoots is roots for a message: "/h/dev" for one mounted at
// WorkDir, else "/h/dev at /work/dev, /h/w at /work/w".
func DescribeRoots(roots []Root) string {
	if len(roots) == 1 && roots[0].Container == WorkDir {
		return roots[0].Host
	}
	var parts []string
	for _, r := range roots {
		parts = append(parts, r.Host+" at "+r.Container)
	}
	return strings.Join(parts, ", ")
}

// RootsOrigin says where Roots came from, for messages: the variable or the
// file that set them, or that they are the default because nothing did.
func (c *Config) RootsOrigin() string {
	switch {
	case c.File != nil && c.RootsFrom == c.File.Path && c.Roots[0].Name != "":
		return "set by [roots] in " + c.File.Path
	case c.File != nil && c.RootsFrom == c.File.Path:
		return "set by repo_root in " + c.File.Path
	case c.RootsFrom == "":
		return "the default: CABOOSE_REPO_ROOT is unset"
	}
	return "set by CABOOSE_REPO_ROOT"
}

// RepoRootHelp is the explanation every repo-root error ends with, indented
// to follow a "caboose: " first line. The repo root is the one setting a new
// user trips over on the first run -- its default is one person's layout --
// so the message has to say what it is and how to change it, not just that
// it is wrong.
const RepoRootHelp = `       The repo root is the directory tree mounted into the container, at
       /work; every project you run caboose in has to be under it. Point it
       at the directory that holds your projects, e.g. in your shell profile:
           export CABOOSE_REPO_ROOT=/path/holding/your/projects
       or as repo_root in the environment's config.toml, where a [roots]
       table can instead name several, each mounted at /work/<name>.
       A container that already exists keeps the roots it was created with;
       'caboose restart' remounts them (this kills running sessions).`

// ResolveRoots checks every root is a directory, makes each absolute and
// physical (symlinks resolved), as `cd "$root" && pwd -P` does, and refuses
// roots that overlap: a project under two of them would have two container
// paths, and so two sets of Claude Code state.
func (c *Config) ResolveRoots() error {
	for i := range c.Roots {
		r := &c.Roots[i]
		fi, err := os.Stat(r.Host)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return c.rootError(*r, "does not exist")
		case err != nil:
			return c.rootError(*r, err.Error())
		case !fi.IsDir():
			return c.rootError(*r, "is not a directory")
		}
		p, err := Physical(r.Host)
		if err != nil {
			return c.rootError(*r, err.Error())
		}
		r.Host = p
	}
	for i, a := range c.Roots {
		for _, b := range c.Roots[i+1:] {
			if Within(a.Host, b.Host) || Within(b.Host, a.Host) {
				return fmt.Errorf("roots %s (%s) and %s (%s) overlap (%s): a project under both would have two\n"+
					"       paths in the container. Name directories that do not contain one another.",
					a.Name, a.Host, b.Name, b.Host, c.RootsOrigin())
			}
		}
	}
	return nil
}

func (c *Config) rootError(r Root, problem string) error {
	what := "repo root " + r.Host
	if r.Name != "" {
		what = "root " + r.Name + ", " + r.Host + ","
	}
	return fmt.Errorf("%s %s (%s).\n%s", what, problem, c.RootsOrigin(), RepoRootHelp)
}

// ReadyTimeoutSeconds parses CABOOSE_READY_TIMEOUT.
func (c *Config) ReadyTimeoutSeconds() (int, error) {
	n, err := strconv.Atoi(c.ReadyTimeout)
	if err != nil {
		return 0, fmt.Errorf("CABOOSE_READY_TIMEOUT must be a whole number of seconds, not '%s'", c.ReadyTimeout)
	}
	return n, nil
}

// Physical returns p made absolute with every symlink resolved: the
// equivalent of `cd p && pwd -P`.
func Physical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Within reports whether path is root or below it. Both must be clean and
// absolute. A root of "/" holds everything, which the obvious prefix test on
// root+"/" gets wrong.
func Within(path, root string) bool {
	return path == root || root == "/" || strings.HasPrefix(path, root+"/")
}

// Cwd is `pwd -P`.
func Cwd() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(wd)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
