package conformance

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/conformance/probe"
)

// The commands of the daemon, web UI and answer checks. As everywhere,
// every word is a literal the suite wrote, from the run's nonce and fixed
// text: canary paths in the sandbox's home (HPath), the probe's path and
// its fixed messages, URLs and hosts of a fixed alphabet, process ids the
// host read. The one exception is a socket path the sandbox listed
// (SockPath), which the socket check must hand to the probe: it is held to
// a strict alphabet and single-quoted, and the probe only dials it.

// HomeCanaryDir is where the sandbox sees the canaries of its home: its
// ~/.config/caboose, a directory the sandbox config always keeps, so a
// mount of the data dir's home/.config/caboose. The home itself is the
// image's: only what is kept of it is the data dir's.
const HomeCanaryDir = sandboxHome + "/.config/caboose"

// homeRelRe is the host-side path of a canary in the sandbox's home: in
// the data dir's home/.config/caboose (the data dir B/data, or the
// environment's default, home/envs/<env>/data).
var homeRelRe = regexp.MustCompile(`^(?:home/envs/` + EnvPattern + `/)?data/home/\.config/caboose/(conf-[0-9a-f]{8}-[a-z0-9-]+)$`)

// HPath is a canary in the sandbox's home, as the sandbox sees it.
type HPath struct{ p string }

func (p HPath) String() string { return p.p }

// HomePath is where the sandbox sees canary c, made in the data dir's
// home/.config/caboose.
func HomePath(c Canary) (HPath, error) {
	m := homeRelRe.FindStringSubmatch(c.Rel)
	if m == nil {
		return HPath{}, fmt.Errorf("conformance: canary %s is not in the sandbox's home", c.Rel)
	}
	return HPath{HomeCanaryDir + "/" + m[1]}, nil
}

var hpathRe = regexp.MustCompile(`^/home/agent/\.config/caboose/conf-[0-9a-f]{8}-[a-z0-9-]+$`)

func (p HPath) valid() bool { return hpathRe.MatchString(p.p) }

// ReadHome prints the home canary p on stderr.
func ReadHome(p HPath) (Cmd, error) {
	if !p.valid() {
		return Cmd{}, errors.New("conformance: not a canary of the sandbox's home")
	}
	return Cmd{body: "cat " + p.p + " 1>&2"}, nil
}

// AppendHome appends a line to the home canary p.
func AppendHome(p HPath) (Cmd, error) {
	if !p.valid() {
		return Cmd{}, errors.New("conformance: not a canary of the sandbox's home")
	}
	return Cmd{body: "echo x >> " + p.p}, nil
}

// nonceWordRe is the run's nonce as the commands carry it: 32 hex digits
// in a real run, its first eight and a model's suffix in the fake's.
var nonceWordRe = regexp.MustCompile(`^[0-9a-f]{8}[0-9a-z-]{0,40}$`)

// ScanComplete is the line a scan prints last when it went through.
func ScanComplete(n8 string) string { return "conf-" + n8 + "-scan-complete" }

// scanPrune leaves /proc, /sys and /dev out of a search of /.
const scanPrune = "find / '(' -path /proc -o -path /sys -o -path /dev ')' -prune -o"

// ScanMax is the most paths a scan prints. A scan prints at most one
// line more, so that a check can tell a search that found more than it
// may print, whose completion line the cap cut off, from one that did not
// go through: the roots alone hold some hundreds of the run's canaries.
const ScanMax = 4000

// ScanNames prints every path in the sandbox whose last component holds
// conf-<n8>-, /proc, /sys and /dev aside: at most 2,000,000 entries
// looked at, 60 seconds, ScanMax+1 lines printed. A path below one of
// the run's names is not printed itself, since that name is (every path
// under the roots would be, the roots being named so). The search's own
// line, ScanComplete, is printed only when find went through without an
// error; run it as root, so nothing is unreadable.
func ScanNames(n8 string) (Cmd, error) {
	if !n8Re.MatchString(n8) {
		return Cmd{}, errors.New("conformance: not a nonce")
	}
	return Cmd{body: fmt.Sprintf("{ timeout 60 %s -print 2>/dev/null && echo %s; } | head -n 2000000 | grep -x -E '.*conf-%s-[^/]*' | head -n %d 1>&2",
		scanPrune, ScanComplete(n8), n8, ScanMax+1)}, nil
}

