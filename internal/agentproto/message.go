package agentproto

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version is the protocol's. The host and the agent each send it in a hello,
// and the host refuses an agent that speaks another: an image built by an
// older or newer launcher than the one linking to it. What a side adds
// without breaking the other needs no new one: a request the host offers
// in its hello (an older agent never asks it), a message type the other
// side ignores, a field omitted when unset, a refusal's new Reason code
// (an older agent shows its Error, and answers as for a refusal it does
// not know).
const Version = 1

// Message types.
const (
	// TypeHello opens the link, from both sides.
	TypeHello = "hello"
	// TypeRequest is the agent asking the host to do something (Op).
	TypeRequest = "request"
	// TypeResponse is the host's answer to a request, by ID.
	TypeResponse = "response"
	// TypePorts is the agent's list of ports listening in the container,
	// sent whenever it changes.
	TypePorts = "ports"
	// TypeForwards is the host's list of what it forwards, sent whenever
	// that changes.
	TypeForwards = "forwards"
	// TypeChanged is the host's list of paths in the container that
	// changed on the host, for the agent to make watchers inside see. An
	// agent that does not know it ignores it, so it needs no new Version.
	TypeChanged = "changed"

	// The exec port's (a vm guest's): the host's TypeExec asks for one
	// command, a TypeResize changes its terminal's size, and the agent's
	// TypeExit ends it (exec.go).
	TypeExec   = "exec"
	TypeResize = "resize"
	TypeExit   = "exit"

	// The control port's (a vm guest's, control.go): the host's TypeClock
	// sets the guest's clock, TypeBoot starts the sandbox, which the
	// agent's TypeReady answers, and TypeShutdown stops it.
	TypeClock    = "clock"
	TypeBoot     = "boot"
	TypeReady    = "ready"
	TypeShutdown = "shutdown"
)

// MaxChanged is the most paths one TypeChanged message carries.
const MaxChanged = 512

// Request operations.
const (
	OpOpen   = "open"
	OpNotify = "notify"
	// OpSSHAgent is a client of the agent's SSH agent socket, which the
	// host answers by opening a stream whose header names the request,
	// connected to its own SSH agent. An agent that does not know it never
	// asks, and a host that does not never offers it, so it needs no new
	// Version.
	OpSSHAgent = "ssh-agent"
	// OpConnect is a client of the agent's outbound proxy: a TCP
	// connection to Host and Port, dialled from the host. The host checks
	// and dials it, then answers as for OpSSHAgent: a stream whose header
	// names the request, then the response; a refusal is a response alone,
	// whose Reason is one of the Egress* codes. Offered in the host's hello
	// (Egress), so it needs no new Version either.
	OpConnect = "connect"
	// OpHostExec is a client of `caboose-agent host`: a command to run on
	// the host. The host answers as for OpSSHAgent, with a stream whose
	// header names the request, on which the client speaks the exec port's
	// protocol (exec.go) as the host does to a guest's, roles reversed.
	// Offered in the host's hello (HostExec), so it needs no new Version.
	OpHostExec = "host-exec"
	// OpBrokerRequest files a request ahead of time (Ask): a crossing the
	// sandbox expects to need, with its own words about it. The response
	// carries the Verdict and, when pending, the RequestID. Offered in the
	// host's hello (Broker), so it needs no new Version.
	OpBrokerRequest = "broker-request"
	// OpBrokerRequests lists requests (Since, Session, All), waiting up to
	// Wait seconds for a decision newer than Since. The response carries
	// Requests and the Cursor to wait from next. Offered in the host's
	// hello (Broker).
	OpBrokerRequests = "broker-requests"
	// OpAliasRun is a client of `caboose-agent run`: an alias, one of the
	// host's [alias.NAME], to run there. The request's Exec names it,
	// Argv [NAME, KEY=VALUE...] and Dir the client's directory
	// (informational): never a command, which only the host's
	// config.toml says. Answered as OpHostExec is, with a stream on which
	// the client speaks the exec protocol, its Argv the same; a refusal
	// is a response alone, whose Reason is one of the Alias* codes and,
	// when pending, whose RequestID is the request filed. Offered in the
	// host's hello (AliasesOn), so it needs no new Version.
	OpAliasRun = "alias-run"
	// OpAliases lists the host's aliases: the response's Aliases, names,
	// descriptions and parameters only. Offered in the host's hello
	// (AliasesOn).
	OpAliases = "aliases"
	// OpCheckpoint is a client of `caboose-agent checkpoint`: a checkpoint
	// taken now of Paths, container paths of [checkpoints] paths in the
	// host's config.toml (none: every one of them). The host maps each to
	// the host directory it copies itself, and takes none it does not
	// list. The response's Checkpoints say, per path, the checkpoint
	// taken or why none was; a refusal of the whole request is a response
	// alone, whose Reason is one of the Checkpoint* codes. Offered in the
	// host's hello (CheckpointsOn), so it needs no new Version.
	OpCheckpoint = "checkpoint"
)

