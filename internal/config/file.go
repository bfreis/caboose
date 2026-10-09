package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName is an environment's config file, in its EnvDir.
const FileName = "config.toml"

// FileFormat is the config.toml structure this caboose reads and writes. A
// file of a newer one is refused: it may mean something this caboose
// cannot tell.
const FileFormat = 1

// ConfigDoc is where config.toml is documented, for errors that send the
// reader there.
const ConfigDoc = "docs/configuration.md"

// The isolation kinds, each the name of a family of profile tables:
// [container.NAME] runs the sandbox under docker's own runtime (runc),
// [gvisor.NAME] under runsc, [vm.NAME] in a VM of caboose's own.
const (
	KindContainer = "container"
	KindGVisor    = "gvisor"
	KindVM        = "vm"
)

// Kinds are the isolation kinds, the default first.
var Kinds = []string{KindContainer, KindGVisor, KindVM}

// The image kinds, each the name of a family of profile tables as the
// isolation kinds are: [apko.NAME] builds the base from packages,
// [dockerfile.NAME] from a Dockerfile of the user's, and [ref.NAME] is an
// image of the user's, used as it is.
const (
	ImageKindApko       = "apko"
	ImageKindDockerfile = "dockerfile"
	ImageKindRef        = "ref"
)

// ImageKinds are the image kinds, the default first.
var ImageKinds = []string{ImageKindApko, ImageKindDockerfile, ImageKindRef}

// valueType is what a key's value must be.
type valueType int

const (
	typeString valueType = iota
	typeInt
	typeBool
	// typeStrings is an array of non-empty strings.
	typeStrings
	// typePorts is an array of ports (whole numbers) and ranges ("a-b").
	typePorts
)

var portRangeSyntax = regexp.MustCompile(`^[0-9]+-[0-9]+$`)

func (t valueType) String() string {
	return [...]string{"a string", "a whole number", "true or false", "an array of strings",
		`an array of ports and "a-b" ranges`}[t]
}

// tables are the keys of config.toml's plain tables, by table, "" for the
// top level. [roots] and the isolation profiles are read apart.
var tables = map[string]map[string]valueType{
	"": {"format": typeInt, "isolation": typeString, "image": typeString},
	"build": {
		"auto_build": typeBool,
	},
	"session": {
		"tmux":          typeBool,
		"tz":            typeString,
		"hostname":      typeString,
		"auto_sync":     typeBool,
		"keep_versions": typeInt,
		"ready_timeout": typeInt,
	},
	"link": {
		"forward_ports": typePorts,
		"open_urls":     typeString,
		"ssh_agent":     typeString,
		"host_exec":     typeBool,
	},
}

// profileKeys are the keys of each kind's profiles. Each kind has its own:
// a key another kind has is an error in this one's table, so a setting
// that cannot apply under a kind cannot be written for it.
var profileKeys = map[string]map[string]valueType{
	KindContainer: {"run_args": typeStrings, "engine_socket": typeBool},
	KindGVisor:    {"run_args": typeStrings, "engine_socket": typeBool},
	KindVM: {
		"cpus":         typeInt,
		"memory":       typeString,
		"egress":       typeBool,
		"egress_ports": typePorts,
		"egress_allow": typeStrings,
	},
}

// imageProfileKeys are the keys of each image kind's profiles, as
// profileKeys are the isolation kinds'.
var imageProfileKeys = map[string]map[string]valueType{
	ImageKindApko:       {"packages": typeStrings, "defaults": typeBool},
	ImageKindDockerfile: {},
	ImageKindRef:        {"image": typeString},
}

// kindKeys are the keys of kind's profiles, an isolation kind's or an image
// kind's; nil for a name that is neither.
func kindKeys(kind string) map[string]valueType {
	if k := profileKeys[kind]; k != nil {
		return k
	}
	return imageProfileKeys[kind]
}

// rootsKey is the [roots] table: host directories by name, each mounted at
// /work/<name> unless its long form names another path.
const rootsKey = "roots"

// File is an environment's config.toml as read and checked.
type File struct {
	Path string
	// Vals are the keys the file sets, by their dotted names ("isolation",
	// "session.tmux", "vm.default.cpus"), as TOML gave them: a string, an
	// int64, a bool or a []any, each of its key's type.
	Vals map[string]any
	// Roots is the [roots] table, as written; nil when there is none.
	Roots map[string]FileRoot
	// Profiles are the isolation profiles the file defines, as
	// "<kind>.<name>", sorted.
	Profiles []string
	// ImageProfiles are the image profiles the file defines, as
	// "<kind>.<name>", sorted.
	ImageProfiles []string
	// Format is the file's format key, 0 when it has none.
	Format int
}