// ScanContent prints every regular file under 10 MiB, in /work and the
// sandbox's home, that holds the nonce: the mounts that are writable by
// design. ScanComplete ends it, and ScanMax+1 lines bound it, as in
// ScanNames.
func ScanContent(n8, nonce string) (Cmd, error) {
	if !n8Re.MatchString(n8) || !nonceWordRe.MatchString(nonce) || !strings.HasPrefix(nonce, n8) {
		return Cmd{}, errors.New("conformance: not a nonce")
	}
	return Cmd{body: fmt.Sprintf("{ timeout 60 find /work %s -type f -size -10240k -exec grep -l -F -e %s '{}' ';' 2>/dev/null && echo %s; } | head -n %d 1>&2",
		sandboxHome, nonce, ScanComplete(n8), ScanMax+1)}, nil
}

// ListSockets prints the paths of the sandbox's unix sockets, /proc, /sys
// and /dev aside, at most 63, then ScanComplete when find went through.
// Run it as root.
func ListSockets(n8 string) (Cmd, error) {
	if !n8Re.MatchString(n8) {
		return Cmd{}, errors.New("conformance: not a nonce")
	}
	return Cmd{body: fmt.Sprintf("{ timeout 60 %s -type s -print 2>/dev/null && echo %s; } | head -n 64 1>&2", scanPrune, ScanComplete(n8))}, nil
}

// ProcNetUnix prints the unix sockets of the sandbox's network namespace.
func ProcNetUnix() Cmd { return Cmd{body: "head -n 2000 /proc/net/unix 1>&2"} }

// SockPath is a socket's path in the sandbox, as a listing showed it.
type SockPath struct{ p string }

func (s SockPath) String() string { return s.p }

var sockRe = regexp.MustCompile(`^(/[A-Za-z0-9._@+:-]{1,64}){1,12}$`)

// NewSockPath takes a socket path the sandbox listed: absolute, clean, at
// most 103 bytes (the socket path limit of the platforms), of a strict
// alphabet, no "." or ".." component.
func NewSockPath(s string) (SockPath, error) {
	if len(s) > 103 || !sockRe.MatchString(s) || path.Clean(s) != s {
		return SockPath{}, fmt.Errorf("conformance: %q is not a socket path the suite may pass on", s)
	}
	for _, seg := range strings.Split(s[1:], "/") {
		if seg == "." || seg == ".." {
			return SockPath{}, fmt.Errorf("conformance: %q is not a socket path the suite may pass on", s)
		}
	}
	return SockPath{s}, nil
}

// SocketHolders prints, for every descriptor of every process that is a
// socket, its link's text (socket:[INODE]) and on the next line its path
// (/proc/PID/fd/N), then ScanComplete and find's exit status (timeout's
// 124 when it was cut short): which process holds the socket
// /proc/net/unix gives an inode for. Run it as root.
func SocketHolders(n8 string) (Cmd, error) {
	if !n8Re.MatchString(n8) {
		return Cmd{}, errors.New("conformance: not a nonce")
	}
	mark := ScanComplete(n8)
	return Cmd{body: fmt.Sprintf("{ timeout 60 find /proc -mindepth 3 -maxdepth 3 -path '/proc/[0-9]*/fd/*' -type l -exec readlink '{}' ';' -print 2>/dev/null; echo %s $?; } | grep -F -A1 -e 'socket:[' -e %s | head -n %d 1>&2", mark, mark, MaxHolderLines)}, nil
}

// MaxHolderLines bounds SocketHolders' output.
const MaxHolderLines = 8000

// ProcExe prints what /proc/PID/exe links to. Run it as root.
func ProcExe(pid int) (Cmd, error) {
	if pid <= 0 || pid > 1<<22 {
		return Cmd{}, errors.New("conformance: not a pid")
	}
	return Cmd{body: fmt.Sprintf("readlink /proc/%d/exe 1>&2", pid)}, nil
}

