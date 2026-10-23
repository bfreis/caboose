package conformance

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/bfreis/caboose/internal/agentproto"
)

// The commands of the egress checks: a connection to a Target, through the
// outbound proxy or around it, and two looks at the guest's network. Each
// word is a literal the suite wrote, single-quoted where it is not a
// fixed flag: a Target's address (netip's text) and port, and a token of a
// fixed alphabet that the connection sends, which is how the suite's
// listener tells the check's own connection from any other. Every command
// sends its output to stderr, which the wrapper keeps.

// tokenRe is a token a connection sends.
var tokenRe = regexp.MustCompile(`^conf-[a-z0-9]{1,8}-[a-z0-9-]{1,24}$`)

// Token is the token of a check's attempt n.
func Token(check string, n int) (string, error) {
	t := fmt.Sprintf("conf-%s-%d", lowerID(check), n)
	if !tokenRe.MatchString(t) {
		return "", fmt.Errorf("conformance: %q is not a token", t)
	}
	return t, nil
}

func lowerID(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func targetAndToken(t Target, token string) error {
	if !t.valid() {
		return errors.New("conformance: a connection's target is a suite listener's")
	}
	if !tokenRe.MatchString(token) {
		return fmt.Errorf("conformance: %q is not a token", token)
	}
	return nil
}

// proxyURL is the sandbox's outbound proxy, as HTTP_PROXY names it.
const proxyURL = "http://" + agentproto.EgressListen

// AgentConnect connects to t through the outbound proxy with the agent's
// CONNECT client, sends token, and prints what comes back.
func AgentConnect(t Target, token string) (Cmd, error) {
	if err := targetAndToken(t, token); err != nil {
		return Cmd{}, err
	}
	return Cmd{body: fmt.Sprintf("echo '%s' | %s connect '%s' '%d' 1>&2", token, agentBin, t.Host(), t.port)}, nil
}

// CurlProxy asks the outbound proxy for http://t/token, as a client that
// HTTP_PROXY steers does; NO_PROXY is overridden, so that a loopback
// target goes through the proxy too.
func CurlProxy(t Target, token string) (Cmd, error) {
	if err := targetAndToken(t, token); err != nil {
		return Cmd{}, err
	}
	return Cmd{body: fmt.Sprintf("curl -g -sS -f --max-time 20 --noproxy '' --proxy '%s' 'http://%s/%s' 1>&2", proxyURL, t.hostPort(), token)}, nil
}

// CurlDirect asks for http://t/token around the proxy: straight from the
// guest's own network.
func CurlDirect(t Target, token string) (Cmd, error) {
	if err := targetAndToken(t, token); err != nil {
		return Cmd{}, err
	}
	return Cmd{body: fmt.Sprintf("curl -g -sS -f --max-time 10 --noproxy '%s' 'http://%s/%s' 1>&2", t.Host(), t.hostPort(), token)}, nil
}

// Ping sends one echo request to t's address, from the guest's own
// network (a raw socket: run as root).
func Ping(t Target) (Cmd, error) {
	if !t.valid() || t.name != "" {
		return Cmd{}, errors.New("conformance: a ping's target is a suite listener's address")
	}
	return Cmd{body: fmt.Sprintf("ping -c 1 -W 2 '%s' 1>&2", t.addr)}, nil
}

// Routes prints the guest's routes: IPv4's, then IPv6's (unless the
// kernel has no IPv6, which it says on a line of its own), every table
// when ip can list them all, the main one when it cannot (the first
// attempt's complaint discarded). It fails when either family's last
// attempt does.
func Routes() Cmd {
	return Cmd{body: `{ ip route show table all 2>/dev/null || ip route show; } 1>&2 && if [ -e /proc/net/if_inet6 ]; then { ip -6 route show table all 2>/dev/null || ip -6 route show; } 1>&2; else echo '` + NoIPv6 + `' 1>&2; fi`}
}

// NoIPv6 is the line Routes prints for a kernel without IPv6.
const NoIPv6 = "conformance: no ipv6"

// NetDevices prints each of the guest's network interfaces as "nic NAME"
// when the kernel has a device behind it (a virtio NIC: its
// /sys/class/net entry has a device link), else "virtual NAME" (lo, a
// bridge, a veth, a tunnel: made by the guest's own kernel, reaching
// nothing beyond the VM but through a NIC); then each virtio device the
// virtio_net driver has bound, "virtio-net NAME", which no network
// namespace hides. Shell builtins and test alone, so it needs no tool of
// the base's.
func NetDevices() Cmd {
	return Cmd{body: `for d in /sys/class/net/*; do if [ -e "$d/device" ]; then echo "nic ${d##*/}"; else echo "virtual ${d##*/}"; fi; done 1>&2; ` +
		`for v in /sys/bus/virtio/drivers/virtio_net/virtio*; do if [ -e "$v" ]; then echo "virtio-net ${v##*/}"; fi; done 1>&2`}
}
