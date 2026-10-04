package agentproto

import (
	"encoding/json"
	"fmt"
)

// Version is the protocol's. The host and the agent each send it in a hello,
// and the host refuses an agent that speaks another: an image built by an
// older or newer launcher than the one linking to it.
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
)

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

	// Window, in a hello, is how many bytes each of the sender's streams
	// takes in flight, unread (HostWindow, AgentWindow); the session reads
	// it from the peer's first message. 0, from a side that announces
	// none, is DefaultWindow, so it needs no new Version.
	Window uint32 `json:"window,omitempty"`

	Boot *BootSpec `json:"boot,omitempty"`
	// Time is the host's wall clock, in nanoseconds since the Unix epoch.
	Time int64 `json:"time,omitempty"`
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
	// Request is the request (OpSSHAgent, OpConnect) this stream answers, where there
	// is no Port.
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
