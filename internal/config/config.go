// Package config resolves the launcher's settings: the environment
// (CABOOSE_ENV, --env), its config.toml, the data dir that goes with it,
// and the few CABOOSE_ variables that are not an environment's settings.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/bfreis/caboose/internal/apkobuild/pkgset"
)

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

// Config is the resolved configuration: the environment's config.toml, its
// defaults, and the few variables that are not settings of an environment.
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

	// Image and Container are named after the environment (ImageFor,
	// ContainerFor), never configured.
	Image     string
	Container string
	DataDir   string
	// Roots are the host directories mounted into the container, as
	// configured; ResolveRoots makes them physical. Each is mounted at
	// /work/<name> unless its long form names another path.
	Roots []Root
	// RootsFrom is the config file's path when its [roots] set Roots, ""
	// when they are the default. Every message about the roots says which,
	// because someone who never set them has no reason to know they exist.
	RootsFrom string

	// ImageProfile is the image profile in use: what the sandbox's base is
	// built from (selectImage).
	ImageProfile ImageProfile
	// AutoBuild is [build] auto_build: a missing image is built, and a
	// stale one rebuilt, when the container is created. False makes either
	// an error that says to run caboose build.
	AutoBuild bool

	// Tmux is [session] tmux: sessions run in tmux, which detaches them.
	Tmux bool
	// TZ is [session] tz, "" for this machine's zone.
	TZ string
	// Hostname is [session] hostname: the sandbox's hostname, "" for the
	// default the launcher derives from this machine's name
	// (launcher/hostname.go). Checked at load (CheckHostname).
	Hostname string
	// AutoSync is [session] auto_sync: a launch that finds nothing running
	// in the container syncs first, when a sync remote is set.
	AutoSync bool
	// KeepVersions is [session] keep_versions: the number of installed
	// Claude Code versions to retain (~224MB each). The updater never
	// deletes the version it replaced, so without this the data dir grows
	// forever.
	KeepVersions int
	// ReadyTimeout is [session] ready_timeout, in seconds.
	ReadyTimeout int

	// ForwardPorts is [link] forward_ports: the ports listening in the
	// sandbox that the link forwards to this machine's localhost, written
	// as hostlink.ParsePorts reads them ("3000-3999 5173"), "none" for an
	// empty array.
	ForwardPorts string
	// OpenURLs is [link] open_urls: whether the sandbox may open URLs in
	// this machine's browser, "ask", "allow" or "off".
	OpenURLs string
	// SSHAgent is [link] ssh_agent: the host's SSH agent socket to give the
	// sandbox, whatever $SSH_AUTH_SOCK says ("none" for none); "" is the
	// one ssh on the host would use. A ~ is expanded.
	SSHAgent string
	// HostExec is [link] host_exec: whether sessions may run commands on
	// this machine, as the user, through the link. The host's key, never
	// the sandbox config's: it is the hole in the wall.
	HostExec bool

	// Isolation is the kind of the profile in use (KindContainer,
	// KindGVisor, KindVM), and Profile its "<kind>.<name>", "" when no
	// profile is defined and the default, a plain container, is used. The
	// host's alone, never the sandbox config's, since a session writes
	// that.
	Isolation, Profile string
	// RunArgs and EngineSocket are a container or gvisor profile's
	// run_args and engine_socket: arguments of the user's own for docker
	// run (checked by launcher/runargs.go), and whether the engine's
	// socket is mounted.
	RunArgs      []string
	EngineSocket bool
	// VMCPUs and VMMemory are a vm profile's cpus and memory ("8G",
	// "4096M", or MiB), 0 and "" for the launcher's defaults
	// (launcher/vm.go).
	VMCPUs   int
	VMMemory string
	// Egress is a vm profile's egress: whether the sandbox's outbound
	// connections are dialled from this machine, through the link, so that
	// its VPN routes and resolver apply. EgressPorts are the ports it
	// reaches, as ForwardPorts is written; EgressAllow the names,
	// *.suffix patterns and CIDRs it may reach although they are private.
	// Under container and gvisor they are the defaults, and unused.
	Egress      bool
	EgressPorts string
	EgressAllow []string

	// Session is CABOOSE_SESSION, the tmux session to name outright.
	Session string
	// Force is CABOOSE_FORCE: what would ask before ending sessions or
	// deleting goes on without asking.
	Force bool
	// NoAutoUpdate is CABOOSE_NO_AUTO_UPDATE: caboose does not update
	// itself. Machine-wide, so not config.toml's.
	NoAutoUpdate bool

	// Home is $HOME, as the rest of the launcher sees it.
	Home string
	// Getenv reads any other variable (TERM, TZ, SSH_AUTH_SOCK, ...).
	Getenv Env
	// File is the environment's config.toml, nil when it has none.
	File *File
}