// SocketGone is the line SameSocket prints when the path it compares is
// no socket (any more): the comparison then proves nothing.
const SocketGone = "conformance-socket-gone"

// MaxBoundSockets is the most socket paths SameSocket compares one with.
const MaxBoundSockets = 256

// SameSocket prints each of bound that is the very socket file at s: the
// same device and inode, by test's -ef (which follows the links on the
// way, as /var/run's to /run), when s is a socket, and SocketGone when it
// is not (gone since it was found). /proc/net/unix names a
// socket by the path it was bound at, and a search of the filesystem
// finds it where it is now: the two spellings of one socket are told
// apart from two sockets so. Every path is a literal; run it as root.
func SameSocket(s SockPath, bound []SockPath) (Cmd, error) {
	if s.p == "" || len(bound) == 0 || len(bound) > MaxBoundSockets {
		return Cmd{}, fmt.Errorf("conformance: a socket and 1 to %d others to compare it with", MaxBoundSockets)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "if test -S '%s'; then", s.p)
	for _, o := range bound {
		if o.p == "" {
			return Cmd{}, errors.New("conformance: an empty socket path")
		}
		fmt.Fprintf(&b, " test '%s' -ef '%s' && echo '%s';", s.p, o.p, o.p)
	}
	fmt.Fprintf(&b, " else echo '%s'; fi 1>&2; true", SocketGone)
	return Cmd{body: b.String()}, nil
}

// ProbeName is the probe binary's name in the writable part.
const ProbeName = ".conformance-probe"

// ProbeRel is the probe's host path, relative to B.
func ProbeRel(n8 string) (string, error) {
	if !n8Re.MatchString(n8) {
		return "", errors.New("conformance: not a nonce")
	}
	return "roots/conf-" + n8 + "-dev/part/" + ProbeName, nil
}

// IsProbeRel reports whether rel is the probe's host path in the run n8.
func IsProbeRel(n8, rel string) bool {
	p, err := ProbeRel(n8)
	return err == nil && rel == p
}

func probeCmd(n8, nonce string, link SockPath, rest string) (Cmd, error) {
	if !n8Re.MatchString(n8) || !nonceWordRe.MatchString(nonce) || !strings.HasPrefix(nonce, n8) {
		return Cmd{}, errors.New("conformance: not a nonce")
	}
	if link.p == "" {
		return Cmd{}, errors.New("conformance: the probe needs the link's socket")
	}
	return Cmd{body: fmt.Sprintf("/work/conf-%s-dev/part/%s --nonce %s --link '%s' %s --timeout 10 1>&2", n8, ProbeName, nonce, link.p, rest)}, nil
}

// mountPointRe is a mount point the probe's mount mode may name: a root
// of the run's, or its writable part.
var mountPointRe = regexp.MustCompile(`^/work/conf-([0-9a-f]{8})-(ro|dev(/part)?)$`)

// ProbeMount runs the probe's mount mode: the one call op on the mount
// point mp, a root of the run's or its writable part. It is destructive,
// as an unmount is.
func ProbeMount(nonce, op string, mp SPath) (Cmd, error) {
	if !nonceWordRe.MatchString(nonce) {
		return Cmd{}, errors.New("conformance: not a nonce")
	}
	m := mountPointRe.FindStringSubmatch(mp.p)
	if m == nil || !strings.HasPrefix(nonce, m[1]) {
		return Cmd{}, fmt.Errorf("conformance: %q is not a mount point of the run's", mp.p)
	}
	if !slices.Contains(probe.MountOps(), op) {
		return Cmd{}, fmt.Errorf("conformance: %q is not a mount operation of the probe's", op)
	}
	return Cmd{body: fmt.Sprintf("/work/conf-%s-dev/part/%s --nonce %s --mount %s --target %s 1>&2", m[1], ProbeName, nonce, op, mp.p), destructive: true}, nil
}

// MountTable prints the sandbox's /proc/self/mountinfo on stderr, at most
// 2000 lines.
func MountTable() Cmd { return Cmd{body: "head -n 2000 /proc/self/mountinfo 1>&2"} }

