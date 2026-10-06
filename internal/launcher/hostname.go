package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
)

// defaultHostname is the prefix of every default hostname, and all of it
// when the machine has no name.
const defaultHostname = "caboose"

// maxHostname is the longest a hostname may be: one DNS label (RFC 1123).
const maxHostname = 63

// cleanHostPart makes a machine's name fit in a hostname: cut at the
// first dot (a domain is no part of the name), lowercase, anything but
// letters, digits and - a -, runs of - one, none at either end.
func cleanHostPart(s string) string {
	s, _, _ = strings.Cut(s, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(collapseDashes(b.String()), "-")
}

func collapseDashes(s string) string {
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return s
}

// hostnameOf is the default hostname for environment env on a machine
// called host: caboose-<host>, and -<env> after it in any environment but
// the default one, within one DNS label. It is the host part that gives
// way when the name is too long, never the prefix or the environment.
func hostnameOf(env, host string) string {
	suffix := ""
	if env != config.DefaultEnv && env != "" {
		if e := cleanHostPart(env); e != "" {
			suffix = "-" + e
		}
	}
	host = cleanHostPart(host)
	if room := maxHostname - len(defaultHostname) - 1 - len(suffix); len(host) > room {
		host = strings.TrimRight(host[:max(room, 0)], "-")
	}
	if host == "" {
		return defaultHostname + suffix
	}
	return defaultHostname + "-" + host + suffix
}

// machineName is this machine's name for a hostname. On a Mac,
// os.Hostname follows the network (DHCP, a VPN), so the Bonjour
// LocalHostName, which the user sets, is preferred; run is how scutil is
// run. "" when neither says.
func machineName(goos string, run func(name string, args ...string) ([]byte, error), osHostname func() (string, error)) string {
	if goos == "darwin" {
		if out, err := run("scutil", "--get", "LocalHostName"); err == nil {
			if n := strings.TrimSpace(string(out)); n != "" {
				return n
			}
		}
	}
	n, _ := osHostname()
	return n
}

// hostPart is the cleaned name of this machine, "" when it has none.
func (a *App) hostPart() string {
	if a.HostPart != nil {
		return a.HostPart()
	}
	return cleanHostPart(machineName(runtime.GOOS, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}, os.Hostname))
}

// hostname is the sandbox's: hostname in config.toml, else the default.
func (a *App) hostname() string {
	if a.Cfg.Hostname != "" {
		return a.Cfg.Hostname
	}
	return hostnameOf(a.Cfg.Env, a.hostPart())
}

// syncHost names this machine in sync commits: the cleaned name, or
// os.Hostname as it is when that cleans to nothing.
func (a *App) syncHost() string {
	if h := a.hostPart(); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

// createdHostname is the hostname the container was created with; ok is
// false when that cannot be told, or the container records none.
func (a *App) createdHostname() (name string, ok bool) {
	labels, err := a.box().Labels()
	if err != nil {
		return "", false
	}
	name, ok = labels[assets.LabelHostname]
	return name, ok
}

// hostnameDrift says how the container's hostname differs from the
// configuration's, or "" when it does not, or it cannot be told.
func (a *App) hostnameDrift() string {
	if d := a.missingLabel(assets.LabelHostname, "hostname"); d != "" {
		return d
	}
	have, ok := a.createdHostname()
	want := a.hostname()
	if !ok || have == want {
		return ""
	}
	return fmt.Sprintf("the container was created with hostname %s; the configuration says %s", have, want)
}

// warnIfHostnameDrifted is warnIfRunArgsDrifted for the hostname.
func (a *App) warnIfHostnameDrifted() {
	if d := a.hostnameDrift(); d != "" {
		a.Note("%s.", d)
		a.Note("run 'caboose restart' to recreate it with it (this kills running sessions).")
	}
}