// The Reason of a refused OpCheckpoint.
const (
	// CheckpointOff: the host's policy refuses it (checkpoint_requests).
	CheckpointOff = "off"
	// CheckpointBad: a path is not one the sandbox can name.
	CheckpointBad = "bad"
	// CheckpointLimit: asked too often.
	CheckpointLimit = "limit"
	// CheckpointUnavailable: the host takes none now: no paths are
	// configured, the store cannot be reached, its link takes none.
	CheckpointUnavailable = "unavailable"
)

// The bounds of an OpCheckpoint's paths, which the agent keeps to before
// it sends one and the host checks again: config.toml's own (a test holds
// the two to each other).
const (
	MaxCheckpointPaths = 32
	MaxCheckpointPath  = 1024
)

// CheckpointWait is the longest the host spends on one OpCheckpoint: the
// wait for another checkpoint to finish and the takes themselves. The
// agent waits a little longer for its answer.
const CheckpointWait = 5 * time.Minute

// CheckpointTaken is one path of an OpCheckpoint's answer: the
// checkpoint's ID when one was taken, else Reason, why not.
type CheckpointTaken struct {
	Path   string `json:"path"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// The Reason of a refused OpAliasRun.
const (
	// AliasDenied: the host's policy refuses it.
	AliasDenied = "denied"
	// AliasPending: the host's policy asks a person first; RequestID is
	// the request filed for it.
	AliasPending = ReasonPending
	// AliasUnknown: the host has no alias of that name.
	AliasUnknown = "unknown"
	// AliasUnavailable: the host cannot run it now (its command is
	// missing there, too many run at once, the link is closing).
	AliasUnavailable = "unavailable"
)

// MaxAliases is the most aliases an OpAliases response lists.
const MaxAliases = 256

// AliasInfo is an alias as the host lists it to the sandbox: never its
// command or directory.
type AliasInfo struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Params      map[string][]string `json:"params,omitempty"`
	// Asks is set when running it asks a person on the host first.
	Asks bool `json:"asks,omitempty"`
}

// The Verdict of a crossing or a filed request.
const (
	VerdictAllowed = "allowed"
	VerdictDenied  = "denied"
	VerdictPending = "pending"
)

// ReasonPending is the Reason of a refused OpOpen whose URL waits for a
// person's answer on the host: the RequestID says which request.
const ReasonPending = "pending"

// TypeMods is the agent's count of the Claude Code sessions in the
// sandbox and of those with caboose's mod loaded (Mods), sent when it
// changes. A host that does not know it ignores it, so it needs no new
// Version.
const TypeMods = "mods"

// The bounds of what OpBrokerRequests carries back, and of how long one
// waits.
const (
	// MaxRequests is the most requests one response lists.
	MaxRequests = 200
	// MaxWait is the longest an OpBrokerRequests waits, in seconds.
	MaxWait = 300
)

// The bounds of an Ask's text, in bytes, which the agent keeps to before it
// sends one and the host checks again (the broker's limits; a test holds the
// two to each other). MaxAskArgs is the most Args an Ask may carry.
const (
	MaxAskContext        = 2000
	MaxAskOptions        = 5
	MaxAskOption         = 200
	MaxAskRecommendation = 500
	MaxAskMeanwhile      = 1000
	MaxAskSession        = 100
	MaxAskArgs           = MaxAliasArgs
)

// Ask is a request filed ahead of time (OpBrokerRequest): which crossing,
// and what the sandbox says of it. Everything in it is the sandbox's, and
// the host bounds and checks it before use.
type Ask struct {
	// Kind is "alias", "egress", "open_url" or "port".
	Kind string `json:"kind"`
	// Alias and Args, for an alias.
	Alias string            `json:"alias,omitempty"`
	Args  map[string]string `json:"args,omitempty"`
	// Host and Port, for egress; Port alone, for port.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// URL, for open_url.
	URL string `json:"url,omitempty"`
	// The sandbox's words about it.
	Context        string   `json:"context,omitempty"`
	Options        []string `json:"options,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Meanwhile      string   `json:"meanwhile,omitempty"`
	// Session is the session the sandbox says it asks for:
	// informational, and who hears of the decision.
	Session string `json:"session,omitempty"`
}

