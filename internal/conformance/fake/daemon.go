package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/probe"
)

// The daemon's mutants, which Daemon acts on beside DaemonDirMounted,
// SocketReachable, WebPortOpen and AnswerAccepted of the model's own list
// (and SandboxWritesConfig and TrustFromSandbox of the link's).
const (
	// EnvDirMounted mounts the environment's directory (config.toml,
	// grants.json, the checkpoint store) in the sandbox's home.
	EnvDirMounted Mutant = "daemon: env dir mounted"
	// OutsideMounted mounts the host directory outside every root at /mnt.
	OutsideMounted Mutant = "daemon: outside dir mounted"
	// CredsMounted mounts the stand-in credentials' directory at /mnt.
	CredsMounted Mutant = "daemon: creds dir mounted"
	// CanaryCopied puts a copy of the stand-in credentials, under another
	// name, in the sandbox's home: only its content tells.
	CanaryCopied Mutant = "daemon: host file copied into the home"
	// RootMountedTwice mounts the read-only root a second time, at /mnt.
	RootMountedTwice Mutant = "daemon: a root mounted twice"
	// PartDirInHome mounts a directory of the writable part that holds no
	// name of the run's below it (as a repository's .git does not) in the
	// home canaries' directory, under a name of the run's that no home
	// canary has: the name search prints the directory alone.
	PartDirInHome Mutant = "daemon: a part's directory mounted among the home canaries"
	// UIForwardAllowed loads forward_ports holding the web UI's port.
	UIForwardAllowed Mutant = "daemon: UI port forwardable"
	// UIURLAllowedInConfig takes an open_urls_allow naming the UI.
	UIURLAllowedInConfig Mutant = "daemon: UI origin allowed"
	// UIEgressAllowed takes an egress_allow naming the UI.
	UIEgressAllowed Mutant = "daemon: UI egress allowed"
	// UIPublishAllowed takes run_args publishing onto the UI's port.
	UIPublishAllowed Mutant = "daemon: UI port publishable"
	// NetworkHostRefused closes D3's gap: it is in no FAIL row.
	NetworkHostRefused Mutant = "daemon: --network host refused"
	// RequestTextAnswers allows a request whose own text says approved.
	RequestTextAnswers Mutant = "daemon: request text answers"
	// RequestsApproveFlag lets caboose-agent requests --approve answer.
	RequestsApproveFlag Mutant = "daemon: requests --approve answers"
	// ProposalAnswers lets a proposal answer a request.
	ProposalAnswers Mutant = "daemon: a proposal answers"
	// SandboxRootsApplied takes the sandbox config's roots into the next
	// launch's mounts.
	SandboxRootsApplied Mutant = "daemon: sandbox config roots applied"
	// UIURLOpens opens every URL to the UI.
	UIURLOpens Mutant = "daemon: UI URLs opened"
	// URLSpellingsMissed refuses only the plain spellings of loopback.
	URLSpellingsMissed Mutant = "daemon: loopback spellings missed"
	// URLRefusedButOpened opens a UI URL it reports as refused.
	URLRefusedButOpened Mutant = "daemon: UI URLs opened while refused"
	// RateLimitOff takes every request, however fast, and files each.
	RateLimitOff Mutant = "daemon: no rate limit"
	// FloodKillsLink leaves the link down after a flood.
	FloodKillsLink Mutant = "daemon: a flood kills the link"
	// FloodLeaksMemory grows the daemon's memory with every flood.
	FloodLeaksMemory Mutant = "daemon: a flood leaks memory"
	// FloodRestartsDaemon restarts the daemon under a flood.
	FloodRestartsDaemon Mutant = "daemon: a flood restarts the daemon"
	// DaemonPidVisible shares the host's pids with the sandbox.
	DaemonPidVisible Mutant = "daemon: the daemon's pid visible"
	// DaemonKillable lets a signal to the daemon's pid through, though
	// the sandbox sees no such process.
	DaemonKillable Mutant = "daemon: the daemon killable"
	// GrantedWhileRefused makes a grant on the probe's grant op, which
	// it reports as refused: only the host's grants tell.
	GrantedWhileRefused Mutant = "daemon: a refused grant op grants"
	// RootUnderHome mounts the read-only root a second time, in the
	// sandbox's home.
	RootUnderHome Mutant = "daemon: a root mounted in the home"
	// UIForwardDoctorOnly loads forward_ports holding the UI's port and
	// only doctor reports it.
	UIForwardDoctorOnly Mutant = "daemon: UI forward reported by doctor only"
	// RateLimitSilent drops a request over the rate without saying why.
	RateLimitSilent Mutant = "daemon: rate limit silent"
	// RequestAllowedNoGrant allows a pending request at the next Wait,
	// with no grant and no config change.
	RequestAllowedNoGrant Mutant = "daemon: request allowed without a grant"
	// TermSucceeds reports a signal to the daemon's pid as sent; the
	// daemon is untouched.
	TermSucceeds Mutant = "daemon: a signal to the daemon's pid succeeds"
	// ForeignSocketMounted mounts a host socket into the sandbox that
	// answers the daemon's hello in HTTP (a wrapper relaying the daemon,
	// say): found by the search, bound in no namespace of the sandbox's.
	ForeignSocketMounted Mutant = "daemon: a host HTTP socket mounted in"
	// HomeReadOnly closes X1's gap: it is in no FAIL row.
	HomeReadOnly Mutant = "daemon: home read-only"
	// BoundSocketIsDaemon binds a socket in the sandbox's own namespace
	// (listed in /proc/net/unix by name) that completes the daemon's
	// hello: a relay to it, bound in the guest, is the daemon however it
	// is bound.
	BoundSocketIsDaemon Mutant = "daemon: a socket bound in the sandbox answers the daemon's hello"
	// SilentRelayToDaemon binds a socket in the sandbox, held by a
	// process of the sandbox's, that relays to the daemon and says
	// nothing back: the daemon logs the hello it refused.
	SilentRelayToDaemon Mutant = "daemon: a silent relay in the sandbox reaches the daemon"
	// AgentRelayToDaemon is one more socket caboose-agent listens on, as
	// it does the SSH agent's, that reaches the daemon and says nothing
	// back: the daemon logs the hello it refused.
	AgentRelayToDaemon Mutant = "daemon: a relay of caboose-agent's reaches the daemon"
	// HelloRelayDropsReply binds a socket in the sandbox that relays to
	// the daemon, which takes its hello and answers it (logging nothing),
	// and drops the answer: only a connection the daemon refuses shows.
	HelloRelayDropsReply Mutant = "daemon: a relay in the sandbox drops the daemon's answer"
)

// DaemonMutants are the mutants Daemon acts on.
var DaemonMutants = []Mutant{
	DaemonDirMounted, SocketReachable, WebPortOpen, AnswerAccepted, SandboxWritesConfig, TrustFromSandbox,
	EnvDirMounted, OutsideMounted, CredsMounted, CanaryCopied, RootMountedTwice, PartDirInHome,
	UIForwardAllowed, UIURLAllowedInConfig, UIEgressAllowed, UIPublishAllowed, NetworkHostRefused,
	RequestTextAnswers, RequestsApproveFlag, ProposalAnswers, SandboxRootsApplied,
	UIURLOpens, URLSpellingsMissed, URLRefusedButOpened,
	RateLimitOff, FloodKillsLink, FloodLeaksMemory, FloodRestartsDaemon,
	DaemonPidVisible, DaemonKillable, GrantedWhileRefused, RootUnderHome, UIForwardDoctorOnly,
	RateLimitSilent, RequestAllowedNoGrant, TermSucceeds, ForeignSocketMounted, HomeReadOnly,
	BoundSocketIsDaemon, SilentRelayToDaemon, AgentRelayToDaemon, HelloRelayDropsReply,
}