// ImageProfile is an image profile, resolved: what the sandbox's base is
// built from. Kind is an image kind (ImageKindApko, ImageKindDockerfile,
// ImageKindRef) and Name the profile's name; the rest are its kind's keys.
type ImageProfile struct {
	Kind, Name string
	// Packages and Defaults are an apko profile's packages and defaults:
	// the user's own packages, and whether caboose's default groups come
	// with them.
	Packages []string
	Defaults bool
	// Dir is a dockerfile profile's dir, absolute: the build context, with
	// its Dockerfile, always DockerfileDir(EnvDir, Name).
	Dir string
	// Ref is a ref profile's image.
	Ref string
}

// String is the profile as config.toml names it: "apko.default".
func (p ImageProfile) String() string { return p.Kind + "." + p.Name }

// Spec is an apko profile's package set.
func (p ImageProfile) Spec() pkgset.Spec {
	return pkgset.Spec{Packages: p.Packages, Defaults: p.Defaults}
}

// DefaultImageProfile is the image profile used when config.toml defines
// none: caboose's packages, built with apko.
func DefaultImageProfile() ImageProfile {
	return ImageProfile{Kind: ImageKindApko, Name: "default", Defaults: true}
}

// BaseRef is the image the sandbox's layer is built on: a ref profile's
// image, or else the base caboose builds and tags BaseImageFor(Env).
func (c *Config) BaseRef() string {
	if c.ImageProfile.Kind == ImageKindRef {
		return c.ImageProfile.Ref
	}
	return BaseImageFor(c.Env)
}