// FileRoot is a [roots] entry as written: the host path, ~ and all, and
// the container path when the long form names one ("" for /work/<name>).
type FileRoot struct{ Host, Path string }

// Has reports whether the file sets key, a dotted name.
func (f *File) Has(key string) bool {
	if f == nil {
		return false
	}
	_, ok := f.Vals[key]
	return ok
}

// readFile reads path, or returns nil when there is none.
func readFile(path string, fsys FS) (*File, error) {
	data, err := fsys.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseFile(path, data)
}

// ParseFile is readFile for contents already read: path only names the
// file in errors. Every table and key must be one this caboose knows --
// a misspelled one would otherwise be ignored without a word -- and every
// value of its key's type.
func ParseFile(path string, data []byte) (*File, error) {
	var raw map[string]any
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	f := &File{Path: path, Vals: map[string]any{}}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{path}, args...)...)
	}
	// The format first: a newer file may have keys this caboose does not
	// know, and what it needs to hear is that it is too old.
	if n, ok := raw["format"].(int64); ok && n > FileFormat {
		return nil, fmt.Errorf("%s is format %d, and this caboose reads up to format %d: 'caboose update' installs one that reads it", path, n, FileFormat)
	}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		v := raw[k]
		switch {
		case k == rootsKey:
			roots, err := readRoots(v)
			if err != nil {
				return nil, fail("%v", err)
			}
			f.Roots = roots
		case slices.Contains(Kinds, k):
			names, err := f.readProfiles(k, v)
			if err != nil {
				return nil, fail("%v", err)
			}
			f.Profiles = append(f.Profiles, names...)
		case slices.Contains(ImageKinds, k):
			names, err := f.readProfiles(k, v)
			if err != nil {
				return nil, fail("%v", err)
			}
			f.ImageProfiles = append(f.ImageProfiles, names...)
		case tables[k] != nil && k != "":
			if err := f.readTable(k, v, tables[k]); err != nil {
				return nil, fail("%v", err)
			}
		default:
			t, ok := tables[""][k]
			if !ok {
				return nil, fail("%s", unknownKey("", k))
			}
			if err := f.readValue(k, v, t); err != nil {
				return nil, fail("%v", err)
			}
		}
	}
	if v, ok := f.Vals["format"]; ok {
		switch n := v.(int64); {
		case n < 1:
			return nil, fail("format must be a whole number, 1 or more")
		default:
			f.Format = int(n)
		}
	}
	return f, nil
}

// readProfiles reads kind's profile tables, [kind.NAME], each with the
// keys of its kind, and returns their "<kind>.<name>"s, sorted.
func (f *File) readProfiles(kind string, v any) ([]string, error) {
	profiles, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be tables of profiles, [%s.NAME]", kind, kind)
	}
	var names []string
	for _, name := range slices.Sorted(maps.Keys(profiles)) {
		if _, ok := profiles[name].(map[string]any); !ok {
			return nil, fmt.Errorf("%s must be tables of profiles, [%s.NAME]", kind, kind)
		}
		if !ValidProfileName(name) {
			return nil, fmt.Errorf("[%s.%s]: '%s' is not a profile name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", kind, name, name)
		}
		if err := f.readTable(kind+"."+name, profiles[name], kindKeys(kind)); err != nil {
			return nil, err
		}
		names = append(names, kind+"."+name)
	}
	return names, nil
}

// readTable reads table name's keys, each of keys.
func (f *File) readTable(name string, v any, keys map[string]valueType) error {
	t, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a table, [%s]", name, name)
	}
	for _, k := range slices.Sorted(maps.Keys(t)) {
		vt, ok := keys[k]
		if !ok {
			return errors.New(unknownKey(name, k))
		}
		if err := f.readValue(name+"."+k, t[k], vt); err != nil {
			return err
		}
	}
	return nil
}

