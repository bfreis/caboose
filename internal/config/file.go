package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName is an environment's config file, in its EnvDir.
const FileName = "config.toml"

// fileKeys maps each key config.toml accepts to the setting it stands for:
// the name, after CABOOSE_, of the variable that overrides it.
var fileKeys = map[string]string{
	"repo_root":     "REPO_ROOT",
	"image":         "IMAGE",
	"container":     "CONTAINER",
	"base_image":    "BASE_IMAGE",
	"keep_versions": "KEEP_VERSIONS",
	"ready_timeout": "READY_TIMEOUT",
	"docker_sock":   "DOCKER_SOCK",
	"tz":            "TZ",
	"no_tmux":       "NO_TMUX",
	"no_auto_build": "NO_AUTO_BUILD",
	"auto_sync":     "AUTO_SYNC",
	// A list, or a string split at whitespace as the variable is.
	"docker_run_args": "DOCKER_RUN_ARGS",
	"forward_ports":   "FORWARD_PORTS",
	"open_urls":       "OPEN_URLS",
	"isolation":       "ISOLATION",
	"vm_cpus":         "VM_CPUS",
	"vm_memory":       "VM_MEMORY",
	"egress_proxy":    "EGRESS_PROXY",
	"egress_ports":    "EGRESS_PORTS",
	"egress_allow":    "EGRESS_ALLOW",
	"ssh_agent":       "SSH_AGENT",
	"host_exec":       "HOST_EXEC",
}

// rootsKey is the one table config.toml accepts: several repo roots by
// name, each mounted at /work/<name>, in place of repo_root.
const rootsKey = "roots"

// formatKey is config.toml's structure, a whole number. A file without
// one is from before it had one, and read as FileFormat.
const formatKey = "format"

// FileFormat is the config.toml structure this caboose reads and writes. A
// file of a newer one is refused: it may mean something this caboose
// cannot tell.
const FileFormat = 1

// File is an environment's config.toml as read: each setting's value by its
// CABOOSE_ name, as an environment variable would give it.
type File struct {
	Path string
	Vals map[string]string
	// Roots is the [roots] table: host path by name, as written.
	Roots map[string]string
	// Format is the file's format key, 0 when it has none.
	Format int
	// RunArgs is docker_run_args written as a list, nil otherwise; written
	// as a string, it is in Vals, as the variable would give it.
	RunArgs []string
}