// LockPath is where an apko image profile's lock is kept: next to
// config.toml, as apko-<name>.lock.json. "" for any other kind, or with no
// EnvDir.
func (c *Config) LockPath() string {
	if c.ImageProfile.Kind != ImageKindApko || c.EnvDir == "" {
		return ""
	}
	return c.EnvDir + "/apko-" + c.ImageProfile.Name + ".lock.json"
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

// CheckImages refuses a ref profile whose image is the environment's image
// itself. The layer is built FROM the base and tagged Image, so the first
// build would move that tag off the base onto the layer, and every build
// after it would stack one more layer on the last one -- while the base
// looked "changed" each time, so the image was never current.
func (c *Config) CheckImages() error {
	p := c.ImageProfile
	if p.Kind == ImageKindRef && SameImage(p.Ref, c.Image) {
		return fmt.Errorf("image in [%s] ('%s') is the environment's own image: caboose builds its layer\n"+
			"       FROM the base and tags the result '%s', so it would build over its own base, and stack\n"+
			"       another layer on every rebuild. Name the image to build on instead",
			p, p.Ref, c.Image)
	}
	return nil
}

// Machine is the part of the configuration that is the machine's, not an
// environment's: HOME, CABOOSE_HOME and the variables. It is what caboose
// update runs on when the environment's cannot be loaded: updating is
// machine-wide, and the fix for a config.toml a caboose cannot read may be
// the newer caboose that wrote it.
func Machine(getenv Env) (*Config, error) {
	home := getenv("HOME")
	cabooseHome := getenv("CABOOSE_HOME")
	if cabooseHome == "" && home != "" {
		cabooseHome = home + "/.caboose"
	}
	c := &Config{Env: DefaultEnv, Home: home, CabooseHome: cabooseHome, Getenv: getenv}
	if err := c.readVariables(); err != nil {
		return nil, err
	}
	return c, nil
}

// readVariables reads the variables that are not an environment's
// settings.
func (c *Config) readVariables() error {
	var err error
	c.Session = c.Getenv("CABOOSE_SESSION")
	if c.Force, err = EnvBool(c.Getenv, "CABOOSE_FORCE"); err != nil {
		return err
	}
	c.NoAutoUpdate, err = EnvBool(c.Getenv, "CABOOSE_NO_AUTO_UPDATE")
	return err
}

// EnvBool reads variable name as a boolean: true, 1, yes or on, or
// false, 0, no or off, in any case; unset or empty is false. Anything else
// is an error naming the variable.
func EnvBool(getenv Env, name string) (bool, error) {
	switch strings.ToLower(getenv(name)) {
	case "", "false", "0", "no", "off":
		return false, nil
	case "true", "1", "yes", "on":
		return true, nil
	}
	return false, fmt.Errorf("%s=%q is not a boolean: set it to true, 1, yes or on, or false, 0, no or off", name, getenv(name))
}

// Load resolves the configuration.
//
// The error is an unset (or empty) HOME when a default needs it: the roots
// and the data dir both default under it, and quietly putting them at
// /dev and /.caboose instead is the last thing anyone wants.
//
// env is the --env flag's value, "" when it was not given; it wins over
// CABOOSE_ENV.
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
	c := &Config{
		Env:          env,
		CabooseHome:  cabooseHome,
		Image:        ImageFor(env),
		Container:    ContainerFor(env),
		AutoBuild:    true,
		Tmux:         true,
		KeepVersions: 2,
		ReadyTimeout: 600,
		ForwardPorts: DefaultForwardPorts,
		OpenURLs:     "ask",
		Isolation:    KindContainer,
		ImageProfile: DefaultImageProfile(),
		Egress:       true,
		EgressPorts:  DefaultEgressPorts,
		Home:         home,
		Getenv:       getenv,
	}
	if err := c.readVariables(); err != nil {
		return nil, err
	}
	// With neither, CABOOSE_DATA_DIR is the only place anything is kept, and
	// there is no config file.
	if cabooseHome != "" {
		c.EnvDir = EnvDirFor(cabooseHome, env)
		var err error
		if c.File, err = readFile(c.EnvDir+"/"+FileName, fsys); err != nil {
			return nil, err
		}
	}
	dataDir := getenv("CABOOSE_DATA_DIR")
	fileRoots := c.File != nil && c.File.Roots != nil
	// The data dir defaults under CABOOSE_HOME, and that under HOME.
	if home == "" && (!fileRoots || dataDir == "" && getenv("CABOOSE_HOME") == "") {
		var need []string
		if dataDir == "" && getenv("CABOOSE_HOME") == "" {
			need = append(need, "CABOOSE_DATA_DIR")
		}
		if !fileRoots {
			need = append(need, "the roots")
		}
		return nil, fmt.Errorf("HOME is not set (needed for the default %s)", strings.Join(need, " and "))
	}
	c.DataDir = or(dataDir, c.EnvDir+"/data")
	if c.File != nil {
		if err := c.readFile(); err != nil {
			return nil, err
		}
	}
	if c.Roots == nil {
		c.Roots = []Root{{Name: DefaultRootName, Host: home + "/" + DefaultRootName, Container: WorkDir + "/" + DefaultRootName}}
	}
	return c, nil
}