// ProbeUserNS runs the probe's user-namespace attempt: roRoot is a
// read-only root's mount point, part the writable part's, witness a
// read-only canary under roRoot. It is destructive (it unmounts).
func ProbeUserNS(nonce string, roRoot, part, witness SPath) (Cmd, error) {
	if !nonceWordRe.MatchString(nonce) {
		return Cmd{}, errors.New("conformance: not a nonce")
	}
	r := mountPointRe.FindStringSubmatch(roRoot.p)
	if r == nil || r[2] != "ro" || !strings.HasPrefix(nonce, r[1]) {
		return Cmd{}, fmt.Errorf("conformance: %q is not the run's read-only root", roRoot.p)
	}
	pm := mountPointRe.FindStringSubmatch(part.p)
	if pm == nil || pm[3] != "/part" || pm[1] != r[1] {
		return Cmd{}, fmt.Errorf("conformance: %q is not the run's writable part", part.p)
	}
	if !witness.valid() || !strings.HasPrefix(witness.p, roRoot.p+"/") {
		return Cmd{}, fmt.Errorf("conformance: %q is not a canary under the read-only root", witness.p)
	}
	return Cmd{body: fmt.Sprintf("/work/conf-%s-dev/part/%s --nonce %s --mount %s --target %s --part %s --witness %s 1>&2",
		r[1], ProbeName, nonce, probe.MountUserNS, roRoot.p, part.p, witness.p), destructive: true}, nil
}

// ProbeDaemon runs the probe's daemon-hello on the socket at daemon.
func ProbeDaemon(n8, nonce string, link, daemon SockPath) (Cmd, error) {
	if daemon.p == "" {
		return Cmd{}, errors.New("conformance: no socket to try")
	}
	return probeCmd(n8, nonce, link, "--daemon '"+daemon.p+"' --only "+probe.DaemonHello)
}

// ProbeDaemonNoHello runs the probe's daemon-no-hello on daemon: a
// request before any hello, which the daemon logs wherever it comes from.
func ProbeDaemonNoHello(n8, nonce string, link, daemon SockPath) (Cmd, error) {
	if daemon.p == "" {
		return Cmd{}, errors.New("conformance: no socket to try")
	}
	return probeCmd(n8, nonce, link, "--daemon '"+daemon.p+"' --only "+probe.DaemonNoHello)
}

// ProbeLink runs the probe's link messages only (its control first).
func ProbeLink(n8, nonce string, link SockPath, only []string) (Cmd, error) {
	known := probe.Messages()
	if len(only) == 0 {
		return Cmd{}, errors.New("conformance: no message to send")
	}
	for _, m := range only {
		if !slices.Contains(known, m) || m == probe.DaemonHello {
			return Cmd{}, fmt.Errorf("conformance: %q is not a link message of the probe's", m)
		}
	}
	return probeCmd(n8, nonce, link, "--only "+strings.Join(only, ","))
}

// urlRe is a URL a check asks the host to open: http, a host of a fixed
// alphabet (brackets for IPv6), an optional port, and a short path.
var urlRe = regexp.MustCompile(`^https?://[A-Za-z0-9.:\[\]-]{1,64}(/[A-Za-z0-9._#=/-]{0,32})?$`)

// AgentOpen asks the host to open u in its browser.
func AgentOpen(u string) (Cmd, error) {
	if !urlRe.MatchString(u) {
		return Cmd{}, fmt.Errorf("conformance: %q is not a URL the suite may send", u)
	}
	return Cmd{body: agentBin + " open '" + u + "'"}, nil
}

// AgentRequestAlias files a request for alias, with the text context,
// and prints the host's answer as JSON on stderr.
func AgentRequestAlias(alias, context string) (Cmd, error) {
	w, err := quoteWords([]string{alias, context})
	if err != nil {
		return Cmd{}, err
	}
	words := strings.SplitN(w, " ", 2)
	return Cmd{body: agentBin + " request alias --alias " + words[0] + " --context " + words[1] + " --json 1>&2"}, nil
}

// AgentRequests runs caboose-agent requests with args, which a check
// means to be refused.
func AgentRequests(args ...string) (Cmd, error) {
	w, err := quoteWords(args)
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{body: agentBin + " requests " + w}, nil
}