// readFile reads path, or returns nil when there is none. Every key must be
// one of fileKeys -- a misspelled one would otherwise be ignored without a
// word -- and a string, a whole number or a boolean.
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
// file in errors.
func ParseFile(path string, data []byte) (*File, error) {
	var raw map[string]any
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	f := &File{Path: path, Vals: map[string]string{}}
	var err error
	var unknown []string
	for k, v := range raw {
		if k == rootsKey {
			if f.Roots, err = readRoots(path, v); err != nil {
				return nil, err
			}
			continue
		}
		if k == formatKey {
			n, ok := v.(int64)
			switch {
			case !ok || n < 1:
				return nil, fmt.Errorf("%s: format must be a whole number, 1 or more", path)
			case n > FileFormat:
				return nil, fmt.Errorf("%s is format %d, and this caboose reads up to format %d: 'caboose update' installs one that reads it", path, n, FileFormat)
			}
			f.Format = int(n)
			continue
		}
		name, ok := fileKeys[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		switch v := v.(type) {
		case []any:
			if name != "DOCKER_RUN_ARGS" {
				return nil, fmt.Errorf("%s: %s must be a string, a number or true/false", path, k)
			}
			f.RunArgs = []string{}
			for _, e := range v {
				s, ok := e.(string)
				if !ok || s == "" {
					return nil, fmt.Errorf("%s: %s must be a list of strings, each an argument to docker run", path, k)
				}
				f.RunArgs = append(f.RunArgs, s)
			}
		case string:
			f.Vals[name] = v
		case int64:
			f.Vals[name] = fmt.Sprint(v)
		case bool:
			// egress_proxy is "on" or "off", and false must not read as
			// unset, which is on.
			// Nor host_exec, whose false must say off over a variable's
			// absence as plainly as its true says on.
			if name == "EGRESS_PROXY" || name == "HOST_EXEC" {
				f.Vals[name] = map[bool]string{true: "on", false: "off"}[v]
				continue
			}
			// The variables' own meaning: set (non-empty) or not.
			if v {
				f.Vals[name] = "1"
			}
		default:
			return nil, fmt.Errorf("%s: %s must be a string, a number or true/false", path, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		var known []string
		for k := range fileKeys {
			known = append(known, k)
		}
		sort.Strings(known)
		known = append(known, formatKey, "["+rootsKey+"]")
		return nil, fmt.Errorf("%s: unknown setting %s (known: %s)", path,
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	if f.Roots != nil && f.Vals["REPO_ROOT"] != "" {
		return nil, fmt.Errorf("%s: repo_root and [roots] both name the repo roots; keep one", path)
	}
	return f, nil
}

// readRoots reads the [roots] table: at least one entry, each a path by a
// name that becomes a directory under /work.
func readRoots(path string, v any) (map[string]string, error) {
	table, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: roots must be a table, [roots], of name = \"path\"", path)
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("%s: [roots] names no roots", path)
	}
	roots := map[string]string{}
	for name, p := range table {
		if !ValidRootName(name) {
			return nil, fmt.Errorf("%s: '%s' is not a root name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", path, name)
		}
		s, ok := p.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("%s: roots.%s must be a path", path, name)
		}
		roots[name] = s
	}
	return roots, nil
}

// ValidRootName reports whether name can name a root: it becomes the
// directory /work/<name>, so it follows the same rule as an environment's.
func ValidRootName(name string) bool { return envName.MatchString(name) }

// Template is the config.toml `caboose setup` writes: every setting,
// commented out, at its default.
const Template = `# This environment's settings. Each can also be set for one shell by the
# CABOOSE_ variable in brackets, which wins over this file. What the sandbox
# keeps of its home, and what of that syncs, is not here: that is the
# sandbox's own ~/.config/caboose/sandbox.toml.

format = 1    # this file's structure

# The directory holding your projects, mounted into the container at /work.
# [CABOOSE_REPO_ROOT]
#repo_root = "~/dev"

# Or several, in place of repo_root, each mounted at /work/<name>:
#[roots]
#dev = "~/dev"
#work = "~/work"

# An image to build the sandbox on, instead of caboose's own default.
# [CABOOSE_BASE_IMAGE]
#base_image = "debian:13.7-slim"

# How many Claude Code versions to keep installed (~224MB each).
# [CABOOSE_KEEP_VERSIONS]
#keep_versions = 2

# What keeps the sandbox from this machine: "docker" (runc, the default,
# which shares this machine's kernel), "gvisor" (runsc, a kernel of its
# own; docker must have runsc registered) or "vm" (a VM of caboose's own,
# on a Mac, with no Docker needed). [CABOOSE_ISOLATION]
#isolation = "gvisor"

# The vm isolation's size: CPUs, and memory ("8G", "4096M"). By default
# half this machine's CPUs and half its memory, at most 8G.
# [CABOOSE_VM_CPUS, CABOOSE_VM_MEMORY]
#vm_cpus = 4
#vm_memory = "8G"

# Mount the host's docker socket: root-equivalent access to the host.
# [CABOOSE_DOCKER_SOCK]
#docker_sock = "/var/run/docker.sock"

# Skip tmux: native rendering, but no detach/reattach. [CABOOSE_NO_TMUX]
#no_tmux = true

# Refuse to build a missing image on the first launch. [CABOOSE_NO_AUTO_BUILD]
#no_auto_build = true

# Sync (caboose sync) before a launch that finds nothing running in the
# container, once a sync remote is set. [CABOOSE_AUTO_SYNC]
#auto_sync = true

# More arguments for docker run as the container is created, each written
# --flag=value; caboose's own flags are refused. Some remove isolation, as
# the docker socket does. [CABOOSE_DOCKER_RUN_ARGS, split at whitespace]
#docker_run_args = ["--cap-add=NET_ADMIN", "--device=/dev/net/tun"]

# Which ports listening in the sandbox are forwarded to the same port on
# this machine's localhost: ports and ranges, or "none". A running link
# rereads this file when it changes. [CABOOSE_FORWARD_PORTS]
#forward_ports = "3000-3999 5173 8000-8999"

# Whether the sandbox may open URLs in this machine's browser: "ask" (a
# dialog each time), "allow" or "off". [CABOOSE_OPEN_URLS]
#open_urls = "ask"

# Under isolation "vm": whether the sandbox's outbound connections are
# made from this machine, through the link, so that a VPN's routes and
# DNS apply as they do here: "on" (the default) or "off" (the VM's own
# NAT). Ignored under docker and gvisor. [CABOOSE_EGRESS_PROXY]
#egress_proxy = "off"

# The ports the sandbox may reach through it: ports and ranges, or
# "none". [CABOOSE_EGRESS_PORTS]
#egress_ports = "22 80 443"

# Private addresses it may reach anyway (loopback, private, link-local,
# tailnet and multicast ones are refused otherwise): names, "*.suffix"
# patterns, CIDRs or addresses. [CABOOSE_EGRESS_ALLOW]
#egress_allow = "git.corp.example *.internal.example 10.20.0.0/16"

# The SSH agent the sandbox gets: a socket on this machine, or "none". By
# default the one ssh here would use, an IdentityAgent in ~/.ssh/config
# or else $SSH_AUTH_SOCK -- which a work tool may have taken over. Under
# isolation "vm", and docker or gvisor on Linux; on a Mac OrbStack and
# Docker Desktop forward the agent they were started with instead.
# [CABOOSE_SSH_AGENT]
#ssh_agent = "~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"

# Let sessions run commands on this machine, as you, through the link
# (caboose-agent host CMD): a hole in the wall, on purpose. Sessions, and
# whatever steers them (a web page, a repo they work in), can then run
# anything here. For an environment whose point is a separate login and
# tools, not containment. Off by default. [CABOOSE_HOST_EXEC]
#host_exec = true
`