// RequestInfo is a request as the host lists it to the sandbox: the
// host's own description of it (Effect), never the sandbox's words.
type RequestInfo struct {
	ID        uint64    `json:"id"`
	Kind      string    `json:"kind"`
	Effect    string    `json:"effect,omitempty"`
	State     string    `json:"state"`
	Scope     string    `json:"scope,omitempty"`
	Hours     int       `json:"hours,omitempty"`
	Expires   time.Time `json:"expires,omitzero"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Session   string    `json:"session,omitempty"`
	// Seq is the cursor value of its last change.
	Seq uint64 `json:"seq,omitempty"`
}

// ModsInfo is TypeMods' count: Claude Code sessions running in the
// sandbox, and how many of them have caboose's mod loaded.
type ModsInfo struct {
	Sessions int `json:"sessions"`
	WithMod  int `json:"with_mod"`
}

// HostExecOff is the refusal of an OpHostExec on a host that offers none:
// what turns it on, and what makes a running link see that.
const HostExecOff = "running commands on the host is off for this environment: host_exec = true in [link] of its " +
	"config.toml, on the host, turns it on, and the running link rereads that file within seconds"

// The Reason of a refused OpConnect, for the agent to choose what it
// answers its client with; Error says the rest, written for a person.
const (
	// EgressOff: the host offers no outbound proxy.
	EgressOff = "off"
	// EgressBad: the request is not a host name or address and a port.
	EgressBad = "bad"
	// EgressPort: the port is not in egress_ports.
	EgressPort = "port"
	// EgressDenied: the name resolves to an address the host refuses
	// (loopback, private, ...), and egress_allow does not name it.
	EgressDenied = "denied"
	// EgressLimit: too many connections open, or opened too fast.
	EgressLimit = "limit"
	// EgressResolve: the name does not resolve on the host.
	EgressResolve = "resolve"
	// EgressDial: the host could not connect.
	EgressDial = "dial"
	// EgressPending: the host's policy asks a person first. The
	// connection is refused for now; RequestID is the request filed for
	// it, which a person may allow.
	EgressPending = "pending"
)

// EgressListen is where the host offers the agent to serve its outbound
// proxy, and so what HTTP_PROXY names in the sandbox: on loopback, outside
// the default forward_ports, and left out of the agent's port list, so it
// is never forwarded.
const EgressListen = "127.0.0.1:9128"

// MaxEgressHost is the longest host an OpConnect may name, a DNS name's.
const MaxEgressHost = 253

// EgressNoProxy is NO_PROXY where the sandbox uses the outbound proxy:
// what is the VM's own, which the proxy could not reach (the host dials
// from this machine, and refuses loopback and private addresses anyway).
// The guest's loopback, by name and address; every *.localhost; and
// 172.16.0.0/12, where the VM's dockerd puts its bridges (172.17.0.0/16
// first). The agent adds the guest's hostname at boot. curl, Go and
// Python's requests read a CIDR there; a client that reads none (wget,
// and Node's own proxy support may not) still sends a bridge's address
// to the proxy, which refuses it. Single-label names, as compose's
// services are, cannot be said in NO_PROXY: those go to the proxy too, and
// are refused unless they resolve on the host.
const EgressNoProxy = "localhost,127.0.0.1,::1,.localhost,172.16.0.0/12"

// EgressEnv is the environment that points a guest's programs at the
// outbound proxy served at listen (EgressListen): curl, git, apt, pip, npm
// and Go read the variables in one case or the other, so both are set.
func EgressEnv(listen string) []string {
	u := "http://" + listen
	return []string{
		"HTTP_PROXY=" + u, "HTTPS_PROXY=" + u, "http_proxy=" + u, "https_proxy=" + u,
		"NO_PROXY=" + EgressNoProxy, "no_proxy=" + EgressNoProxy,
	}
}

// Message is a control message. Which fields mean anything depends on Type.
type Message struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"`

	ID uint64 `json:"id,omitempty"`
	Op string `json:"op,omitempty"`

	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text,omitempty"`

	OK    bool   `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`
	// Reason is a refused request's code (Egress*), for OpConnect.
	Reason string `json:"reason,omitempty"`

	// Host and Port are where an OpConnect is to: a DNS name or an IP
	// address (no brackets), never resolved by the agent.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`

	Ports    []int     `json:"ports,omitempty"`
	Forwards []Forward `json:"forwards,omitempty"`

	// Paths are TypeChanged's paths, and OpCheckpoint's.
	Paths []string `json:"paths,omitempty"`

	Exec *ExecRequest `json:"exec,omitempty"`
	Rows uint16       `json:"rows,omitempty"`
	Cols uint16       `json:"cols,omitempty"`
	Code int          `json:"code,omitempty"`

	// SSHAgent, in the host's hello, is where the agent is to serve the
	// host's SSH agent (OpSSHAgent): under vm, where no socket of the
	// host's can be mounted.
	SSHAgent string `json:"ssh_agent,omitempty"`
	// Egress, in the host's hello, is the address (host:port) in the
	// sandbox where the agent is to serve its outbound proxy (OpConnect):
	// under vm, whose NAT reaches none of the host's VPN routes. "" offers
	// none.
	Egress string `json:"egress,omitempty"`
	// HostExec, in the host's hello, offers OpHostExec: host_exec, in the
	// host's config.toml.
	HostExec bool `json:"host_exec,omitempty"`
	// Roots, in the host's hello, are the container paths of the roots it
	// relays changes under (TypeChanged): the agent touches nothing
	// outside them. A root may be mounted anywhere, so the agent cannot
	// tell them itself.
	Roots []string `json:"roots,omitempty"`

	// Window, in a hello, is how many bytes each of the sender's streams
	// takes in flight, unread (HostWindow, AgentWindow); the session reads
	// it from the peer's first message. 0, from a side that announces
	// none, is DefaultWindow.
	Window uint32 `json:"window,omitempty"`

	Boot *BootSpec `json:"boot,omitempty"`
	// Time is the host's wall clock, in nanoseconds since the Unix epoch.
	Time int64 `json:"time,omitempty"`

	// Broker, in the host's hello, offers OpBrokerRequest and
	// OpBrokerRequests: the host files requests a person answers.
	Broker bool `json:"broker,omitempty"`
	// RequestID is the request a pending answer filed (Reason "pending"
	// of OpOpen, EgressPending of OpConnect, Verdict "pending").
	RequestID uint64 `json:"request_id,omitempty"`
	// Verdict is an OpBrokerRequest's answer: VerdictAllowed,
	// VerdictDenied or VerdictPending.
	Verdict string `json:"verdict,omitempty"`
	// Ask is OpBrokerRequest's request.
	Ask *Ask `json:"ask,omitempty"`
	// Since, Wait (seconds, 0 to MaxWait), Session and All are
	// OpBrokerRequests' filter; Requests (at most MaxRequests, newest
	// first) and Cursor its answer.
	Since    uint64        `json:"since,omitempty"`
	Wait     int           `json:"wait,omitempty"`
	Session  string        `json:"session,omitempty"`
	All      bool          `json:"all,omitempty"`
	Requests []RequestInfo `json:"requests,omitempty"`
	Cursor   uint64        `json:"cursor,omitempty"`
	// Mods is TypeMods' count.
	Mods *ModsInfo `json:"mods,omitempty"`
	// ModPID is only in a request an in-container command sends the
	// agent's link (`caboose-agent requests --mod`), never to the host: the
	// Claude Code process the command found above it, whose mod is waiting.
	ModPID int `json:"mod_pid,omitempty"`
	// AliasesOn, in the host's hello, offers OpAliasRun and OpAliases:
	// the host runs the aliases its config.toml defines.
	AliasesOn bool `json:"aliases_on,omitempty"`
	// Aliases are OpAliases' answer, at most MaxAliases.
	Aliases []AliasInfo `json:"aliases,omitempty"`
	// CheckpointsOn, in the host's hello, offers OpCheckpoint: its link
	// takes checkpoints when the sandbox asks.
	CheckpointsOn bool `json:"checkpoints_on,omitempty"`
	// Checkpoints are OpCheckpoint's answer, one per path asked for (every
	// configured one when none was), at most MaxCheckpointPaths.
	Checkpoints []CheckpointTaken `json:"checkpoints,omitempty"`
}

