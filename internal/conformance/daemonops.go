package conformance

import (
	"context"
	"errors"
	"net/netip"
	"strings"
)

// DaemonOps are the host-side calls of the daemon, web UI, answer and
// credentials-neighbour checks: what the throwaway environment's daemon,
// broker and launcher say, read as the suite's own user under B. The real
// Host implements them in the launcher's conformance command; a Host that
// does not makes every check that needs them an ERROR, never a PASS.
//
// None of them may answer a request, read a token or a credential, open a
// browser, or reach a daemon but the throwaway environment's own.
type DaemonOps interface {
	// Daemon is the throwaway daemon as its daemon.json and the host's
	// process table say.
	Daemon() (DaemonInfo, error)
	// HostHello opens the throwaway daemon's socket from the host, as the
	// suite's own user, and sends a status hello: nil when the daemon
	// answered it.
	HostHello(ctx context.Context) error
	// LinkSocket is the sandbox path of the link's socket: the one socket
	// a sandbox has by design.
	LinkSocket() string
	// HostAddrs are this machine's non-loopback addresses and, under vm,
	// the guest's gateway: where a sandbox could reach the host other
	// than by name.
	HostAddrs() ([]netip.Addr, error)
	// UIStatic is the HTTP status the throwaway daemon's web UI gives the
	// host for its page (GET /, no token).
	UIStatic(ctx context.Context) (int, error)
	// TryUI loads a copy of the configuration changed as v says, under B,
	// with the launcher's own loader, and decides a reach to the web UI's
	// port under it; it changes nothing.
	TryUI(v UIVariant) (UIDecision, error)
	// Requests are the broker's requests for the environment, as the
	// daemon's requests method lists them to the host.
	Requests() ([]RequestInfo, error)
	// Grants is how many grants the broker holds for the environment.
	Grants() (int, error)
	// NextMounts are the mounts the launcher would create the sandbox with
	// now, from config.toml, without creating anything.
	NextMounts() ([]Mount, error)
	// Doctor is caboose doctor's output for the environment.
	Doctor(ctx context.Context) (string, error)
	// OpenURLs is the open_urls the running link decides by.
	OpenURLs() (string, error)
	// StubOpened are the URLs the suite's stub opener recorded this run,
	// oldest first.
	StubOpened() ([]string, error)
	// DaemonRSS is the throwaway daemon's resident memory, in bytes.
	DaemonRSS() (int64, error)
	// DaemonLog is the throwaway daemon's log as the witness reads it
	// (MarkDaemonLog of its daemon.log.1 and daemon.log): the host's
	// witness that a socket which said nothing reached the daemon.
	DaemonLog() (DaemonLogMark, error)
	// HostSilent connects to the throwaway daemon's socket from the host,
	// as the suite's own user, and closes without a hello: the witness's
	// positive control, which the daemon logs as a refusal.
	HostSilent(ctx context.Context) error
	// Relays are the sandbox paths caboose-agent serves for the host, and
	// what each reaches there ("the host's SSH agent").
	Relays() map[string]string
}

// refusalLines are what the daemon logs for a connection it took and
// closed without a frame: a peer it does not serve, one past its
// connections, a hello that never came or was no hello.
var refusalLines = []string{"refused a connection", "a client's hello"}

// DaemonLogMark is what the witness keeps of the daemon's log between two
// reads: how many refusals it records, and enough of daemon.log to tell
// it was moved aside in between (cappedlog renames it to daemon.log.1 and
// starts a new one with a line saying so): a log that shrank, or begins
// with another line, was rotated, and a refusal may have gone with it.
type DaemonLogMark struct {
	Refusals int
	Size     int
	Head     string
}

// maxHead bounds DaemonLogMark.Head.
const maxHead = 512

// MarkDaemonLog is the mark of a daemon's log, from the copy moved aside
// (old, nil for none) and the current file.
func MarkDaemonLog(old, cur []byte) DaemonLogMark {
	head, _, _ := strings.Cut(string(cur[:min(len(cur), maxHead)]), "\n")
	return DaemonLogMark{Refusals: DaemonRefusals(old) + DaemonRefusals(cur), Size: len(cur), Head: head}
}

// RotatedSince reports whether the log was moved aside between before and
// m.
func (m DaemonLogMark) RotatedSince(before DaemonLogMark) bool {
	return m.Size < before.Size || m.Head != before.Head && before.Size > 0
}

// DaemonRefusals counts the lines of a daemon log that record a
// connection the daemon took and refused.
func DaemonRefusals(log []byte) int {
	n := 0
	for _, l := range strings.Split(string(log), "\n") {
		for _, r := range refusalLines {
			if strings.Contains(l, r) {
				n++
				break
			}
		}
	}
	return n
}

// DaemonInfo is what the host knows of the throwaway daemon.
type DaemonInfo struct {
	Running bool
	PID     int
	// Started is the process's start time as the host reports it, so a
	// restarted daemon is told from one whose pid was reused.
	Started string
	// Dir is the daemon's directory, named as Mount.Source names host
	// paths.
	Dir string
	// WebAddr is the web UI's address, "" when it serves none; WebOwned
	// whether the listener there is the throwaway daemon's own (its pid
	// holds it).
	WebAddr  string
	WebOwned bool
}

// UIVariant is a change to the throwaway configuration that would reach
// the web UI's port: [link] forward_ports and open_urls_allow, the vm
// profile's egress_allow, the container profile's run_args.
type UIVariant struct {
	ForwardPorts                        string
	OpenURLsAllow, EgressAllow, RunArgs []string
}

// UIDecision is where the product stopped a UIVariant, and what it said.
type UIDecision struct {
	// Stage is UIAtLoad, UIAtDecide, UIAtDoctor or "" (nothing refused it).
	Stage   string
	Message string
}

// The stages a reach to the web UI can be stopped at.
const (
	// UIAtLoad: the configuration does not load.
	UIAtLoad = "load"
	// UIAtDecide: it loads, and the reach is refused when it is decided
	// (the link's forward, CheckURL, the egress policy, the create).
	UIAtDecide = "decide"
	// UIAtDoctor: it loads and is used; only doctor reports it.
	UIAtDoctor = "doctor"
)

// RequestInfo is one request of the broker's.
type RequestInfo struct {
	ID uint64
	// State is "pending", "allowed", "denied" or "expired".
	State       string
	Kind, Alias string
}

// DaemonOps are the daemon calls, inside the envelope only.
func (e Env) DaemonOps() (DaemonOps, error) {
	if err := e.sealed(); err != nil {
		return nil, err
	}
	ops, ok := e.host.(DaemonOps)
	if !ok || ops == nil {
		return nil, errors.New("conformance: this host offers no daemon calls")
	}
	return ops, nil
}