// The daemon's faults: the setup's answers a check must take as ERROR or
// SKIP, never PASS.
const (
	// ScanCutShort: the searches never print that they went through.
	ScanCutShort Fault = "daemon: scan cut short"
	// DScanBlind: the search by name finds nothing.
	DScanBlind Fault = "daemon: scan blind"
	// ContentBlind: the search by content finds nothing.
	ContentBlind Fault = "daemon: content scan blind"
	// SocketsUnlisted: the socket listings find nothing.
	SocketsUnlisted Fault = "daemon: sockets unlisted"
	// ProbeMissing: the probe is not in the writable part.
	ProbeMissing Fault = "daemon: probe missing"
	// ProbeSwapped: the probe in the writable part is another binary than
	// the one the suite placed, as the sandbox can make it.
	ProbeSwapped Fault = "daemon: probe swapped"
	// ProbeUnproven: the probe can tell nothing of a socket.
	ProbeUnproven Fault = "daemon: probe unproven"
	// DaemonDown: the throwaway daemon is not running.
	DaemonDown Fault = "daemon: down"
	// UINotOwned: the web UI's port is another daemon's.
	UINotOwned Fault = "daemon: UI port not ours"
	// UIDown: the web UI does not serve its page.
	UIDown Fault = "daemon: UI down"
	// DCurlMissing: the sandbox has no curl.
	DCurlMissing Fault = "daemon: no curl"
	// UIRefusesAll: the launcher refuses every forward.
	UIRefusesAll Fault = "daemon: every forward refused"
	// UIRefusalUnnamed: the UI's refusals do not name it.
	UIRefusalUnnamed Fault = "daemon: UI refusal unnamed"
	// RequestNotFiled: a request is not filed (the alias is unknown).
	RequestNotFiled Fault = "daemon: request not filed"
	// OpenURLsAsk: the configuration's open_urls is ask.
	OpenURLsAsk Fault = "daemon: open_urls ask"
	// StubOpenerDeaf: the stub opener records nothing.
	StubOpenerDeaf Fault = "daemon: stub opener deaf"
	// MountsDrift: the next launch's mounts are not the running ones.
	MountsDrift Fault = "daemon: next mounts differ"
	// NoRoot: the sandbox's root cannot signal.
	NoRoot Fault = "daemon: no root"
	// SocketFindFails: the search for sockets fails; /proc/net/unix still
	// lists the link's socket.
	SocketFindFails Fault = "daemon: socket search fails"
	// ProbeControlFails: the probe's status message is not answered, and
	// it skips the rest.
	ProbeControlFails Fault = "daemon: probe control fails"
	// LinkOpsUnproven: the probe can tell nothing of the link's ops.
	LinkOpsUnproven Fault = "daemon: link ops unproven"
	// SocketFindCutShort: the search for sockets lists some, the link's
	// among them, and never says it went through.
	SocketFindCutShort Fault = "daemon: socket search cut short"
	// ProbeControlOnly: the probe's status message is not answered, and
	// the rest are reported held anyway.
	ProbeControlOnly Fault = "daemon: probe control alone fails"
	// HomeCanaryUnseen: reading the home canary finds no such file.
	HomeCanaryUnseen Fault = "daemon: home canary unseen"
	// HomeAppendUnseen: appending to the home canary finds no such file.
	HomeAppendUnseen Fault = "daemon: home canary unseen on append"
	// RSSUnreadable: the daemon's memory cannot be read.
	RSSUnreadable Fault = "daemon: rss unreadable"
	// SameSocketBlind: comparing a socket with those /proc/net/unix
	// names finds none the same, not even the link's own.
	SameSocketBlind Fault = "daemon: socket comparison blind"
	// SocketVanishes: every socket but the link's is gone by the time it
	// is compared with those /proc/net/unix names.
	SocketVanishes Fault = "daemon: socket gone before the comparison"
	// WitnessBlind: the daemon's log does not record a connection closed
	// without a hello.
	WitnessBlind Fault = "daemon: the daemon's log records no refusal"
	// HoldersUnlisted: the list of the processes holding sockets is
	// empty but for its last line.
	HoldersUnlisted Fault = "daemon: socket holders unlisted"
	// LogRotates: the daemon's log is moved aside while a socket is tried.
	LogRotates Fault = "daemon: the daemon's log rotates during a try"
	// HoldersCutShort: the list of the processes holding sockets is cut
	// short by its timeout.
	HoldersCutShort Fault = "daemon: socket holders cut short"
	// AgentExeImpostor: what holds the link's socket runs from
	// /tmp/caboose-agent, a binary of the agent's name but not at any of
	// its paths.
	AgentExeImpostor Fault = "daemon: the link's holder runs from another path"
	// AgentExeDeleted: caboose-agent's file is gone from its path while
	// it runs, so /proc/PID/exe reads it with " (deleted)".
	AgentExeDeleted Fault = "daemon: caboose-agent's file deleted while it runs"
)

// DaemonFaults are every daemon Fault.
var DaemonFaults = []Fault{ScanCutShort, DScanBlind, ContentBlind, SocketsUnlisted, ProbeMissing, ProbeSwapped, ProbeUnproven, DaemonDown,
	UINotOwned, UIDown, DCurlMissing, UIRefusesAll, UIRefusalUnnamed, RequestNotFiled, OpenURLsAsk,
	StubOpenerDeaf, MountsDrift, NoRoot, RSSUnreadable, SocketFindFails, ProbeControlFails, LinkOpsUnproven, HomeCanaryUnseen,
	SocketFindCutShort, ProbeControlOnly, HomeAppendUnseen, SameSocketBlind, SocketVanishes,
	WitnessBlind, HoldersUnlisted, LogRotates, HoldersCutShort, AgentExeImpostor, AgentExeDeleted}

const (
	fakeLinkSocket  = "/run/conf-link-fake.sock"
	fakeStaleSocket = "/tmp/conf-stale-fake.sock"
	fakeDaemonPID   = 4242
	fakeRSS         = 40 << 20
	bucketSize      = 5
)

// Daemon is a Link with the throwaway daemon, its broker, its web UI and
// the launcher's decisions on them: what the daemon checks talk to.
// Nothing in it runs anything.
type Daemon struct {
	*Link

	env      string
	pid      int
	started  int
	rss      int64
	tokens   int
	requests []conformance.RequestInfo
	contexts map[uint64]string
	grants   int
	opened   []string
	killed   bool
	linkDead bool
	refused  int    // the daemon log's refusals
	logSize  int    // and what it grew by
	logHead  string // its first line, once it rotated
}

// plainDir is the directory of the writable part, no canary, that
// PartDirInHome mounts.
const plainDir = "plain"

