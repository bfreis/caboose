package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ssh through the outbound proxy. HTTP_PROXY reaches no ssh, so with the
// proxy on (BootSpec.Egress) the agent points ssh at it at every boot, as
// root, before the entrypoint starts: never through ~/.ssh, which is the
// user's (a kept mount), but through a drop-in of the system's ssh config,
// which the image's ssh reads when its /etc/ssh/ssh_config includes
// ssh_config.d/*.conf (Debian's, Ubuntu's, Fedora's and Alpine's do). The
// root's /etc is the overlay's, new at every boot, so the file is too.
// ssh takes each option's first value, and reads ~/.ssh/config before the
// system's, so a ProxyCommand, ProxyJump or "ProxyCommand none" of the
// user's for a host wins. An image whose ssh_config includes no drop-ins
// gets GIT_SSH_COMMAND instead, unless it sets one of its own: git's ssh
// goes through the proxy, and a plain ssh does not.
//
// ssh runs a ProxyCommand through /bin/sh -c with %h in it, and an
// OpenSSH before 9.6 lets a hostile host name (a git submodule's URL)
// carry shell into it (CVE-2023-51385). So neither is set up unless
// `ssh -V` says 9.6 or later, or a distribution's build known to carry the
// fix (sshFixed); otherwise ssh keeps the VM's own NAT, and the console
// says why.

// SSHEgressConf is the drop-in, under the guest's root.
const SSHEgressConf = "/etc/ssh/ssh_config.d/50-caboose-egress.conf"

const sshSystemConf = "/etc/ssh/ssh_config"

// sshEgressProxyCommand is ssh's ProxyCommand through the guest's agent:
// the launcher's own, whatever the image's caboose-agent is.
func sshEgressProxyCommand(agent string) string { return agent + " connect %h %p" }

// sshEgressHosts is the drop-in's Host line, for the guest called
// hostname. What is the VM's own is left alone, as NO_PROXY leaves it
// (agentproto.EgressNoProxy), since the host would refuse it: its
// loopback, *.localhost, its hostname, and its dockerd's bridges, which
// ssh's patterns, matching the name as given and knowing no CIDR, say as
// 172.16.* to 172.31.*.
func sshEgressHosts(hostname string) string {
	pats := []string{"*", "!localhost", "!*.localhost", "!127.*", "!::1"}
	if plainHostname(hostname) {
		pats = append(pats, "!"+strings.ToLower(hostname))
	}
	for i := 16; i <= 31; i++ {
		pats = append(pats, "!172."+strconv.Itoa(i)+".*")
	}
	return "Host " + strings.Join(pats, " ")
}

// plainHostname reports whether s is a host name that may be written into
// a config as it is: letters, digits, '-' and '.', 1 to 253 bytes.
func plainHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// sshEgressDropIn is the drop-in's text, for the agent at agent, in the
// guest called hostname.
func sshEgressDropIn(agent, hostname string) string {
	return `# Written by caboose at every boot of the sandbox's VM, while egress_proxy
# is on: ssh connects through the host's caboose link, as everything else
# in the sandbox does, so the host's VPN routes and DNS apply. Your own
# ~/.ssh/config is read first: a ProxyCommand or ProxyJump of yours for a
# host, or "ProxyCommand none", wins over this.
` + sshEgressHosts(hostname) + `
    ProxyCommand ` + sshEgressProxyCommand(agent) + `
`
}

// sshVersionRE reads `ssh -V`: OpenSSH_9.2p1 Debian-2+deb12u3, ...
var sshVersionRE = regexp.MustCompile(`OpenSSH_(\d+)\.(\d+)(?:p\d+)?(?:[ \t]+([^ \t,]+))?`)

// sshBackports are distributions' builds of an OpenSSH before 9.6 known to
// carry CVE-2023-51385's fix, by version and the revision `ssh -V` shows:
// the release's first fixed build, and any later one.
var sshBackports = []struct {
	version string
	re      *regexp.Regexp // the revision, with the build's number
	from    int
}{
	{"9.2", regexp.MustCompile(`^Debian-2\+deb12u(\d+)$`), 2},  // bookworm, DSA-5586-1
	{"8.9", regexp.MustCompile(`^Ubuntu-3ubuntu0\.(\d+)$`), 6}, // jammy
}

// sshFixed reports whether `ssh -V`'s output v names an OpenSSH that
// refuses the host names CVE-2023-51385 needs, and if not, what it is.
func sshFixed(v string) (bool, string) {
	m := sshVersionRE.FindStringSubmatch(v)
	if m == nil {
		return false, "no OpenSSH version in ssh -V"
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	what := "OpenSSH " + m[1] + "." + m[2]
	if m[3] != "" {
		what += " (" + m[3] + ")"
	}
	if major > 9 || major == 9 && minor >= 6 {
		return true, what
	}
	for _, b := range sshBackports {
		if b.version != m[1]+"."+m[2] {
			continue
		}
		if r := b.re.FindStringSubmatch(m[3]); r != nil {
			if n, err := strconv.Atoi(r[1]); err == nil && n >= b.from {
				return true, what
			}
		}
	}
	return false, what
}

// sshVersion is what the image's ssh says to -V, found on env's PATH; ""
// when there is none.
func sshVersion(env []string) string {
	path := getenv(env, "PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	ssh, err := lookPath("ssh", path)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, ssh, "-V").CombinedOutput()
	if len(out) > 4096 {
		out = out[:4096]
	}
	return string(out)
}

// includesDropIns reports whether ssh_config's text includes
// ssh_config.d/*.conf, as an absolute path or relative to /etc/ssh.
func includesDropIns(conf []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(conf))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !strings.EqualFold(f[0], "Include") {
			continue
		}
		for _, p := range f[1:] {
			p = strings.Trim(p, `"`)
			if p == "ssh_config.d/*.conf" || p == "/etc/ssh/ssh_config.d/*.conf" {
				return true
			}
		}
	}
	return false
}

// sshEgress points ssh under root at the outbound proxy through agent, in
// the guest called hostname, when version (sshVersion's) is an OpenSSH
// that sshFixed takes: the drop-in when the system's config includes it,
// else GIT_SSH_COMMAND in env when env has none. It returns env, changed
// or not, and what it did or why not, for the console ("" for nothing).
func sshEgress(root string, env []string, agent, hostname, version string) ([]string, string, error) {
	if version == "" {
		return env, "", nil // no ssh to point anywhere
	}
	if ok, what := sshFixed(version); !ok {
		return env, fmt.Sprintf("ssh keeps the VM's own network, not the outbound proxy: %s is older than 9.6, "+
			"whose ProxyCommand a hostile host name could inject shell into (CVE-2023-51385); an image with a newer ssh goes through it", what), nil
	}
	conf, err := os.ReadFile(filepath.Join(root, sshSystemConf))
	if err == nil && includesDropIns(conf) {
		path := filepath.Join(root, SSHEgressConf)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return env, "", err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(sshEgressDropIn(agent, hostname)), 0o644); err != nil {
			return env, "", err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return env, "", err
		}
		return env, "ssh goes through the outbound proxy: wrote " + SSHEgressConf, nil
	}
	if getenv(env, "GIT_SSH_COMMAND") != "" {
		return env, "", nil
	}
	cmd := "GIT_SSH_COMMAND=ssh -o 'ProxyCommand=" + sshEgressProxyCommand(agent) + "'"
	return append(append([]string(nil), env...), cmd), "ssh goes through the outbound proxy: set GIT_SSH_COMMAND (" + sshSystemConf + " includes no ssh_config.d)", nil
}
