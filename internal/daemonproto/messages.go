package daemonproto

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Methods of protocol 1. Their names, and the shape of replace and stop, are
// frozen across protocols: a client of a newer protocol has to be able to
// ask an older daemon to make way for a newer one.
const (
	MethodPing        = "ping"
	MethodStatus      = "status"
	MethodEnsureLink  = "ensure_link"
	MethodRestartLink = "restart_link"
	MethodStopLink    = "stop_link"
	MethodStop        = "stop"
	MethodReplace     = "replace"

	// MethodSubscribe is reserved for the web page and the app: the daemon
	// will then send event frames. Protocol 1 answers it with
	// KindUnknownMethod.
	MethodSubscribe = "subscribe"
)

// Error kinds. A kind is part of the protocol: callers branch on it, the
// message is for people. A client must treat a kind it does not know (from a
// later release of the same protocol) as KindInternal.
const (
	KindBadRequest       = "bad_request"       // params missing, malformed or out of range
	KindUnknownMethod    = "unknown_method"    //
	KindUnknownEnv       = "unknown_env"       // no such environment
	KindProtocolMismatch = "protocol_mismatch" // the peers' protocols differ and this side refuses
	KindBusy             = "busy"              // refused for now (host-exec commands running, a link starting)
	KindUnavailable      = "unavailable"       // no daemon answers (client side)
	KindTimeout          = "timeout"           // no answer in time (client side)
	KindBadFrame         = "bad_frame"         // the peer sent what the protocol forbids (client side)

	// The request broker's.
	KindNotOwner          = "not_owner"          // this daemon runs no link of the environment, so its broker is not this daemon's to serve
	KindNotFound          = "not_found"          // no such request or grant
	KindAlreadyAnswered   = "already_answered"   // the request was decided already; the first answer wins
	KindStaleDiff         = "stale_diff"         // config.toml would change differently than the diff confirmed
	KindNotAllowed        = "not_allowed"        // this kind of client may not do that
	KindBrokerUnavailable = "broker_unavailable" // the broker cannot serve now (its files, config.toml)
	KindInternal          = "internal"
)

// Error is a failure, on the wire and as a Go error.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Kind
	}
	return e.Kind + ": " + e.Message
}

// Is makes errors.Is(err, &Error{Kind: k}) compare kinds.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind
}

// NewError returns an Error, cutting a message that is over the limit and
// replacing control characters, since the peer will refuse a frame with them.
func NewError(kind, format string, a ...any) *Error {
	return &Error{Kind: kind, Message: Text(fmt.Sprintf(format, a...), maxMessage)}
}

// Text is s as a peer takes it in a message: control and invalid
// characters replaced, and cut to at most max bytes (at least 4).
func Text(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if !printable(string(r)) {
			return '?'
		}
		return r
	}, s)
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max-3], "") + "..."
	}
	return s
}

func (e *Error) validate() error {
	if len(e.Kind) > maxName || !nameRe.MatchString(e.Kind) {
		return bad("bad error kind")
	}
	return checkText("error message", e.Message, maxMessage)
}

// Kind returns an error's kind: its own for an *Error, else KindInternal.
func Kind(err error) string {
	if e, ok := err.(*Error); ok {
		switch e.Kind {
		case KindBadRequest, KindUnknownMethod, KindUnknownEnv, KindProtocolMismatch, KindBusy,
			KindUnavailable, KindTimeout, KindBadFrame,
			KindNotOwner, KindNotFound, KindAlreadyAnswered, KindStaleDiff, KindNotAllowed, KindBrokerUnavailable:
			return e.Kind
		}
	}
	return KindInternal
}

// Hello builds a hello frame.
func Hello(client, version string) *Frame { return helloProto(client, version, ProtocolVersion) }

func helloProto(client, version string, protocol int) *Frame {
	return &Frame{Type: TypeHello, Protocol: protocol, Version: version, Client: client}
}

// Request builds a request frame, encoding params (nil for none).
func Request(id uint64, method string, params any) (*Frame, error) {
	f := &Frame{Type: TypeRequest, ID: id, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, bad("params: %v", err)
		}
		f.Params = b
	}
	return f, nil
}

// Result builds a success response; a nil result is sent as {}.
func Result(id uint64, result any) (*Frame, error) {
	if result == nil {
		result = struct{}{}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, bad("result: %v", err)
	}
	return &Frame{Type: TypeResponse, ID: id, Result: b}, nil
}

