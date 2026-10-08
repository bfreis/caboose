package launcher

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// The user's own `docker run` arguments (config.RunArgs) go in after
// caboose's, where most of docker's single-value flags take the last one
// given -- so one of them could quietly undo what the launcher counts on:
// the container's name, its user, its entrypoint, its labels, a mount of
// its own. They are the host's to set (a container or gvisor profile's
// run_args in config.toml, never the sandbox config, which a session
// writes), and loosening the sandbox is theirs to choose; what is checked
// here is only that they leave
// caboose's own flags alone. Each is written --flag or --flag=value:
// docker's parser takes a flag's value from the next argument too, and
// checking that would mean knowing which of its flags take one.

// ownedFlags are the docker run flags caboose sets or depends on, and why.
var ownedFlags = map[string]string{
	"name":        "caboose names the container after the environment",
	"hostname":    "caboose sets the hostname (hostname in [session])",
	"restart":     "caboose sets the restart policy",
	"init":        "caboose runs the container under tini",
	"detach":      "caboose runs the container detached",
	"rm":          "the container is long-lived, and caboose removes it itself",
	"entrypoint":  "the image's entrypoint is caboose's",
	"user":        "the container runs as the agent user the image made, or as isolation needs",
	"runtime":     "set by isolation in config.toml, which also picks the user it needs",
	"interactive": "the container runs detached",
	"tty":         "the container runs detached",
	"attach":      "the container runs detached",
	"platform":    "the image is built for this machine's platform",
	"pull":        "the image is built here, never pulled",
	"cidfile":     "caboose finds the container by its name",
	"env-file":    "caboose cannot check what it sets; use --env=NAME=VALUE",
	"label-file":  "caboose cannot check what it sets; use --label=KEY=VALUE",
	"help":        "it is not an argument for a container",
}

// bareFlags are the boolean flags that may be written without =value.
// Any other flag without one would take the next argument as its value.
var bareFlags = []string{"privileged", "read-only", "oom-kill-disable", "no-healthcheck", "publish-all", "use-api-socket"}

// shortFlags are docker run's short flags, by their long names, to say
// which to write instead.
var shortFlags = map[byte]string{
	'a': "attach", 'c': "cpu-shares", 'd': "detach", 'e': "env", 'h': "hostname",
	'i': "interactive", 'l': "label", 'm': "memory", 'p': "publish", 'P': "publish-all",
	't': "tty", 'u': "user", 'v': "volume", 'w': "workdir",
}

// ownedEnv are the variables caboose or its image sets, beyond those in its
// own -e arguments; ownedEnvPrefixes, the prefixes of its own.
var (
	ownedEnv         = []string{"HOME", "PATH", "TZ", "SSH_AUTH_SOCK", "USE_BUILTIN_RIPGREP", "IS_SANDBOX"}
	ownedEnvPrefixes = []string{"CABOOSE_", "TINI_"}
)

// labelPrefix is the prefix of every label caboose sets (assets.Label*).
const labelPrefix = "dev.bfreis.caboose."

// checkRunArgs refuses the first of args, the user's docker run arguments,
// that is not written --flag[=value] or would override what caboose sets.
// own are caboose's own arguments as createContainer builds them (-v
// SRC:DST, -e NAME=VALUE), whose mount targets and variables are caboose's
// too; with none (doctor, restart before it removes anything), what is
// checked is what does not depend on them, and the roots. caboose's
// instructions (datadir.ManagedTarget) are always its own: a mount over
// them would hand the sandbox instructions of its own choosing.
func checkRunArgs(args, own []string, roots []config.Root) error {
	targets := []string{config.WorkDir, datadir.ManagedTarget}
	for _, r := range roots {
		targets = append(targets, r.Container)
	}
	env := slices.Clone(ownedEnv)
	for i := 0; i+1 < len(own); i++ {
		switch own[i] {
		case "-v":
			if t := mountTarget("volume", own[i+1]); t != "" {
				targets = append(targets, t)
			}
		case "-e":
			name, _, _ := strings.Cut(own[i+1], "=")
			env = append(env, name)
		}
	}
	for _, arg := range args {
		if err := checkRunArg(arg, targets, env); err != nil {
			return fmt.Errorf("'%s': %s", arg, err)
		}
	}
	return nil
}