// readFile takes c's settings from its config file, over the defaults.
func (c *Config) readFile() error {
	f := c.File
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{f.Path}, args...)...)
	}
	str := func(key string, dst *string) {
		if v, ok := f.Vals[key].(string); ok {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := f.Vals[key].(bool); ok {
			*dst = v
		}
	}
	positive := func(key string, dst *int) error {
		v, ok := f.Vals[key].(int64)
		if !ok {
			return nil
		}
		if v < 1 || v > 1<<31-1 {
			return fail("%s must be a whole number, 1 or more", displayKey(key))
		}
		*dst = int(v)
		return nil
	}

	boolean("build.auto_build", &c.AutoBuild)
	boolean("session.tmux", &c.Tmux)
	str("session.tz", &c.TZ)
	str("session.hostname", &c.Hostname)
	if err := CheckHostname(c.Hostname); err != nil {
		return fail("%v", err)
	}
	boolean("session.auto_sync", &c.AutoSync)
	if err := positive("session.keep_versions", &c.KeepVersions); err != nil {
		return err
	}
	if err := positive("session.ready_timeout", &c.ReadyTimeout); err != nil {
		return err
	}
	if v, ok := f.Vals["link.forward_ports"]; ok {
		c.ForwardPorts = portsString(v)
	}
	str("link.open_urls", &c.OpenURLs)
	switch c.OpenURLs {
	case "ask", "allow", "off":
	default:
		return fail(`open_urls in [link]: %q is not "ask", "allow" or "off"`, c.OpenURLs)
	}
	str("link.ssh_agent", &c.SSHAgent)
	c.SSHAgent = expandTilde(c.SSHAgent, c.Home)
	boolean("link.host_exec", &c.HostExec)

	if err := c.selectProfile(); err != nil {
		return fail("%v", err)
	}
	if p := c.Profile; p != "" {
		if v, ok := f.Vals[p+".run_args"].([]any); ok {
			for _, a := range v {
				c.RunArgs = append(c.RunArgs, a.(string))
			}
		}
		boolean(p+".engine_socket", &c.EngineSocket)
		if err := positive(p+".cpus", &c.VMCPUs); err != nil {
			return err
		}
		str(p+".memory", &c.VMMemory)
		boolean(p+".egress", &c.Egress)
		if v, ok := f.Vals[p+".egress_ports"]; ok {
			c.EgressPorts = portsString(v)
		}
		if v, ok := f.Vals[p+".egress_allow"].([]any); ok {
			for _, a := range v {
				c.EgressAllow = append(c.EgressAllow, a.(string))
			}
		}
	}

	if err := c.selectImage(); err != nil {
		return fail("%v", err)
	}

	if f.Roots != nil {
		roots, err := FileRoots(f.Roots, c.Home)
		if err != nil {
			return fail("%v", err)
		}
		c.Roots, c.RootsFrom = roots, f.Path
	}
	return nil
}

// selectProfile picks the isolation profile: the one isolation names;
// else the only one the file defines; else, with none, a plain container
// at its defaults. Several and no isolation is an error, not a guess.
func (c *Config) selectProfile() error {
	f := c.File
	name, set := f.Vals["isolation"].(string)
	if !set {
		switch len(f.Profiles) {
		case 0:
			return nil
		case 1:
			name = f.Profiles[0]
		default:
			return fmt.Errorf("it defines the isolation profiles %s, and isolation does not say which to use: set isolation = %q, say",
				strings.Join(f.Profiles, ", "), f.Profiles[0])
		}
	}
	kind, _, ok := strings.Cut(name, ".")
	if !ok || !slices.Contains(Kinds, kind) {
		hint := ""
		if name == "docker" {
			hint = ` ("docker" is the container kind now)`
		}
		return fmt.Errorf("isolation = %q is not a profile, \"<kind>.<name>\" of a kind %s%s: e.g. %q, with a [%s] table",
			name, strings.Join(Kinds, ", "), hint, KindGVisor+".default", KindGVisor+".default")
	}
	if !slices.Contains(f.Profiles, name) {
		defined := "none is"
		if len(f.Profiles) > 0 {
			defined = strings.Join(f.Profiles, ", ") + " are"
		}
		return fmt.Errorf("isolation = %q names a profile the file does not define (%s): add a [%s] table, or name another", name, defined, name)
	}
	c.Isolation, c.Profile = kind, name
	return nil
}

// ReadImage resolves the image profile again from File, as Load does: for
// a caller that has just reread File after changing it.
func (c *Config) ReadImage() error {
	c.ImageProfile = DefaultImageProfile()
	if c.File == nil {
		return nil
	}
	if err := c.selectImage(); err != nil {
		return fmt.Errorf("%s: %v", c.File.Path, err)
	}
	return nil
}