// DaemonDirs are the fake's directories of the daemon checks' canaries.
type DaemonDirs struct {
	Part, Home, Outside, Creds, Daemon, Env, Checkpoints string
}

// NewDaemon is a Daemon over a new Link: its directories made, and the
// probe in the writable part. The canaries are the caller's to make.
func NewDaemon(n8, isolation, trust string) (*Daemon, DaemonDirs, error) {
	l, err := NewLink(n8, isolation, trust)
	if err != nil {
		return nil, DaemonDirs{}, err
	}
	d, dirs := DaemonOn(l)
	return d, dirs, nil
}

// DaemonOn is a Daemon over an existing Link, whose files it shares: so a
// test can run the daemon checks in the world another group's checks left
// (the checkpoint checks', through Checkpoints.Link), as a run does.
func DaemonOn(l *Link) (*Daemon, DaemonDirs) {
	d := &Daemon{Link: l, env: conformance.EnvPrefix + l.n8, pid: fakeDaemonPID, started: 1, rss: fakeRSS,
		tokens: bucketSize, contexts: map[uint64]string{}}
	dirs := DaemonDirs{
		Part: l.Part(), Home: "data/home/.config/caboose", Outside: "canary/outside-root", Creds: "canary/creds",
		Daemon: "home/daemon", Env: "home/envs/" + d.env, Checkpoints: "home/envs/" + d.env + "/checkpoints",
	}
	l.mu.Lock()
	for _, p := range []string{"home", "home/daemon", "home/envs", dirs.Env, dirs.Checkpoints, "data", "data/home", "data/home/.config", dirs.Home, dirs.Outside, dirs.Creds} {
		if _, ok := l.nodes[p]; !ok {
			l.nodes[p] = l.newNode(kDir, "")
		}
	}
	// A directory of the part that is no canary, holding plain files.
	l.nodes[l.Part()+"/"+plainDir] = l.newNode(kDir, "")
	for i := 1; i <= 3; i++ {
		l.nodes[fmt.Sprintf("%s/%s/file-%d", l.Part(), plainDir, i)] = l.newNode(kFile, "plain\n")
	}
	probeNode := l.newNode(kFile, "probe")
	probeNode.mode = 0o755
	l.nodes[l.Part()+"/"+conformance.ProbeName] = probeNode
	l.probeSum = hashHex(probeNode.content)
	l.mu.Unlock()
	return d, dirs
}

// Env is a conformance.Env over the Daemon.
func (d *Daemon) Env(isolation, trust string) conformance.Env {
	return conformance.NewEnv(d, d, d, d, isolation, trust)
}

// SetFault turns fault f on; ProbeMissing takes the probe away.
func (d *Daemon) SetFault(f Fault) {
	d.Link.SetFault(f)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch f {
	case ProbeMissing:
		delete(d.nodes, d.Part()+"/"+conformance.ProbeName)
	case ProbeSwapped:
		if n := d.nodes[d.Part()+"/"+conformance.ProbeName]; n != nil {
			n.content = "another probe"
		}
	}
}

func (d *Daemon) fault(f Fault) bool { return d.faults[f] }
func (d *Daemon) mut(m Mutant) bool  { return d.mutants[m] }

// Opened are the URLs the stub opener recorded.
func (d *Daemon) Opened() []string { d.mu.Lock(); defer d.mu.Unlock(); return slices.Clone(d.opened) }

// visible are the sandbox paths a host path is seen at.
func (d *Daemon) visible(rel string) []string {
	var out []string
	at := func(hostDir, sandboxDir string) {
		if rel == hostDir {
			out = append(out, sandboxDir)
		} else if rest, ok := strings.CutPrefix(rel, hostDir+"/"); ok {
			out = append(out, sandboxDir+"/"+rest)
		}
	}
	if rest, ok := strings.CutPrefix(rel, "roots/"); ok && rest != "" {
		out = append(out, "/work/"+rest)
	}
	at(managedDir, "/etc/claude-code")
	at("data/home", "/home/agent")
	if d.mut(DaemonDirMounted) {
		at("home/daemon", "/home/agent/.caboose-daemon")
	}
	if d.mut(EnvDirMounted) {
		at("home/envs/"+d.env, "/home/agent/.caboose-env")
	}
	if d.mut(OutsideMounted) {
		at("canary/outside-root", "/mnt/outside")
	}
	if d.mut(CredsMounted) {
		at("canary/creds", "/mnt/creds")
	}
	if d.mut(RootMountedTwice) {
		at(d.RO(), "/mnt/again")
	}
	if d.mut(RootUnderHome) {
		at(d.RO(), "/home/agent/.again")
	}
	if d.mut(PartDirInHome) {
		at(d.Part()+"/"+plainDir, conformance.HomeCanaryDir+"/conf-"+d.n8+"-"+plainDir)
	}
	return out
}

func (d *Daemon) mounts() []conformance.Mount {
	ms := []conformance.Mount{
		{Source: "<B>/" + d.RO(), Target: "/work/conf-" + d.n8 + "-ro", ReadOnly: !d.mut(RootsWritable)},
		{Source: "<B>/" + d.Dev(), Target: "/work/conf-" + d.n8 + "-dev", ReadOnly: !d.mut(ParentWritable)},
		{Source: "<B>/" + d.Part(), Target: "/work/conf-" + d.n8 + "-dev/part"},
		{Source: "<B>/" + managedDir, Target: "/etc/claude-code", ReadOnly: !d.mut(ManagedWritable)},
		{Source: "<B>/data/home", Target: "/home/agent"},
	}
	for _, x := range []struct {
		m        Mutant
		src, dst string
	}{
		{DaemonDirMounted, "home/daemon", "/home/agent/.caboose-daemon"},
		{EnvDirMounted, "home/envs/" + d.env, "/home/agent/.caboose-env"},
		{OutsideMounted, "canary/outside-root", "/mnt/outside"},
		{CredsMounted, "canary/creds", "/mnt/creds"},
		{RootMountedTwice, d.RO(), "/mnt/again"},
		{RootUnderHome, d.RO(), "/home/agent/.again"},
		{PartDirInHome, d.Part() + "/" + plainDir, conformance.HomeCanaryDir + "/conf-" + d.n8 + "-" + plainDir},
	} {
		if d.mut(x.m) {
			ms = append(ms, conformance.Mount{Source: "<B>/" + x.src, Target: x.dst, ReadOnly: true})
		}
	}
	return ms
}

func (d *Daemon) Mounts() []conformance.Mount {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mounts()
}

// Exec runs the daemon checks' commands in the model and hands the rest
// to the Link.
func (d *Daemon) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if rc, o, e, ok := d.agentShut(user); ok {
		return rc, o, e, nil
	}
	body, ok := conformance.Unwrap(argv)
	if !ok || !d.mine(body) {
		return d.Link.Exec(ctx, user, argv, stdin)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down || d.absent || d.stopped {
		return -1, "", "", errors.New("the sandbox is not running")
	}
	if d.shellNo {
		return 127, "", "exec: the shell is not there: no such file or directory", nil
	}
	d.event("exec")
	var out []string
	rc := d.run(user, body, &out)
	return 0, fmt.Sprintf("\nrc=%d\n", rc), strings.Join(out, "\n"), nil
}