// TooMany is what the host says to a request over its rate.
const TooMany = "too many requests"

// Flood runs c n times at once and prints how many were refused for
// their rate. c is a caboose-agent command of the builders here.
func Flood(n int, c Cmd) (Cmd, error) {
	if n < 2 || n > 64 {
		return Cmd{}, errors.New("conformance: a flood is 2 to 64 commands")
	}
	if !c.valid() || c.destructive || !strings.HasPrefix(c.body, agentBin+" ") || strings.ContainsAny(strings.TrimSuffix(c.body, " 1>&2"), ";&|<>{}") {
		return Cmd{}, errors.New("conformance: only a caboose-agent command is flooded")
	}
	nums := make([]string, n)
	for i := range nums {
		nums[i] = fmt.Sprint(i + 1)
	}
	return Cmd{body: fmt.Sprintf("{ for i in %s; do %s & done; wait; } 2>&1 | grep -c -F '%s' 1>&2", strings.Join(nums, " "), c.body, TooMany)}, nil
}

func pidOK(pid int) bool { return pid > 0 && pid < 1<<22 }

// KillZero asks whether pid is a process the sandbox can signal.
func KillZero(pid int) (Cmd, error) {
	if !pidOK(pid) {
		return Cmd{}, errors.New("conformance: not a process id")
	}
	return Cmd{body: fmt.Sprintf("kill -0 %d", pid)}, nil
}

// KillTerm sends pid SIGTERM. The checks send it only to a pid the
// sandbox has no process of (KillZero said so), the host daemon's.
func KillTerm(pid int) (Cmd, error) {
	if !pidOK(pid) || pid == 1 {
		return Cmd{}, errors.New("conformance: not a process id the suite may signal")
	}
	return Cmd{body: fmt.Sprintf("kill -TERM %d", pid)}, nil
}

// ProcCmdline prints pid's command line on stderr.
func ProcCmdline(pid int) (Cmd, error) {
	if !pidOK(pid) {
		return Cmd{}, errors.New("conformance: not a process id")
	}
	return Cmd{body: fmt.Sprintf("cat /proc/%d/cmdline 1>&2", pid)}, nil
}

// The ways a check asks the web UI's API without the token.
const (
	UINoToken    = "no-token"
	UIWrongToken = "wrong-token"
	UIForeign    = "foreign-origin"
)

// UIHostNames are the names a sandbox may know the host by.
var UIHostNames = []string{"host.docker.internal", "host.internal"}

// UIHTTP is the line CurlUI prints with the HTTP status.
const UIHTTP = "http="

// CurlUI posts to the web UI's API at host (an address or one of
// UIHostNames) and port, as the page would but for the token, as how
// says. It prints UIHTTP and the status.
func CurlUI(host string, port int, how string) (Cmd, error) {
	hostPart := host
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Is6() && !a.Is4In6() {
			hostPart = "[" + a.String() + "]"
		} else {
			hostPart = a.Unmap().String()
		}
	} else if !slices.Contains(UIHostNames, host) {
		return Cmd{}, fmt.Errorf("conformance: %q is not a host the suite asks", host)
	}
	if port < 1 || port > 65535 {
		return Cmd{}, errors.New("conformance: not a port")
	}
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	extra := ""
	switch how {
	case UINoToken:
	case UIWrongToken:
		extra = " -H 'X-Caboose-Token: conf-wrong-token'"
	case UIForeign:
		origin = "http://conf-foreign.invalid"
		extra = " -H 'Host: conf-foreign.invalid'"
	default:
		return Cmd{}, fmt.Errorf("conformance: no way %q", how)
	}
	return Cmd{body: fmt.Sprintf("curl -g -sS -o /dev/null -w '%s%%{http_code}' --max-time 5 -X POST -H 'Origin: %s' -H 'Sec-Fetch-Site: same-origin' -H 'Content-Type: application/json'%s --data '{}' 'http://%s:%d/api/requests' 1>&2",
		UIHTTP, origin, extra, hostPart, port)}, nil
}