// Failure builds an error response. ID 0 is for refusing the connection.
func Failure(id uint64, e *Error) *Frame {
	return &Frame{Type: TypeResponse, ID: id, Error: e}
}

// Compat is how a client's protocol relates to a daemon's.
type Compat int

const (
	CompatSame        Compat = iota // they work together
	CompatClientNewer               // the daemon is to be replaced, at once
	CompatClientOlder               // the client refuses and says what to run
)

// Compare relates a client's protocol to a daemon's. Release versions play
// no part: the same protocol is compatible whatever the releases.
func Compare(client, daemon int) Compat {
	switch {
	case client == daemon:
		return CompatSame
	case client > daemon:
		return CompatClientNewer
	default:
		return CompatClientOlder
	}
}

// DecodeParams reads a request's params into v (a pointer to a struct),
// ignoring fields it does not know, and runs v's Validate if it has one.
func DecodeParams(f *Frame, v any) *Error {
	if f.Type != TypeRequest {
		return NewError(KindBadRequest, "not a request")
	}
	if len(f.Params) > 0 {
		if err := json.Unmarshal(f.Params, v); err != nil {
			return NewError(KindBadRequest, "params: %v", err)
		}
	}
	if c, ok := v.(interface{ Validate() error }); ok {
		if err := c.Validate(); err != nil {
			return NewError(KindBadRequest, "%v", err)
		}
	}
	return nil
}

// EnvParams are the params of ensure_link, restart_link and stop_link.
type EnvParams struct {
	Env string `json:"env"`
}

// Validate checks the environment's name.
func (p *EnvParams) Validate() error {
	if !ValidEnv(p.Env) {
		return fmt.Errorf("bad environment name")
	}
	return nil
}