// readValue checks v is of type t, and keeps it as key's.
func (f *File) readValue(key string, v any, t valueType) error {
	bad := fmt.Errorf("%s must be %s", displayKey(key), t)
	switch t {
	case typeString:
		if _, ok := v.(string); !ok {
			return bad
		}
	case typeInt:
		if _, ok := v.(int64); !ok {
			return bad
		}
	case typeBool:
		if _, ok := v.(bool); !ok {
			return bad
		}
	case typeStrings:
		list, ok := v.([]any)
		if !ok {
			return bad
		}
		for _, e := range list {
			if s, ok := e.(string); !ok || s == "" {
				return bad
			}
		}
	case typePorts:
		list, ok := v.([]any)
		if !ok {
			return bad
		}
		for _, e := range list {
			switch e := e.(type) {
			case int64:
			case string:
				if !portRangeSyntax.MatchString(e) {
					return fmt.Errorf(`%s: %q is not a range of ports, "a-b"`, displayKey(key), e)
				}
			default:
				return bad
			}
			// The same reading the launch makes of them, so a port that
			// cannot be one is said when the file is read.
			if _, err := ParsePortsOf(displayKey(key), fmt.Sprint(e)); err != nil {
				return err
			}
		}
	}
	f.Vals[key] = v
	return nil
}

// displayKey is a dotted key as a person finds it in the file: "tz in
// [session]", "isolation".
func displayKey(key string) string {
	i := strings.LastIndex(key, ".")
	if i < 0 {
		return key
	}
	return key[i+1:] + " in [" + key[:i] + "]"
}

// unknownKey is the error for key in table ("" for the top level): the
// keys there are.
func unknownKey(table, key string) string {
	var known []string
	if table == "" {
		for k := range tables[""] {
			known = append(known, k)
		}
		for k := range tables {
			if k != "" {
				known = append(known, "["+k+"]")
			}
		}
		known = append(known, "["+rootsKey+"]")
		for _, k := range slices.Concat(Kinds, ImageKinds) {
			known = append(known, "["+k+".NAME]")
		}
	} else {
		keys := tables[table]
		if keys == nil {
			kind, _, _ := strings.Cut(table, ".")
			keys = kindKeys(kind)
		}
		for k := range keys {
			known = append(known, k)
		}
	}
	slices.Sort(known)
	where := "top-level setting"
	if table != "" {
		where = "setting in [" + table + "]"
	}
	if len(known) == 0 {
		return fmt.Sprintf("unknown %s %s (it takes none: the table alone defines the profile; see %s, or 'caboose update' if this caboose predates it)", where, key, ConfigDoc)
	}
	return fmt.Sprintf("unknown %s %s (known: %s; see %s, or 'caboose update' if this caboose predates it)", where, key, strings.Join(known, ", "), ConfigDoc)
}

// readRoots reads the [roots] table: at least one entry, each a host path
// by a name, or the long form, a table of exactly host and path.
func readRoots(v any) (map[string]FileRoot, error) {
	table, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New(`roots must be a table, [roots], of name = "path"`)
	}
	if len(table) == 0 {
		return nil, errors.New("[roots] names no roots")
	}
	roots := map[string]FileRoot{}
	for name, e := range table {
		if !ValidRootName(name) {
			return nil, fmt.Errorf("'%s' is not a root name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", name)
		}
		switch e := e.(type) {
		case string:
			if e == "" {
				return nil, fmt.Errorf("root %s must be a path", name)
			}
			roots[name] = FileRoot{Host: e}
		case map[string]any:
			host, hok := e["host"].(string)
			p, pok := e["path"].(string)
			if len(e) != 2 || !hok || !pok || host == "" || p == "" {
				return nil, fmt.Errorf(`root %s must be a path, or a table of exactly host = "path on this machine" and path = "path in the sandbox"`, name)
			}
			roots[name] = FileRoot{Host: host, Path: p}
		default:
			return nil, fmt.Errorf("root %s must be a path", name)
		}
	}
	return roots, nil
}

// ValidRootName reports whether name can name a root: it becomes the
// directory /work/<name>, so it follows the same rule as an environment's.
func ValidRootName(name string) bool { return envName.MatchString(name) }

// ValidProfileName reports whether name can name a profile, as in
// [vm.NAME] or [apko.NAME]: the same rule as a root's.
func ValidProfileName(name string) bool { return envName.MatchString(name) }