// selectImage picks the image profile, as selectProfile does the isolation
// profile: the one image names; else the only one the file defines; else,
// with none, apko.default at its defaults, which image may also name
// without a table. Several and no image is an error, not a guess.
func (c *Config) selectImage() error {
	f := c.File
	defined := f.ImageProfiles
	name, set := f.Vals["image"].(string)
	if !set {
		switch len(defined) {
		case 0:
			return nil
		case 1:
			name = defined[0]
		default:
			return fmt.Errorf("it defines the image profiles %s, and image does not say which to use: set image = %q, say",
				strings.Join(defined, ", "), defined[0])
		}
	}
	kind, pname, ok := strings.Cut(name, ".")
	if !ok || !slices.Contains(ImageKinds, kind) || !ValidProfileName(pname) {
		return fmt.Errorf("image = %q is not an image profile, \"<kind>.<name>\" of a kind %s: e.g. %q, with a [%s] table",
			name, strings.Join(ImageKinds, ", "), ImageKindDockerfile+".default", ImageKindDockerfile+".default")
	}
	p := ImageProfile{Kind: kind, Name: pname}
	if !slices.Contains(defined, name) {
		if p.String() != DefaultImageProfile().String() {
			what := "none is"
			if len(defined) > 0 {
				what = strings.Join(defined, ", ") + " are"
			}
			return fmt.Errorf("image = %q names a profile the file does not define (%s): add a [%s] table, or name another", name, what, name)
		}
	}
	switch kind {
	case ImageKindApko:
		ap, err := f.ApkoProfile(pname)
		if err != nil {
			return err
		}
		p = ap
	case ImageKindDockerfile:
		if c.EnvDir == "" {
			return fmt.Errorf("[%s] is built from %s/%s in the environment's directory, and there is none", name, DockerfileDirName, pname)
		}
		p.Dir = DockerfileDir(c.EnvDir, pname)
	case ImageKindRef:
		ref, _ := f.Vals[name+".image"].(string)
		if ref == "" {
			return fmt.Errorf("[%s] needs image, the image to build on: image = \"debian:13.7-slim\", say", name)
		}
		p.Ref = ref
	}
	c.ImageProfile = p
	return nil
}

// ApkoProfile is apko profile name ("default" for [apko.default]) as the
// file defines it, whether or not image selects it: its packages, and
// defaults, true unless it says otherwise. A profile the file does not
// define, or a nil file, is caboose's packages alone.
func (f *File) ApkoProfile(name string) (ImageProfile, error) {
	p := ImageProfile{Kind: ImageKindApko, Name: name, Defaults: true}
	if f == nil {
		return p, nil
	}
	key := p.String()
	if v, ok := f.Vals[key+".defaults"].(bool); ok {
		p.Defaults = v
	}
	if v, ok := f.Vals[key+".packages"].([]any); ok {
		for _, e := range v {
			pkg := e.(string)
			if err := pkgset.CheckName(pkg); err != nil {
				return p, fmt.Errorf("packages in [%s]: %v", key, err)
			}
			p.Packages = append(p.Packages, pkg)
		}
	}
	return p, nil
}

// portsString is a ports array as hostlink.ParsePorts reads it: the
// elements joined by spaces, "none" for none.
func portsString(v any) string {
	list, _ := v.([]any)
	if len(list) == 0 {
		return "none"
	}
	parts := make([]string, len(list))
	for i, e := range list {
		parts[i] = fmt.Sprint(e)
	}
	return strings.Join(parts, " ")
}

// ProfileKey is key of the profile in use, as config.toml spells it there,
// for messages: "run_args in [gvisor.default]".
func (c *Config) ProfileKey(key string) string {
	if c.Profile == "" {
		return key + " in a [" + c.Isolation + ".NAME] profile"
	}
	return key + " in [" + c.Profile + "]"
}

// Origin says where key, a dotted name ("link.host_exec"), came from, for
// messages: "host_exec in [link] of /path/config.toml", or "the default".
func (c *Config) Origin(key string) string {
	if c.File.Has(key) {
		return displayKey(key) + " of " + c.File.Path
	}
	return "the default"
}

