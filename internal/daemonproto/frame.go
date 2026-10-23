// Package daemonproto is the protocol between caboose's host daemon and its
// clients (the command line today; a web page and an app later), and the
// client that speaks it.
//
// The transport is a Unix socket carrying newline-delimited JSON: one frame
// per line, every line bounded by MaxLine. The peer is another process, so
// everything read is checked before it is used: a line over the bound, a
// frame that is not exactly one JSON object of a known shape, a string with
// control characters or a name outside its alphabet is refused, and a
// refused line ends the conversation (the stream has lost its framing).
//
// A conversation starts with a hello from each side. Requests carry an id,
// and the daemon answers with a response carrying the same id, either a
// result or an error with a stable kind. Event frames are reserved for the
// daemon to send after a subscription; no version of the protocol sends one
// yet, and a client skips any it receives.
package daemonproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode"
	"unicode/utf8"
)

const (
	// ProtocolVersion is the protocol this code speaks. Two peers with the
	// same protocol work together whatever their releases; a different one
	// is skew (see Compare).
	ProtocolVersion = 1

	// MaxLine bounds a frame, newline included.
	MaxLine = 1 << 20

	// MaxSocketPath is the longest path a Unix socket may have on a Mac:
	// sun_path's 104 bytes, the last a NUL. Every socket caboose makes is
	// held to it.
	MaxSocketPath = 103

	maxName    = 64   // a version, method, event or kind
	maxMessage = 4096 // an error's human message
)

// Frame types.
const (
	TypeHello    = "hello"
	TypeRequest  = "request"
	TypeResponse = "response"
	TypeEvent    = "event"
)

// Client kinds a hello may name.
const (
	ClientCLI = "cli"
	ClientWeb = "web"
	ClientApp = "app"
)