var daemonForms = []string{
	"{ timeout 60 find ", "timeout 60 find / ", "head -n 2000 /proc/net/unix", "/work/conf-", "if test -S '",
	"caboose-agent open ", "caboose-agent request alias ", "caboose-agent requests ", "caboose-agent notify ",
	"{ for i in ", "kill -", "cat /proc/", "readlink /proc/", "curl -g -sS -o /dev/null", "cat " + conformance.HomeCanaryDir + "/conf-", "echo x >> " + conformance.HomeCanaryDir + "/conf-",
}

func (d *Daemon) mine(body string) bool {
	return slices.ContainsFunc(daemonForms, func(p string) bool { return strings.HasPrefix(body, p) })
}

func (d *Daemon) run(user, body string, out *[]string) int {
	say := func(rc int, format string, a ...any) int {
		*out = append(*out, fmt.Sprintf(format, a...))
		return rc
	}
	switch {
	case strings.HasPrefix(body, "{ timeout 60 find / ") && strings.Contains(body, "-print 2>/dev/null && echo") && !strings.Contains(body, "-type s"):
		var found []string
		rc := d.scanNames(body, &found)
		*out = append(*out, pipeCap(body, found)...)
		return rc
	case strings.HasPrefix(body, "{ timeout 60 find /work /home/agent -type f"):
		var found []string
		rc := d.scanContent(&found)
		*out = append(*out, pipeCap(body, found)...)
		return rc
	case strings.HasPrefix(body, "{ timeout 60 find / ") && strings.Contains(body, "-type s"):
		if d.fault(SocketFindFails) {
			return 0
		}
		if !d.fault(SocketsUnlisted) {
			*out = append(*out, d.sockets()...)
		}
		if !d.fault(SocketFindCutShort) {
			*out = append(*out, conformance.ScanComplete(d.n8))
		}
		return 0
	case strings.HasPrefix(body, "{ timeout 60 find /proc -mindepth 3 "):
		if !d.fault(HoldersUnlisted) {
			for i, h := range d.fakeHolders() {
				*out = append(*out, "socket:["+h[1]+"]", "/proc/"+h[0]+"/fd/"+strconv.Itoa(3+i), "--")
			}
		}
		if d.fault(HoldersCutShort) {
			*out = append(*out, conformance.ScanComplete(d.n8)+" 124")
			return 0
		}
		*out = append(*out, conformance.ScanComplete(d.n8)+" 1")
		return 0
	case strings.HasPrefix(body, "readlink /proc/") && strings.HasSuffix(body, "/exe 1>&2"):
		pid := strings.TrimSuffix(strings.TrimPrefix(body, "readlink /proc/"), "/exe 1>&2")
		if e, ok := d.fakeExe(pid); ok {
			*out = append(*out, e)
			return 0
		}
		*out = append(*out, "readlink: /proc/"+pid+"/exe: No such file or directory")
		return 1
	case body == "head -n 2000 /proc/net/unix 1>&2":
		*out = append(*out, "Num       RefCount Protocol Flags    Type St Inode Path")
		if !d.fault(SocketsUnlisted) {
			*out = append(*out, "0000000000000000: 00000002 00000000 00010000 0001 01 12345 "+fakeLinkSocket,
				"0000000000000000: 00000002 00000000 00010000 0001 01 12346")
			// dockerd binds /var/run/docker.sock, its default, and the
			// listing names a socket by the path it was bound at; the
			// search finds it at /run/docker.sock, /var/run being a link
			// to /run in the guest's root.
			if d.isolate == "vm" {
				*out = append(*out, "0000000000000000: 00010000 00000000 00010000 0001 01 12347 "+fakeGuestDockerBound,
					"0000000000000000: 00010000 00000000 00010000 0001 01 12348 "+fakeGuestTTRPCBound)
			}
			if d.isolate == "vm" {
				*out = append(*out, "0000000000000000: 00010000 00000000 00010000 0001 01 12350 "+fakeSSHRelay,
					// a connection accepted on the SSH agent's socket
					// shows its path, not listening
					"0000000000000000: 00000003 00000000 00000000 0001 03 12360 "+fakeSSHRelay)
			}
			for _, x := range fakeExtraSockets {
				if d.mut(x.m) {
					*out = append(*out, "0000000000000000: 00010000 00000000 00010000 0001 01 "+x.inode+" "+x.path)
				}
			}
		}
		return 0
	case strings.HasPrefix(body, "/work/conf-"):
		return d.probe(body, out)
	case strings.HasPrefix(body, "if test -S '"):
		return d.sameSocket(body, out)
	case strings.HasPrefix(body, "{ for i in "):
		return d.flood(user, body, out)
	case strings.HasPrefix(body, "curl "):
		return d.curl(tokens(body), out)
	}
	a := tokens(body)
	switch {
	case len(a) == 3 && a[0] == "kill":
		return d.kill(user, a[1], a[2], out)
	case len(a) == 3 && a[0] == "cat" && a[2] == "1>&2" && strings.HasPrefix(a[1], "/proc/"):
		if d.mut(DaemonPidVisible) && a[1] == fmt.Sprintf("/proc/%d/cmdline", d.pid) {
			return say(0, "caboose\x00daemon\x00")
		}
		return say(1, "cat: %s: No such file or directory", a[1])
	case len(a) == 3 && a[0] == "cat" && a[2] == "1>&2":
		rel, ok := d.homeRel(a[1])
		if d.fault(HomeCanaryUnseen) {
			ok = false
		}
		if n := d.nodes[rel]; ok && n != nil && n.kind == kFile && !d.mut(HomeReadOnly) {
			return say(0, "%s", strings.TrimSuffix(n.content, "\n"))
		}
		if ok && d.mut(HomeReadOnly) {
			return say(1, "cat: %s: Permission denied", a[1])
		}
		return say(1, "cat: %s: No such file or directory", a[1])
	case len(a) == 4 && a[0] == "echo" && a[2] == ">>":
		rel, ok := d.homeRel(a[3])
		n := d.nodes[rel]
		if d.fault(HomeAppendUnseen) {
			ok = false
		}
		switch {
		case !ok || n == nil:
			return say(2, "%s", cantCreate(a[3], eNoent))
		case d.mut(HomeReadOnly):
			return say(2, "%s", cantCreate(a[3], eAcces))
		}
		n.content += a[1] + "\n"
		d.touchNode(n)
		d.logMut("append", rel)
		return 0
	case len(a) >= 2 && a[0] == "caboose-agent":
		return d.agent(a[1:], out)
	}
	return say(127, "sh: 1: %s: not found", a[0])
}

func (d *Daemon) homeRel(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, conformance.HomeCanaryDir+"/")
	if !ok || strings.Contains(rest, "/") || !strings.HasPrefix(rest, "conf-"+d.n8+"-") {
		return "", false
	}
	return "data/home/.config/caboose/" + rest, true
}

// inHidden reports whether rel is a hidden canary's path or below one:
// a canary that is missing is not there for the scans to find either.
func (d *Daemon) inHidden(rel string) bool {
	for role := range d.hidden {
		if c, ok := d.canary[role]; ok && (rel == c.Rel || strings.HasPrefix(rel, c.Rel+"/")) {
			return true
		}
	}
	return false
}