var envName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidEnv reports whether name can be an environment's: it becomes a
// directory name, part of a container's name and an image's tag, all of
// which the rule fits.
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
		return fmt.Errorf("hostname in [session]: %q is not a hostname: 1 to 63 lowercase letters, digits and -, not starting or ending with -", v)
	}
	return nil
}

var hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// DockerfileDirName is the directory of an environment's EnvDir that holds
// its dockerfile image profiles' build contexts, one per profile:
// dockerfile/<name>.
const DockerfileDirName = "dockerfile"

// DockerfileDir is dockerfile profile name's build context in envDir, with
// its Dockerfile: fixed, never configured, since a dir of the user's
// choosing could be one the sandbox can write.
func DockerfileDir(envDir, name string) string {
	return filepath.Join(envDir, DockerfileDirName, name)
}

// EnvDirFor is where environment name lives under cabooseHome.
func EnvDirFor(cabooseHome, name string) string { return cabooseHome + "/envs/" + name }

// ContainerFor is environment name's container: caboose-<name>, the
// default environment's too.
func ContainerFor(name string) string { return "caboose-" + name }

// ImageFor is environment name's image, the layer the container runs:
// caboose:<name>.
func ImageFor(name string) string { return "caboose:" + name }

// BaseImageFor is the base caboose builds for environment name, from an
// apko or a dockerfile image profile: caboose-base:<name>.
func BaseImageFor(name string) string { return "caboose-base:" + name }

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

func expandTilde(p, home string) string {
	if home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		return home + strings.TrimPrefix(p, "~")
	}
	return p
}

// DefaultRootName is the root used when the config file has no [roots]:
// ~/dev, at /work/dev. A common place to keep repos, and nothing more
// principled than that; a missing one is an error that explains the
// setting rather than a guess.
const DefaultRootName = "dev"

// WorkDir is where the roots are mounted in the container by default, each
// at WorkDir/<name>. The path is the same on every machine, whatever the
// user name or where the projects live on the host, so the project keys
// Claude Code derives from a cwd are too -- which is what lets its
// per-project state sync unchanged.
const WorkDir = "/work"

// Root is a host directory mounted into the container.
type Root struct {
	// Name is the root's name in [roots]; "" for a root read off a
	// container's mounts.
	Name string
	// Host is the host path: as configured until ResolveRoots makes it
	// physical, or as the backend reports the source of a mount.
	Host string
	// Container is where the container sees it.
	Container string
}

// FileRoots are [roots] as written, checked: names sorted, each host path
// absolute (after ~, from home), each container path one
// CheckContainerPath allows, and none at, inside or around another's. A
// root at WorkDir itself is allowed only alone, since every other root's
// default path is inside it.
func FileRoots(file map[string]FileRoot, home string) ([]Root, error) {
	var roots []Root
	for _, name := range slices.Sorted(maps.Keys(file)) {
		fr := file[name]
		host := expandTilde(fr.Host, home)
		if !path.IsAbs(host) {
			return nil, fmt.Errorf("root %s: %s is not an absolute path (or one starting with ~/)", name, fr.Host)
		}
		if err := badPathChar(host, ":"); err != nil {
			return nil, fmt.Errorf("root %s: host path %q %v", name, fr.Host, err)
		}
		ctr := or(fr.Path, WorkDir+"/"+name)
		if err := CheckContainerPath(ctr); err != nil {
			return nil, fmt.Errorf("root %s: %v", name, err)
		}
		roots = append(roots, Root{Name: name, Host: path.Clean(host), Container: ctr})
	}
	for i, a := range roots {
		for _, b := range roots[i+1:] {
			for _, p := range [][2]Root{{a, b}, {b, a}} {
				outer, inner := p[0], p[1]
				switch {
				case outer.Container == WorkDir:
					// Whatever the other root's path: /work is for a sole root.
					return nil, fmt.Errorf("root %s at %s cannot share the sandbox with %s at %s, which only a sole root may use; give %s a path of its own",
						inner.Name, inner.Container, outer.Name, WorkDir, outer.Name)
				case Within(inner.Container, outer.Container):
					return nil, fmt.Errorf("roots %s and %s nest in the sandbox (%s, %s): a project under both would have two paths; give them paths that do not contain one another",
						outer.Name, inner.Name, outer.Container, inner.Container)
				}
			}
		}
	}
	return roots, nil
}

