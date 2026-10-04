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
)

// MaxChanged is the most paths one TypeChanged message carries.
const MaxChanged = 512

// Request operations.
const (
	OpOpen   = "open"
	OpNotify = "notify"
)

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

	Ports    []int     `json:"ports,omitempty"`
	Forwards []Forward `json:"forwards,omitempty"`

	Paths []string `json:"paths,omitempty"`
}

// Forward is one port the host was asked to forward, and what came of it.
type Forward struct {
	Port int `json:"port"`
	// HostPort is where it listens on the host, 0 when it does not.
	HostPort int `json:"host_port,omitempty"`
	// Reason says why it is not forwarded.
	Reason string `json:"reason,omitempty"`
}

// StreamHeader opens a stream: a connection the host accepted, for Port in
// the container.
type StreamHeader struct {
	Port int `json:"port"`
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

// Send encodes m and sends it on s.
func (s *Session) Send(m Message) error {
	b, err := Encode(m)
	if err != nil {
		return err
	}
	return s.SendControl(b)
}

// ValidPort reports whether p can be a TCP port.
func ValidPort(p int) bool { return p > 0 && p < 1<<16 }