// ReplaceParams are the params of replace: the daemon exits so that the
// next command starts a newer one. It answers at once. Without Force it
// then waits while host-exec commands run (a held one, such as a keep-awake
// command, makes it wait), and does nothing when it already is Version or
// newer; with Force it kills them and exits. Only a client of a newer
// protocol may force: the daemon refuses it from one of its own.
type ReplaceParams struct {
	Version string `json:"version,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

// Validate checks the version.
func (p *ReplaceParams) Validate() error {
	return checkText("version", p.Version, maxName)
}

// ReplaceResult says whether the daemon exits now or waits for host-exec
// commands, and how many run.
type ReplaceResult struct {
	Exiting   bool `json:"exiting"`
	HostExecs int  `json:"host_execs,omitempty"` // commands and jobs together
	// Jobs is how many of HostExecs are jobs (stop, restart, apply).
	Jobs int `json:"jobs,omitempty"`
	// Current is set when the daemon is already the version asked for, or
	// a newer one: it does nothing.
	Current bool `json:"current,omitempty"`
}

// Owners of an environment's link other than the daemon asked.
const (
	OwnerRelease = "release"
	OwnerDev     = "dev"
)

// Sandbox states, as an EnvStatus reports them.
const (
	SandboxRunning = "running"
	SandboxStopped = "stopped"
	SandboxAbsent  = "absent"
	SandboxUnknown = "unknown"
)

// Link states.
const (
	LinkRunning  = "running"
	LinkStarting = "starting"
	LinkStopped  = "stopped" // none runs, none is wanted
	LinkFailed   = "failed"  // one was wanted and could not run; LastError says why
)

// DaemonInfo is what a daemon says of itself.
type DaemonInfo struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	Protocol  int       `json:"protocol"`
	Started   time.Time `json:"started"`
	Dev       bool      `json:"dev,omitempty"` // a checkout's build, with a directory of its own
	Dir       string    `json:"dir,omitempty"`
	HostExecs int       `json:"host_execs"` // host-exec commands running, jobs included
	// Jobs is how many of HostExecs are jobs (stop, restart, apply), which
	// a daemon's shutdown neither ends nor waits for.
	Jobs int `json:"jobs,omitempty"`
	// Replacing is set while a replace waits for the host-exec commands
	// to end before the daemon exits.
	Replacing bool `json:"replacing,omitempty"`
	// Recovering is set while a daemon that has just started is still
	// bringing back the links its directory ran before: an environment
	// it does not list yet may be one of them.
	Recovering bool `json:"recovering,omitempty"`
	// Web is the web page's state, never its token; absent when the
	// daemon serves no page.
	Web *WebState `json:"web,omitempty"`
	// App is the attached menu-bar app ({attached: false} while none
	// is); absent from a daemon that predates it.
	App *AppInfo `json:"app,omitempty"`
}

// EnvStatus is the state of one environment the daemon knows.
type EnvStatus struct {
	Name      string    `json:"name"`
	Sandbox   string    `json:"sandbox"`
	Link      string    `json:"link"`
	Since     time.Time `json:"since,omitzero"` // when the link entered its state
	LastError string    `json:"last_error,omitempty"`
	// Pending is how many requests wait for an answer in the environment's
	// broker; absent while none does, or no link of this daemon serves it.
	Pending int `json:"pending,omitempty"`
	// Trust is the environment's trust level ("fallible" or "hostile").
	Trust string `json:"trust,omitempty"`
	// CheckpointsLine is the text caboose status shows for the checkpoints,
	// or empty.
	CheckpointsLine string `json:"checkpoints_line,omitempty"`
	// Owner is empty when this daemon serves the environment's broker;
	// else which daemon records itself as serving the environment's link:
	// OwnerRelease or OwnerDev (from link.json, so it is the recorded
	// owner, not proof that it is alive).
	Owner string `json:"owner,omitempty"`
	// ModsMissing is how many Claude Code sessions in the sandbox run
	// without caboose's mod, as the link's agent last counted.
	ModsMissing int `json:"mods_missing,omitempty"`
	// Checkpoints is the environment's checkpoint schedule; absent while
	// the daemon runs none for it.
	Checkpoints *CheckpointStatus `json:"checkpoints,omitempty"`
	// Job is the environment's latest job (a stop, a restart, or a
	// proposal applied with its build): while it runs, and for JobKept
	// after it ended; absent otherwise.
	Job *JobInfo `json:"job,omitempty"`
}

// JobKept is how long status keeps saying how an ended job went.
const JobKept = 10 * time.Minute

// StatusResult is the result of status.
type StatusResult struct {
	Daemon DaemonInfo  `json:"daemon"`
	Envs   []EnvStatus `json:"envs"`
	// Attention is what needs the user, newest first, at most
	// MaxAttention, as the daemon last computed it.
	Attention []AttentionItem `json:"attention"`
}

// Link actions, as a LinkResult reports them.
const (
	ActionStarted   = "started"   // no link served the environment; one does now
	ActionReplaced  = "replaced"  // a link with other settings was stopped and another started
	ActionUnchanged = "unchanged" // a link with the same settings already ran
	ActionStopped   = "stopped"
	ActionNoSandbox = "no_sandbox" // ensure_link: the sandbox does not run, so nothing was started
)

// LinkResult is the result of ensure_link, restart_link and stop_link.
type LinkResult struct {
	Action string    `json:"action"`
	Env    EnvStatus `json:"env"`
}

// Validate checks the strings of a status, which the client prints: they
// may carry text that came from a sandbox.
func (s *StatusResult) Validate() error {
	if err := s.Daemon.validate(); err != nil {
		return err
	}
	for i := range s.Envs {
		if err := s.Envs[i].validate(); err != nil {
			return err
		}
	}
	return validateAttention(s.Attention)
}

// Validate checks the strings of a link result.
func (r *LinkResult) Validate() error {
	if err := checkText("action", r.Action, maxName); err != nil {
		return err
	}
	return r.Env.validate()
}

func (d *DaemonInfo) validate() error {
	if err := checkText("daemon version", d.Version, maxName); err != nil {
		return err
	}
	if d.Web != nil {
		if err := d.Web.validate(); err != nil {
			return err
		}
	}
	if d.App != nil {
		if err := d.App.validate(); err != nil {
			return err
		}
	}
	return checkText("daemon dir", d.Dir, 1024)
}

func (e *EnvStatus) validate() error {
	if !ValidEnv(e.Name) {
		return bad("bad environment name in a status")
	}
	for _, s := range []string{e.Sandbox, e.Link} {
		if err := checkText("state", s, maxName); err != nil {
			return err
		}
	}
	if e.Pending < 0 || e.ModsMissing < 0 {
		return bad("negative pending or mods count")
	}
	for _, s := range []struct {
		what, v string
		max     int
	}{{"trust", e.Trust, maxName}, {"checkpoints line", e.CheckpointsLine, maxDetailText}, {"owner", e.Owner, maxName}} {
		if err := checkText(s.what, s.v, s.max); err != nil {
			return err
		}
	}
	if e.Checkpoints != nil {
		if err := e.Checkpoints.validate(); err != nil {
			return err
		}
	}
	if e.Job != nil {
		if err := e.Job.Validate(); err != nil {
			return err
		}
	}
	return checkText("last error", e.LastError, maxMessage)
}