// CheckContainerPath refuses a root's container path that is not absolute
// and clean, or that is at, inside or around a directory the sandbox
// itself needs: /, the system's directories (ReservedPaths), the home and
// caboose's own mounts in it. Mounting a root there would hide what the
// image has, or give a root's contents a say in how the sandbox runs.
func CheckContainerPath(p string) error {
	if err := badPathChar(p, ":,"); err != nil {
		return fmt.Errorf("path %q %v", p, err)
	}
	if !path.IsAbs(p) || path.Clean(p) != p {
		return fmt.Errorf("path %q is not an absolute, clean path in the sandbox", p)
	}
	if p == "/" {
		return errors.New("path \"/\" would cover the whole sandbox")
	}
	first, _, _ := strings.Cut(p[1:], "/")
	if strings.HasPrefix(first, "lib") {
		return fmt.Errorf("path %q is in /%s, which the sandbox's own system needs", p, first)
	}
	for _, r := range ReservedPaths {
		if Within(p, r) || Within(r, p) {
			return fmt.Errorf("path %q is at, inside or around %s, which the sandbox needs for itself", p, r)
		}
	}
	for _, r := range ExactReservedPaths {
		if p == r {
			return fmt.Errorf("path %q would hide what the image keeps in %s; use a directory inside it", p, r)
		}
	}
	return nil
}

// badPathChar is the error for a path holding a character of bad, which
// docker's -v SOURCE:TARGET would misread (a colon, in either; a comma in
// the target, which the options after it are split on), or a control
// character, which would also garble what is shown of it.
func badPathChar(p, bad string) error {
	for _, r := range p {
		switch {
		case strings.ContainsRune(bad, r):
			return fmt.Errorf("has %q, which a mount cannot carry; use a path without it (a symlink to it will do)", r)
		case unicode.IsControl(r):
			return errors.New("has a control character; use a path without one")
		}
	}
	return nil
}

// ReservedPaths are the container paths no root may be at, inside or
// around, besides /lib* and /: the system's, the home (ContainerHome and
// caboose's mounts in it), and the SSH agent's socket.
var ReservedPaths = []string{
	"/bin", "/boot", "/dev", "/etc", "/home", "/proc", "/root", "/run",
	"/sbin", "/ssh-agent.sock", "/sys", "/tmp", "/usr", "/var",
}

// ExactReservedPaths are container paths no root may be at, although one
// may be inside: what they hold belongs to the image.
var ExactReservedPaths = []string{"/media", "/mnt", "/opt", "/srv"}

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

// DescribeRoots is roots for a message: "/h/dev at /work/dev, /h/w at
// /work/w".
func DescribeRoots(roots []Root) string {
	var parts []string
	for _, r := range roots {
		parts = append(parts, r.Host+" at "+r.Container)
	}
	return strings.Join(parts, ", ")
}

// RootsOrigin says where Roots came from, for messages: the file that set
// them, or that they are the default because nothing did.
func (c *Config) RootsOrigin() string {
	if c.RootsFrom == "" {
		return "the default: config.toml has no [roots]"
	}
	return "set by [roots] in " + c.RootsFrom
}

// RootsHelp is the explanation every root error ends with, indented to
// follow a "caboose: " first line. The roots are the one setting a new
// user trips over on the first run -- the default is one person's layout
// -- so the message has to say what they are and how to change them, not
// just that they are wrong.
const RootsHelp = `       The roots are the directories mounted into the sandbox, each at
       /work/<name>; every project you run caboose in has to be under one.
       Name the directories that hold your projects in the environment's
       config.toml ('caboose setup roots' asks for them), e.g.:
           [roots]
           projects = "~/path/holding/your/projects"
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
	return fmt.Errorf("root %s, %s, %s (%s).\n%s", r.Name, r.Host, problem, c.RootsOrigin(), RootsHelp)
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

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