func checkRunArg(arg string, targets, env []string) error {
	long, ok := strings.CutPrefix(arg, "--")
	switch {
	case !ok && len(arg) > 1 && arg[0] == '-':
		if name, ok := shortFlags[arg[1]]; ok {
			return fmt.Errorf("a short flag; write the long one, --%s=...", name)
		}
		return fmt.Errorf("a short flag; write the long one, --flag=value")
	case !ok:
		return fmt.Errorf("not a flag; write each flag and its value as one argument, --flag=value")
	case long == "":
		return fmt.Errorf("not a flag")
	}
	name, value, hasValue := strings.Cut(long, "=")
	if why, ok := ownedFlags[name]; ok {
		return fmt.Errorf("caboose's own: %s", why)
	}
	if !hasValue {
		if slices.Contains(bareFlags, name) {
			return nil
		}
		return fmt.Errorf("no value; write it as --%s=VALUE", name)
	}
	switch name {
	case "env":
		v, _, _ := strings.Cut(value, "=")
		if slices.Contains(env, v) || slices.ContainsFunc(ownedEnvPrefixes, func(p string) bool { return strings.HasPrefix(v, p) }) {
			return fmt.Errorf("caboose sets %s itself", v)
		}
	case "label":
		if strings.HasPrefix(value, labelPrefix) {
			return fmt.Errorf("caboose's labels are its own")
		}
	case "volume", "mount", "tmpfs":
		t := mountTarget(name, value)
		if t == "" {
			return nil
		}
		if config.Within(config.ContainerHome, t) {
			return fmt.Errorf("would hide the agent's home, %s; mount something inside it instead", config.ContainerHome)
		}
		for _, o := range targets {
			if config.Within(t, o) || config.Within(o, t) {
				return fmt.Errorf("overlaps %s, which caboose mounts itself", o)
			}
		}
	}
	return nil
}

// mountTarget is where a --volume, --mount or --tmpfs value mounts, clean,
// or "" when it names no absolute path (docker says what is wrong then).
func mountTarget(flag, value string) string {
	var t string
	switch flag {
	case "volume":
		parts := strings.Split(value, ":")
		t = parts[0]
		if len(parts) > 1 {
			t = parts[1]
		}
	case "tmpfs":
		t, _, _ = strings.Cut(value, ":")
	case "mount":
		for _, f := range strings.Split(value, ",") {
			k, v, _ := strings.Cut(f, "=")
			if k == "target" || k == "dst" || k == "destination" {
				t = v
			}
		}
	}
	if !path.IsAbs(t) {
		return ""
	}
	return path.Clean(t)
}

// wideningFlags are the docker run flags that give a container more of the
// host than caboose's own do: privileges, devices, host namespaces, host
// directories beyond the roots.
var wideningFlags = []string{
	"privileged", "cap-add", "security-opt", "device", "device-cgroup-rule", "gpus",
	"pid", "ipc", "uts", "userns", "cgroupns", "volume", "mount", "volumes-from",
}

// wideningRunArgs are those of args that widen what the sandbox reaches.
func wideningRunArgs(args []string) []string {
	var w []string
	for _, arg := range args {
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if slices.Contains(wideningFlags, name) || (name == "network" || name == "net") && value == "host" {
			w = append(w, arg)
		}
	}
	return w
}

// runArgsLabel is the value of assets.LabelRunArgs for args.
func runArgsLabel(args []string) string {
	if len(args) == 0 {
		return ""
	}
	b, _ := json.Marshal(args)
	return string(b)
}

// createdRunArgs are the user's docker run arguments the container was
// created with; ok is false when that cannot be told, or the container
// records none (an empty label is none).
func (a *App) createdRunArgs() (args []string, ok bool) {
	labels, err := a.box().Labels()
	if err != nil {
		return nil, false
	}
	v, has := labels[assets.LabelRunArgs]
	if !has {
		return nil, false
	}
	if v != "" {
		if json.Unmarshal([]byte(v), &args) != nil {
			return nil, false
		}
	}
	return args, true
}

// runArgsDrift says how the container's docker run arguments differ from
// the configuration's, or "" when they do not, or it cannot be told.
func (a *App) runArgsDrift() string {
	if d := a.missingLabel(assets.LabelRunArgs, "docker run arguments"); d != "" {
		return d
	}
	created, ok := a.createdRunArgs()
	if !ok || slices.Equal(created, a.Cfg.RunArgs) {
		return ""
	}
	return fmt.Sprintf("the container was created with docker run arguments %s; the configuration says %s",
		describeRunArgs(created), describeRunArgs(a.Cfg.RunArgs))
}

func describeRunArgs(args []string) string {
	if len(args) == 0 {
		return "none"
	}
	return strings.Join(args, " ")
}

// warnIfRunArgsDrifted is warnIfRootsDrifted for the docker run arguments.
func (a *App) warnIfRunArgsDrifted() {
	if d := a.runArgsDrift(); d != "" {
		a.Note("%s.", d)
		a.Note("run 'caboose restart' to recreate it with them (this kills running sessions).")
	}
}

// runArgsError is the error for docker run arguments checkRunArgs refused.
func (a *App) runArgsError(err error) error {
	return Die("docker run argument %v (%s)", err, runArgsOrigin(a.Cfg))
}

func runArgsOrigin(c *config.Config) string { return c.Origin(c.Profile + ".run_args") }