// Template is the config.toml `caboose setup` writes: every setting,
// commented out, at its default.
const Template = `# This environment's settings (see docs/configuration.md). What the
# sandbox keeps of its home, and what of that syncs, is not here: that is
# the sandbox's own ~/.config/caboose/sandbox.toml.

format = 1    # this file's structure

# The isolation profile the sandbox runs under, "<kind>.<name>": one of the
# [container.NAME], [gvisor.NAME] and [vm.NAME] tables below. Needed only
# when there are several; with none, the sandbox is a plain container.
#isolation = "gvisor.default"

# The image profile the sandbox is built from, "<kind>.<name>": one of the
# [apko.NAME], [dockerfile.NAME] and [ref.NAME] tables below. Needed only
# when there are several; with none, it is apko.default, caboose's
# packages.
#image = "apko.default"

# The directories holding your projects, each mounted into the sandbox at
# /work/<name>, or at a path of its own in the long form. By default
# dev = "~/dev". A sole root may take path = "/work" itself.
#[roots]
#dev = "~/dev"
#[roots.tools]
#host = "~/src/tools"
#path = "/opt/tools"

#[build]
# Build a missing image, or rebuild a stale one, when the sandbox is
# created; false says to run 'caboose build' instead.
#auto_build = true

#[session]
# Run sessions in tmux, which detaches and reattaches them; false renders
# natively, with no detach.
#tmux = true
# The sandbox's timezone. By default this machine's.
#tz = "Europe/Lisbon"
# The sandbox's hostname: one lowercase DNS label. By default
# caboose-<this machine's name>, with -<env> after it in any environment
# but the default one. Takes effect at the next 'caboose restart'.
#hostname = "caboose-laptop"
# Sync (caboose sync) before a launch that finds nothing running, once a
# sync remote is set.
#auto_sync = false
# How many Claude Code versions to keep installed (~224MB each).
#keep_versions = 2
# Seconds to wait for the sandbox's first start.
#ready_timeout = 600

#[link]
# Which ports listening in the sandbox are forwarded to the same port on
# this machine's localhost, while they listen: ports and "a-b" ranges; []
# for none. 32768-60999 is where a login's loopback callback listens. A
# running link rereads this file when it changes.
#forward_ports = ["3000-3999", 5173, "8000-8999", "32768-60999"]
# Whether the sandbox may open URLs in this machine's browser: "ask" (a
# dialog each time), "allow" or "off".
#open_urls = "ask"
# The SSH agent the sandbox gets: a socket on this machine, or "none". By
# default the one ssh here would use, an IdentityAgent in ~/.ssh/config
# or else $SSH_AUTH_SOCK. Under vm, and container or gvisor on Linux; on
# a Mac OrbStack and Docker Desktop forward the agent they were started
# with instead.
#ssh_agent = "~/.ssh/agent.sock"
# Let sessions run commands on this machine, as you, through the link
# (caboose-agent host CMD): a hole in the wall, on purpose. Sessions, and
# whatever steers them (a web page, a repo they work in), can then run
# anything here. For an environment whose point is a separate login and
# tools, not containment.
#host_exec = false

# A plain container, under docker's own runtime: it shares this machine's
# kernel.
#[container.default]
# More arguments for docker run as the container is created, each written
# --flag=value; caboose's own flags are refused. Some remove isolation, as
# the engine socket does.
#run_args = ["--cap-add=NET_ADMIN", "--device=/dev/net/tun"]
# Mount the engine's socket: root-equivalent access to this machine.
#engine_socket = false

# A container under gVisor (runsc), a kernel of its own; docker must have
# runsc registered ('caboose setup isolation' does that where it can).
# It takes the keys [container.NAME] takes.
#[gvisor.default]

# A VM of caboose's own, on a Mac, with no Docker needed.
#[vm.default]
# CPUs, and memory ("8G", "4096M"). By default half this machine's CPUs
# and half its memory, at most 8G.
#cpus = 4
#memory = "8G"
# Make the sandbox's outbound connections from this machine, through the
# link, so that a VPN's routes and DNS apply as they do here; false uses
# the VM's own NAT.
#egress = true
# The ports the sandbox may reach that way: ports and "a-b" ranges.
#egress_ports = [22, 80, 443]
# Private addresses it may reach anyway (loopback, private, link-local,
# tailnet and multicast ones are refused otherwise): names, "*.suffix"
# patterns, CIDRs or addresses.
#egress_allow = ["git.corp.example", "*.internal.example", "10.20.0.0/16"]

# The base built from caboose's packages, with apko: no Dockerfile. The
# packages are pinned in apko-<name>.lock.json next to this file, which
# 'caboose build --pull' refreshes.
#[apko.default]
# Packages of your own, on top of caboose's: Wolfi package names.
#packages = ["postgresql-17-client"]
# Whether caboose's default package groups come with them; false leaves
# only what the sandbox requires, and packages.
#defaults = true

# A base built from a Dockerfile of yours, an empty table: its build
# context is dockerfile/<name> next to this file, Dockerfile and all, which
# 'caboose setup image' seeds with caboose's.
#[dockerfile.default]

# An image of your own, used as it is (pulled when it is not local).
#[ref.default]
#image = "debian:13.7-slim"
`