// pipeCap is what a scan's last stage, | head -n N 1>&2, lets through
// of out: its first N lines, as the real pipeline does, so that a search
// finding more than it may print loses its completion line here too.
func pipeCap(body string, out []string) []string {
	m := headCapRe.FindStringSubmatch(body)
	if m == nil {
		return out
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || len(out) <= n {
		return out
	}
	return out[:n]
}

var headCapRe = regexp.MustCompile(`\| head -n ([0-9]+) 1>&2$`)

// scanNames is find's walk filtered as the body's grep says: a grep -F
// keeps every path holding the run's names anywhere, a grep -x -E of
// .*conf-<n8>-[^/]* only those whose last component holds them.
func (d *Daemon) scanNames(body string, out *[]string) int {
	if d.fault(DScanBlind) {
		*out = append(*out, conformance.ScanComplete(d.n8))
		return 0
	}
	name := "conf-" + d.n8 + "-"
	var keep func(string) bool
	switch {
	case strings.Contains(body, "| grep -F "+name+" |"):
		keep = func(p string) bool { return strings.Contains(p, name) }
	case strings.Contains(body, "| grep -x -E '.*"+name+"[^/]*' |"):
		keep = func(p string) bool { return strings.Contains(path.Base(p), name) }
	default:
		*out = append(*out, "grep: the fake knows no such filter")
		return 2
	}
	for _, rel := range d.sortedKeys() {
		if d.inHidden(rel) {
			continue
		}
		for _, p := range d.visible(rel) {
			if keep(p) {
				*out = append(*out, p)
			}
		}
	}
	if !d.fault(ScanCutShort) {
		*out = append(*out, conformance.ScanComplete(d.n8))
	}
	return 0
}

func (d *Daemon) scanContent(out *[]string) int {
	if !d.fault(ContentBlind) {
		for _, rel := range d.sortedKeys() {
			n := d.nodes[rel]
			if n.kind != kFile || !strings.Contains(n.content, d.nonce) || d.inHidden(rel) {
				continue
			}
			for _, p := range d.visible(rel) {
				if strings.HasPrefix(p, "/work/") || strings.HasPrefix(p, "/home/agent/") {
					*out = append(*out, p)
				}
			}
		}
		if d.mut(CanaryCopied) {
			*out = append(*out, "/home/agent/.cache/copy")
		}
	}
	if !d.fault(ScanCutShort) {
		*out = append(*out, conformance.ScanComplete(d.n8))
	}
	return 0
}

// daemonSocket is where a mutant shows the daemon's socket.
func (d *Daemon) daemonSocket() string {
	switch {
	case d.mut(DaemonDirMounted):
		return "/home/agent/.caboose-daemon/daemon.sock"
	case d.mut(SocketReachable):
		return "/run/caboose-daemon.sock"
	}
	return ""
}

// fakeGuestDocker is the docker socket a vm guest serves itself: its own
// dockerd's, which answers the daemon's hello in HTTP.
const fakeGuestDocker = "/run/docker.sock"

// fakeGuestDockerBound is the path the guest's dockerd bound its socket
// at, as /proc/net/unix names it.
const fakeGuestDockerBound = "/var/run/docker.sock"

// fakeGuestTTRPC is the guest's containerd's ttrpc socket, which takes a
// connection and says nothing to the daemon's hello; fakeGuestTTRPCBound
// is where containerd bound it, as /proc/net/unix names it.
const (
	fakeGuestTTRPC      = "/run/docker/containerd/containerd.sock.ttrpc"
	fakeGuestTTRPCBound = "/var/run/docker/containerd/containerd.sock.ttrpc"
)

// fakeMountedHTTP is ForeignSocketMounted's socket.
const fakeMountedHTTP = "/run/host-http.sock"

// fakeBoundRelay is BoundSocketIsDaemon's socket.
const fakeBoundRelay = "/run/guest-relay.sock"

// fakeSSHRelay is where caboose-agent serves the host's SSH agent under
// vm; fakeAgentPID is caboose-agent's pid.
const (
	fakeSSHRelay = "/ssh-agent.sock"
	fakeAgentPID = 100
)

// fakeExtraSockets are the mutants' sockets bound in the sandbox: their
// inodes, and the pid that holds each.
var fakeExtraSockets = []struct {
	m           Mutant
	path, inode string
	pid         int
}{
	{BoundSocketIsDaemon, fakeBoundRelay, "12349", 300},
	{SilentRelayToDaemon, "/run/guest-silent-relay.sock", "12351", 300},
	{AgentRelayToDaemon, "/run/conf-agent-relay.sock", "12352", fakeAgentPID},
	{HelloRelayDropsReply, fakeDropRelay, "12353", 300},
}

// fakeDropRelay is HelloRelayDropsReply's socket.
const fakeDropRelay = "/run/guest-drop-relay.sock"

// relaysToDaemon: a mutant's relay that says nothing back, and whose
// every connection the daemon refuses.
func (d *Daemon) relaysToDaemon(s string) bool {
	return d.mut(SilentRelayToDaemon) && s == "/run/guest-silent-relay.sock" || d.mut(AgentRelayToDaemon) && s == "/run/conf-agent-relay.sock"
}

// logRefusal is a line the daemon logs for a connection it refused.
func (d *Daemon) logRefusal() {
	d.refused++
	d.logSize += 60
}

// rotateLog moves the daemon's log aside, as cappedlog does at its cap:
// the refusals of the copy moved aside before it go.
func (d *Daemon) rotateLog() {
	d.refused = 0
	d.logSize = 80
	d.logHead = "2026/10/20 10:00:09 caboose daemon: daemon.log reached 10 MiB: the lines before are in daemon.log.1"
}

// fakeHolders are the sockets' holders, as /proc/PID/fd shows them: pid
// and inode. fakeExes are their executables.
func (d *Daemon) fakeHolders() [][2]string {
	out := [][2]string{{"100", "12345"}, {"100", "12361"}}
	if d.isolate == "vm" {
		out = append(out, [2]string{"200", "12347"}, [2]string{"201", "12348"}, [2]string{"100", "12350"}, [2]string{"100", "12360"})
	}
	for _, x := range fakeExtraSockets {
		if d.mut(x.m) {
			out = append(out, [2]string{strconv.Itoa(x.pid), x.inode})
		}
	}
	return out
}

var fakeExes = map[string]string{"100": "/usr/local/bin/caboose-agent", "200": "/usr/bin/dockerd", "201": "/usr/bin/containerd", "300": "/usr/local/bin/conf-relay"}

// fakeExe is what /proc/PID/exe reads as. Under vm caboose-agent runs
// from where the guest's init copied itself (agentproto.GuestAgent), as
// the guest's agent and the link it starts do, not from the layer's
// path; with AgentExeDeleted that file is gone from its path, and with
// AgentExeImpostor the link's holder is a binary named caboose-agent
// elsewhere.
func (d *Daemon) fakeExe(pid string) (string, bool) {
	e, ok := fakeExes[pid]
	if pid == strconv.Itoa(fakeAgentPID) {
		if d.isolate == "vm" {
			e = agentproto.GuestAgent
		}
		switch {
		case d.fault(AgentExeImpostor):
			e = "/tmp/caboose-agent"
		case d.fault(AgentExeDeleted):
			e += " (deleted)"
		}
	}
	return e, ok
}

func (d *Daemon) DaemonLog() (conformance.DaemonLogMark, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	head := d.logHead
	if head == "" {
		head = "2026/10/20 10:00:00 caboose daemon v1 started (pid 4242, protocol 3, idle exit after 1m0s)"
	}
	return conformance.DaemonLogMark{Refusals: d.refused, Size: 100 + d.logSize, Head: head}, nil
}

func (d *Daemon) HostSilent(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.fault(WitnessBlind) {
		d.logRefusal()
	}
	return nil
}

func (d *Daemon) Relays() map[string]string {
	return map[string]string{fakeSSHRelay: "the host's SSH agent"}
}

var (
	sameSocketS  = regexp.MustCompile(`^if test -S '([^']+)'; then`)
	sameSocketEF = regexp.MustCompile(` test '([^']+)' -ef '([^']+)' && echo '([^']+)';`)
)

// sockID is the identity of the socket file at p in the fake's sandbox,
// as stat sees it (/var/run is a link to /run), or "" for none: what a
// connection to p reaches, and what test -ef compares.
func (d *Daemon) sockID(p string) string {
	if rest, ok := strings.CutPrefix(p, "/var/run/"); ok {
		p = "/run/" + rest
	}
	if slices.Contains(d.sockets(), p) {
		return "file:" + p
	}
	return ""
}

// sameSocket answers SameSocket: each bound path that is the same file
// as the socket at s.
func (d *Daemon) sameSocket(body string, out *[]string) int {
	m := sameSocketS.FindStringSubmatch(body)
	if m == nil {
		*out = append(*out, "sh: syntax error")
		return 2
	}
	id := d.sockID(m[1])
	if d.fault(SocketVanishes) && m[1] != fakeLinkSocket {
		id = ""
	}
	if id == "" {
		*out = append(*out, conformance.SocketGone)
		return 0
	}
	if d.fault(SameSocketBlind) {
		return 0
	}
	for _, x := range sameSocketEF.FindAllStringSubmatch(body, -1) {
		if x[1] == m[1] && x[2] == x[3] && d.sockID(x[2]) == id {
			*out = append(*out, x[3])
		}
	}
	return 0
}

func (d *Daemon) sockets() []string {
	out := []string{fakeLinkSocket, fakeStaleSocket}
	if d.isolate == "vm" {
		out = append(out, fakeGuestDocker, fakeGuestTTRPC)
	}
	if d.isolate == "vm" {
		out = append(out, fakeSSHRelay)
	}
	for _, x := range fakeExtraSockets {
		if d.mut(x.m) {
			out = append(out, x.path)
		}
	}
	if d.mut(ForeignSocketMounted) {
		out = append(out, fakeMountedHTTP)
	}
	if s := d.daemonSocket(); s != "" {
		out = append(out, s)
	}
	return out
}

// probeLine is one line of the probe's output.
func probeLine(o probe.Outcome) string {
	b, _ := json.Marshal(o)
	return string(b)
}

func (d *Daemon) probe(body string, out *[]string) int {
	a := tokens(body)
	if a[0] != "/work/conf-"+d.n8+"-dev/part/"+conformance.ProbeName || d.nodes[d.Part()+"/"+conformance.ProbeName] == nil {
		*out = append(*out, "sh: 1: "+a[0]+": not found")
		return 127
	}
	flags := map[string]string{}
	for i := 1; i+1 < len(a); i += 2 {
		flags[a[i]] = a[i+1]
	}
	if flags["--nonce"] != d.nonce || flags["--link"] != fakeLinkSocket || flags["--only"] == "" {
		*out = append(*out, "conformance-probe: the gate refused")
		return probe.ExitGate
	}
	var outs []probe.Outcome
	if flags["--only"] == probe.DaemonHello {
		o := probe.Outcome{Message: probe.DaemonHello, Outcome: probe.Unreachable, Verdict: probe.Unproven, Detail: "no socket there"}
		switch s := flags["--daemon"]; {
		case d.fault(ProbeUnproven):
		case s != "" && s == d.daemonSocket(), d.mut(BoundSocketIsDaemon) && s == fakeBoundRelay:
			o = probe.Outcome{Message: probe.DaemonHello, Outcome: probe.Accepted, Verdict: probe.NotHeld, Detail: "connected; a daemon hello"}
		case s == fakeSSHRelay && d.isolate == "vm":
			// the host's SSH agent closes on a JSON hello
			o = probe.Outcome{Message: probe.DaemonHello, Outcome: probe.Closed, Verdict: probe.NotHeld, Detail: "connected; EOF"}
		case d.relaysToDaemon(s):
			o = probe.Outcome{Message: probe.DaemonHello, Outcome: probe.Closed, Verdict: probe.NotHeld, Detail: "connected; EOF"}
			d.logRefusal()
		case d.mut(HelloRelayDropsReply) && s == fakeDropRelay:
			// the daemon took the hello and answered; the relay kept the
			// answer: nothing logged
			o = probe.Outcome{Message: probe.DaemonHello, Outcome: probe.Closed, Verdict: probe.NotHeld, Detail: "connected; EOF"}
		case d.sockID(s) == "file:"+fakeGuestTTRPC && d.isolate == "vm":
			o = probe.Outcome{Message: probe.DaemonHello, Outcome: probe.Closed, Verdict: probe.NotHeld, Detail: "connected; read unix @->" + fakeGuestTTRPCBound + ": i/o timeout"}
		case s == fakeStaleSocket:
			o.Verdict, o.Detail = probe.Held, "connection refused"
		case d.sockID(s) == "file:"+fakeGuestDocker && d.isolate == "vm", d.sockID(s) == "file:"+fakeMountedHTTP:
			o = probe.Outcome{Message: probe.DaemonHello, Outcome: probe.Foreign, Verdict: probe.Held, Detail: "connected; answered in another protocol: HTTP/1.1 400 Bad Request"}
		}
		outs = append(outs, o)
	} else if flags["--only"] == probe.DaemonNoHello {
		o := probe.Outcome{Message: probe.DaemonNoHello, Outcome: probe.Unreachable, Verdict: probe.Unproven, Detail: "no socket there"}
		switch s := flags["--daemon"]; {
		case d.relaysToDaemon(s), d.mut(HelloRelayDropsReply) && s == fakeDropRelay:
			o = probe.Outcome{Message: probe.DaemonNoHello, Outcome: probe.Closed, Verdict: probe.Held, Detail: "connected; EOF"}
			d.logRefusal()
		case s == fakeSSHRelay && d.isolate == "vm", d.sockID(s) == "file:"+fakeGuestTTRPC && d.isolate == "vm":
			o = probe.Outcome{Message: probe.DaemonNoHello, Outcome: probe.Closed, Verdict: probe.Held, Detail: "connected; EOF"}
			if d.fault(LogRotates) {
				d.rotateLog()
			}
		}
		outs = append(outs, o)
	} else {
		ctl := probe.Outcome{Message: probe.Control, Outcome: probe.Refused, Verdict: probe.Held, Detail: "ok"}
		if d.fault(ProbeControlFails) || d.fault(ProbeControlOnly) {
			ctl.Outcome, ctl.Verdict, ctl.Detail = probe.NoHost, probe.Unproven, "the host link is down"
		}
		outs = append(outs, ctl)
		for _, m := range strings.Split(flags["--only"], ",") {
			o := probe.Outcome{Message: m, Outcome: probe.Refused, Verdict: probe.Held, Detail: "unknown request"}
			switch {
			case d.fault(ProbeControlFails):
				o.Outcome, o.Verdict, o.Detail = probe.Skipped, probe.Unproven, "the positive control failed"
			case d.fault(LinkOpsUnproven):
				o.Outcome, o.Verdict, o.Detail = probe.RateLimited, probe.Unproven, "too many requests"
			}
			if m == "grant" && d.mut(GrantedWhileRefused) {
				d.grants++
			}
			if m == "answer" && d.mut(AnswerAccepted) {
				o.Outcome, o.Verdict, o.Detail = probe.Accepted, probe.NotHeld, "ok"
				d.answerNewest()
			}
			outs = append(outs, o)
		}
	}
	for _, o := range outs {
		*out = append(*out, probeLine(o))
	}
	return probe.Exit(outs)
}

func (d *Daemon) answerNewest() {
	for i := len(d.requests) - 1; i >= 0; i-- {
		if d.requests[i].State == "pending" {
			d.requests[i].State = "allowed"
			d.grants++
			return
		}
	}
}

// take is a request through the link's bucket.
func (d *Daemon) take() bool {
	if d.mut(RateLimitOff) {
		return true
	}
	if d.tokens == 0 {
		return false
	}
	d.tokens--
	return true
}

func (d *Daemon) agent(a []string, out *[]string) int {
	say := func(rc int, format string, args ...any) int {
		*out = append(*out, fmt.Sprintf(format, args...))
		return rc
	}
	if d.linkDown || d.linkDead {
		return say(126, "caboose-agent: %s: no host is linked", a[0])
	}
	switch a[0] {
	case "notify":
		if !d.take() {
			if d.mut(RateLimitSilent) {
				return say(1, "caboose-agent: dropped")
			}
			return say(1, "caboose-agent: %s; try again shortly", conformance.TooMany)
		}
		d.notify()
		return 0
	case "open":
		if len(a) != 2 {
			return say(2, "usage")
		}
		return d.open(a[1], out)
	case "requests":
		if d.mut(RequestsApproveFlag) && len(a) == 3 && a[1] == "--approve" {
			id, _ := strconv.ParseUint(a[2], 10, 64)
			d.setState(id, "allowed")
			return 0
		}
		return say(2, "caboose-agent: requests: unknown flag %s", a[1])
	case "request":
		return d.request(a[1:], out)
	}
	return say(2, "usage")
}

func (d *Daemon) setState(id uint64, st string) {
	for i := range d.requests {
		if d.requests[i].ID == id && d.requests[i].State == "pending" {
			d.requests[i].State = st
			if st == "allowed" {
				d.grants++
			}
		}
	}
}

// uiSpelled reports whether u names the web UI, and whether only the
// plain spellings a broken check would know.
func uiSpelled(u string) (ui, plain bool) {
	for _, p := range []string{":7171", ":7172"} {
		if strings.Contains(u, p) {
			ui = true
		}
	}
	for _, h := range []string{"//127.0.0.1:", "//localhost:", "//[::1]:"} {
		if strings.Contains(u, h) {
			plain = true
		}
	}
	return ui, plain
}

func (d *Daemon) open(u string, out *[]string) int {
	record := func() {
		if d.stubsMissing {
			d.leaked++
			return
		}
		d.stubCalls++
		if !d.fault(StubOpenerDeaf) {
			d.opened = append(d.opened, u)
		}
	}
	if !d.take() {
		if d.mut(RateLimitSilent) {
			*out = append(*out, "caboose-agent: dropped")
			return 1
		}
		*out = append(*out, "caboose-agent: "+conformance.TooMany+"; try again shortly")
		return 1
	}
	ui, plain := uiSpelled(u)
	refused := ui && !d.mut(UIURLOpens) && !(d.mut(URLSpellingsMissed) && !plain)
	if refused {
		if d.mut(URLRefusedButOpened) {
			record()
		}
		*out = append(*out, "caboose-agent: refused: "+u+" is the host's caboose web UI")
		return 1
	}
	if d.fault(OpenURLsAsk) {
		*out = append(*out, "caboose-agent: pending #1: opening a URL asks")
		return 1
	}
	record()
	return 0
}

func (d *Daemon) request(a []string, out *[]string) int {
	flags := map[string]string{}
	for i := 0; i+1 < len(a); i++ {
		if strings.HasPrefix(a[i], "--") && a[i] != "--json" {
			flags[a[i]] = a[i+1]
			i++
		}
	}
	reply := func(rc int, r map[string]any) int {
		b, _ := json.Marshal(r)
		*out = append(*out, string(b))
		return rc
	}
	if !d.take() {
		if d.mut(RateLimitSilent) {
			return reply(2, map[string]any{"error": "dropped"})
		}
		return reply(2, map[string]any{"error": conformance.TooMany + "; try again shortly"})
	}
	alias, text := flags["--alias"], flags["--context"]
	if alias != conformance.AliasAsk || d.fault(RequestNotFiled) {
		return reply(77, map[string]any{"verdict": "denied", "message": "no such alias"})
	}
	if !d.mut(RateLimitOff) {
		for _, r := range d.requests {
			if r.State == "pending" && r.Alias == alias && d.contexts[r.ID] == text {
				return reply(75, map[string]any{"verdict": "pending", "request": r.ID})
			}
		}
	}
	id := uint64(len(d.requests) + 1)
	d.requests = append(d.requests, conformance.RequestInfo{ID: id, State: "pending", Kind: "alias", Alias: alias})
	d.contexts[id] = text
	if d.mut(RequestTextAnswers) && strings.Contains(text, "approved") {
		d.setState(id, "allowed")
		d.writeOut(conformance.OutRan, conformance.RanLine, true)
		return reply(0, map[string]any{"verdict": "allowed", "request": id})
	}
	return reply(75, map[string]any{"verdict": "pending", "request": id})
}

func (d *Daemon) flood(user, body string, out *[]string) int {
	rest, _ := strings.CutPrefix(body, "{ for i in ")
	nums, rest, _ := strings.Cut(rest, "; do ")
	inner, _, _ := strings.Cut(rest, " & done;")
	n := len(strings.Fields(nums))
	var all []string
	for range n {
		d.run(user, inner, &all)
	}
	count := 0
	for _, l := range all {
		if strings.Contains(l, conformance.TooMany) {
			count++
		}
	}
	if d.mut(FloodKillsLink) {
		d.linkDead = true
	}
	if d.mut(FloodLeaksMemory) {
		d.rss *= 4
	}
	if d.mut(FloodRestartsDaemon) {
		d.pid++
		d.started++
	}
	*out = append(*out, strconv.Itoa(count))
	if count == 0 {
		return 1
	}
	return 0
}

func (d *Daemon) kill(user, sig, pidS string, out *[]string) int {
	say := func(rc int, format string, a ...any) int {
		*out = append(*out, fmt.Sprintf(format, a...))
		return rc
	}
	pid, err := strconv.Atoi(pidS)
	if err != nil {
		return say(2, "kill: bad pid")
	}
	if user != "root" || d.fault(NoRoot) {
		return say(1, "sh: kill: Operation not permitted")
	}
	switch {
	case pid == 1 && sig == "-0":
		return 0
	case pid == d.pid && d.mut(DaemonPidVisible):
		return 0
	case pid == d.pid && sig == "-TERM" && d.mut(TermSucceeds):
		return 0
	case pid == d.pid && sig == "-TERM" && d.mut(DaemonKillable):
		d.killed = true
		return 0
	}
	return say(1, "sh: kill: No such process")
}

// curl answers the web UI's API as the host would.
func (d *Daemon) curl(a []string, out *[]string) int {
	if d.fault(DCurlMissing) {
		*out = append(*out, "sh: 1: curl: not found")
		return 127
	}
	var url, how string
	how = conformance.UINoToken
	for _, w := range a {
		switch {
		case strings.HasPrefix(w, "http://") && strings.Contains(w, "/api/"):
			url = w
		case strings.HasPrefix(w, "X-Caboose-Token:"):
			how = conformance.UIWrongToken
		case strings.HasPrefix(w, "Host: conf-foreign"):
			how = conformance.UIForeign
		}
	}
	host := strings.TrimPrefix(url, "http://")
	host = host[:strings.LastIndex(host, ":")]
	status := 0
	switch {
	case d.mut(WebPortOpen):
		status = 200
	case host == "host.docker.internal":
		status = 421
	case host == "host.internal":
		*out = append(*out, "curl: (6) Could not resolve host: host.internal")
		return 6
	default:
		*out = append(*out, "curl: (7) Failed to connect to "+host+": Connection refused")
		return 7
	}
	_ = how
	*out = append(*out, conformance.UIHTTP+strconv.Itoa(status))
	return 0
}

// Wait passes time in the model: the bucket fills, a proposal a mutant
// reads answers, a signal a mutant let through restarts the daemon.
func (d *Daemon) Wait(ctx context.Context, dur time.Duration) error {
	d.mu.Lock()
	if dur >= 10*time.Second {
		d.tokens = bucketSize
	}
	if d.mut(ProposalAnswers) {
		for p, text := range d.home {
			if !strings.Contains(p, "/.caboose-proposals/") || !strings.Contains(text, "[answer]") {
				continue
			}
			for _, line := range strings.Split(text, "\n") {
				if v, ok := strings.CutPrefix(line, "request = "); ok {
					id, _ := strconv.ParseUint(v, 10, 64)
					d.setState(id, "allowed")
				}
			}
		}
	}
	if d.mut(RequestAllowedNoGrant) {
		for i := range d.requests {
			if d.requests[i].State == "pending" {
				d.requests[i].State = "allowed"
			}
		}
	}
	if d.killed {
		d.killed = false
		d.pid++
		d.started++
	}
	d.mu.Unlock()
	return d.Link.Wait(ctx, dur)
}

// DaemonOps.

func (d *Daemon) Daemon() (conformance.DaemonInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fault(DaemonDown) {
		return conformance.DaemonInfo{Dir: "<B>/home/daemon"}, nil
	}
	return conformance.DaemonInfo{Running: true, PID: d.pid, Started: strconv.Itoa(d.started), Dir: "<B>/home/daemon",
		WebAddr: "127.0.0.1:7171", WebOwned: !d.fault(UINotOwned)}, nil
}

func (d *Daemon) HostHello(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fault(DaemonDown) {
		return errors.New("no daemon")
	}
	return nil
}

func (d *Daemon) LinkSocket() string { return fakeLinkSocket }

func (d *Daemon) HostAddrs() ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("192.168.64.1"), netip.MustParseAddr("fd00::1")}, nil
}