// Frame is one line of the protocol. Which fields are used depends on Type;
// Decode and Encode refuse a frame that sets a field its type does not have.
type Frame struct {
	Type string `json:"type"`

	// hello
	Protocol int    `json:"protocol,omitempty"`
	Version  string `json:"version,omitempty"` // the sender's caboose version
	Client   string `json:"client,omitempty"`  // the sender's kind; the daemon's own hello says "daemon"

	// request and response
	ID uint64 `json:"id,omitempty"`

	// request
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`

	// response: exactly one of Result and Error. An Error with ID 0 is the
	// daemon refusing the connection itself.
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`

	// event
	Event string          `json:"event,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// ClientDaemon is what the daemon's own hello names as its kind.
const ClientDaemon = "daemon"

// Framing errors. Both leave the stream unusable.
var (
	ErrLineTooLong = errors.New("daemonproto: frame over the size limit")
	ErrBadFrame    = errors.New("daemonproto: malformed frame")
)

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadFrame, fmt.Sprintf(format, a...))
}

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	envNameR = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ValidEnv reports whether s can name an environment (the same alphabet and
// length config.ValidEnv accepts).
func ValidEnv(s string) bool { return envNameR.MatchString(s) }

// printable reports whether s is valid UTF-8 without control, format
// (bidi overrides, zero-width characters) or line-separator characters.
func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

func checkText(what, s string, max int) error {
	if len(s) > max {
		return bad("%s over %d bytes", what, max)
	}
	if !printable(s) {
		return bad("%s has control or invalid characters", what)
	}
	return nil
}

func isNull(m json.RawMessage) bool { return len(m) == 0 || string(m) == "null" }

// checkObject requires a payload that is absent or a JSON object.
func checkObject(what string, m json.RawMessage) error {
	if isNull(m) {
		return nil
	}
	if m[0] != '{' {
		return bad("%s is not an object", what)
	}
	return nil
}

// Validate checks a frame's shape: the same checks Decode and Encode make.
func (f *Frame) Validate() error {
	has := func(set ...bool) bool {
		for _, s := range set {
			if s {
				return true
			}
		}
		return false
	}
	hello := has(f.Protocol != 0, f.Version != "", f.Client != "")
	req := has(f.Method != "", !isNull(f.Params))
	resp := has(!isNull(f.Result), f.Error != nil)
	event := has(f.Event != "", !isNull(f.Data))
	switch f.Type {
	case TypeHello:
		if has(f.ID != 0, req, resp, event) {
			return bad("hello with other fields")
		}
		if f.Protocol < 1 {
			return bad("hello without a protocol")
		}
		if f.Version == "" {
			return bad("hello without a version")
		}
		if err := checkText("version", f.Version, maxName); err != nil {
			return err
		}
		switch f.Client {
		case ClientCLI, ClientWeb, ClientApp, ClientDaemon:
		default:
			return bad("unknown client kind")
		}
	case TypeRequest:
		if has(hello, resp, event) {
			return bad("request with other fields")
		}
		if f.ID == 0 {
			return bad("request without an id")
		}
		if len(f.Method) > maxName || !nameRe.MatchString(f.Method) {
			return bad("bad method name")
		}
		return checkObject("params", f.Params)
	case TypeResponse:
		if has(hello, req, event) {
			return bad("response with other fields")
		}
		if (f.Error != nil) == !isNull(f.Result) {
			return bad("response needs exactly one of result and error")
		}
		if f.Error != nil {
			return f.Error.validate()
		}
		if f.ID == 0 {
			return bad("result without an id")
		}
		return checkObject("result", f.Result)
	case TypeEvent:
		if has(hello, req, resp, f.ID != 0) {
			return bad("event with other fields")
		}
		if len(f.Event) > maxName || !nameRe.MatchString(f.Event) {
			return bad("bad event name")
		}
		return checkObject("data", f.Data)
	default:
		return bad("unknown frame type")
	}
	return nil
}

// Decode parses one line (without its newline) into a frame, refusing
// anything over MaxLine, trailing data, an unknown type or a malformed shape.
// Fields it does not know are ignored, so a later release of the same
// protocol may add some.
func Decode(line []byte) (*Frame, error) {
	if len(line)+1 > MaxLine {
		return nil, ErrLineTooLong
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	var f Frame
	if err := dec.Decode(&f); err != nil {
		return nil, bad("%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, bad("data after the frame")
	}
	if isNull(f.Params) {
		f.Params = nil
	}
	if isNull(f.Result) {
		f.Result = nil
	}
	if isNull(f.Data) {
		f.Data = nil
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Encode returns the frame as one line, newline included, refusing a frame
// Decode would refuse or one over MaxLine.
func Encode(f *Frame) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil, bad("%v", err)
	}
	if len(b)+1 > MaxLine {
		return nil, ErrLineTooLong
	}
	return append(b, '\n'), nil
}

// Write encodes f and writes it with one Write call.
func Write(w io.Writer, f *Frame) error {
	b, err := Encode(f)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Reader reads frames from a stream.
type Reader struct {
	r    *bufio.Reader
	line []byte
}

// NewReader returns a Reader on r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReaderSize(r, 4096)} }

// Read returns the next frame. It returns io.EOF at a clean end of stream,
// io.ErrUnexpectedEOF if the stream ends inside a line, ErrLineTooLong for a
// line over MaxLine (without reading the rest of it) and an ErrBadFrame for a
// line that is not a valid frame. After any error but a read error of the
// transport's own, the stream should be closed.
func (r *Reader) Read() (*Frame, error) {
	if cap(r.line) > 64<<10 {
		r.line = nil // do not keep a large frame's buffer for the connection's life
	}
	r.line = r.line[:0]
	for {
		chunk, err := r.r.ReadSlice('\n')
		if len(r.line)+len(chunk) > MaxLine {
			return nil, ErrLineTooLong
		}
		r.line = append(r.line, chunk...)
		switch err {
		case nil:
			return Decode(bytes.TrimSuffix(r.line, []byte("\n")))
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(r.line) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

func unmarshal(m json.RawMessage, v any) error {
	if isNull(m) {
		return nil
	}
	return json.Unmarshal(m, v)
}