// Forward is one port the host was asked to forward, and what came of it.
type Forward struct {
	Port int `json:"port"`
	// HostPort is where it listens on the host, 0 when it does not.
	HostPort int `json:"host_port,omitempty"`
	// Reason says why it is not forwarded.
	Reason string `json:"reason,omitempty"`
}

// StreamHeader opens a stream: on the link, a connection the host
// accepted, for Port in the container.
type StreamHeader struct {
	Port int `json:"port,omitempty"`
	// Name is which of an exec's streams this is (Stream*), on the exec
	// port, where there is no Port.
	Name string `json:"name,omitempty"`
	// Request is the request (OpSSHAgent, OpConnect, OpHostExec,
	// OpAliasRun) this stream answers, where there is no Port.
	Request uint64 `json:"request,omitempty"`
}

// Encode is json.Marshal, for a Message.
func Encode(m Message) ([]byte, error) { return json.Marshal(m) }

// Decode reads a Message, refusing one without a type.
func Decode(b []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("agentproto: bad control message: %w", err)
	}
	if m.Type == "" {
		return m, fmt.Errorf("agentproto: control message without a type")
	}
	return m, nil
}

// Send encodes m and sends it on s. A hello announces this side's window,
// unless it sets another.
func (s *Session) Send(m Message) error {
	if m.Type == TypeHello && m.Window == 0 {
		m.Window = s.Window()
	}
	b, err := Encode(m)
	if err != nil {
		return err
	}
	return s.SendControl(b)
}

// ValidPort reports whether p can be a TCP port.
func ValidPort(p int) bool { return p > 0 && p < 1<<16 }