func (d *Daemon) UIStatic(context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fault(UIDown) {
		return 0, errors.New("connection refused")
	}
	return 200, nil
}

func (d *Daemon) TryUI(v conformance.UIVariant) (conformance.UIDecision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := "the host's caboose web UI, which approves what a sandbox asks for"
	if d.fault(UIRefusalUnnamed) {
		name = "refused"
	}
	ui := func(s string) bool {
		return strings.Contains(s, "7171") || strings.Contains(s, "7172") || s == "7000-7200"
	}
	switch {
	case v.ForwardPorts != "":
		if d.fault(UIRefusesAll) {
			return conformance.UIDecision{Stage: conformance.UIAtLoad, Message: "forward_ports: refused"}, nil
		}
		if ui(v.ForwardPorts) && d.mut(UIForwardDoctorOnly) {
			return conformance.UIDecision{Stage: conformance.UIAtDoctor, Message: "forward_ports holds port 7171: it is " + name}, nil
		}
		if ui(v.ForwardPorts) && !d.mut(UIForwardAllowed) {
			return conformance.UIDecision{Stage: conformance.UIAtLoad, Message: "forward_ports includes port 7171, which is never forwarded: it is " + name}, nil
		}
	case len(v.OpenURLsAllow) > 0:
		if !d.mut(UIURLAllowedInConfig) {
			return conformance.UIDecision{Stage: conformance.UIAtDecide, Message: "port 7171 is " + name}, nil
		}
	case len(v.EgressAllow) > 0:
		if !d.mut(UIEgressAllowed) {
			return conformance.UIDecision{Stage: conformance.UIAtDecide, Message: "deny:ui_port: port 7171 on 127.0.0.1 is " + name}, nil
		}
	case len(v.RunArgs) > 0:
		if slices.ContainsFunc(v.RunArgs, func(a string) bool { return strings.HasPrefix(a, "--publish") && ui(a) }) {
			if d.mut(UIPublishAllowed) {
				return conformance.UIDecision{}, nil
			}
			return conformance.UIDecision{Stage: conformance.UIAtDecide, Message: "publishes onto port 7171 of this machine: it is " + name}, nil
		}
		if d.mut(NetworkHostRefused) {
			return conformance.UIDecision{Stage: conformance.UIAtLoad, Message: "run_args: --network host"}, nil
		}
		return conformance.UIDecision{Stage: conformance.UIAtDoctor, Message: "run_args puts the sandbox on the host's network"}, nil
	}
	return conformance.UIDecision{}, nil
}

func (d *Daemon) Requests() ([]conformance.RequestInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests), nil
}

func (d *Daemon) Grants() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.grants, nil
}

func (d *Daemon) NextMounts() ([]conformance.Mount, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ms := d.mounts()
	if d.fault(MountsDrift) {
		ms = ms[1:]
	}
	if d.mut(SandboxRootsApplied) {
		for _, text := range d.home {
			if strings.Contains(text, "[roots.") {
				ms = append(ms, conformance.Mount{Source: "/", Target: "/work/conf-" + d.n8 + "-evil"})
			}
		}
	}
	return ms, nil
}

func (d *Daemon) Doctor(context.Context) (string, error) {
	return "sandbox.toml: an entry that reaches the host was skipped\n", nil
}

func (d *Daemon) OpenURLs() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fault(OpenURLsAsk) {
		return "ask", nil
	}
	return "allow", nil
}

func (d *Daemon) StubOpened() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.opened), nil
}

func (d *Daemon) DaemonRSS() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fault(RSSUnreadable) {
		return 0, errors.New("unreadable")
	}
	return d.rss, nil
}
