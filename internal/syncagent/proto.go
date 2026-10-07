// Package syncagent runs caboose sync in the sandbox, and is the protocol
// between it and the host's caboose.
//
// The host runs `caboose-agent sync run` (or `sync status`) in the sandbox,
// with no terminal, and the two talk over the command's stdin and stdout in
// newline-delimited JSON: the agent says hello with its Version, the host
// sends one Request, and the agent sends what it needs answered (a conflict
// to settle, a prompt of git's or ssh's) and, last, its result -- a report,
// a status, or an error. Every git command, and every file the sync reads
// or writes, is the sandbox's own; the host only holds the lock, asks the
// person at the terminal, and says what happened. git's own output (a
// fetch's, a push's) is the command's stderr.
//
// Both ends bound every line they read (MaxLine): the host reads what the
// sandbox writes, and trusts none of it.
package syncagent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bfreis/caboose/internal/statesync"
)

// Version is the protocol's. A host and an agent of different versions do
// not talk: the image is from another caboose, and a restart makes it this
// one's.
const Version = 1

// MaxLine bounds one message, either way: the largest is a conflict's
// markers (at most MaxMarkers, base64), or the file edited from them.
const MaxLine = 32 << 20

// MaxMarkers bounds the markers a conflict carries for an edit by hand: a
// larger file is settled by keeping a side, or in the sandbox.
const MaxMarkers = 16 << 20

// The operations, caboose-agent sync's argument.
const (
	OpRun    = "run"
	OpStatus = "status"
)

// Request is what the host asks for, the first line it sends.
type Request struct {
	Version int `json:"version"`
	// Host names this machine in commits.
	Host string `json:"host"`
	// Remote, when set, is the remote to set before the sync (run only).
	Remote string `json:"remote,omitempty"`
	// Auto is a sync nobody is watching: nothing prompts, and an ssh
	// that would ask fails instead (BatchMode).
	Auto bool `json:"auto,omitempty"`
	// Budget, in seconds, bounds what reaches the remote, all told; 0 for
	// none.
	Budget int `json:"budget,omitempty"`
	// Interactive is set when someone at the host's terminal can settle
	// a conflict and answer a prompt: without it, a conflict aborts the
	// sync and a prompt is refused, never sent.
	Interactive bool `json:"interactive,omitempty"`
	// ShowGit sends git's fetch and push output to stderr, which the host
	// shows; without it, it is in the error of one that fails.
	ShowGit bool `json:"show_git,omitempty"`
	// Sandbox is the sandbox config in effect, as the host loaded it (the
	// file, its last good copy, or the defaults); SandboxErr is why the
	// file itself could not be used, which stops a sync.
	Sandbox    []byte `json:"sandbox"`
	SandboxErr string `json:"sandbox_err,omitempty"`
	// Roots are the container paths of the host's roots, for the
	// defaults.
	Roots []string `json:"roots"`
	// Defaults, when set, is written as the sandbox config when a sync
	// leaves none (statesync.Syncer.Defaults).
	Defaults []byte `json:"defaults,omitempty"`
	// Mounted are the home-relative paths the sandbox has mounted under
	// the home: a keep entry not among them syncs nothing yet.
	Mounted []string `json:"mounted"`
}

// Message is one line from the agent; Type says which field is set.
type Message struct {
	Type     string            `json:"type"`
	Version  int               `json:"version,omitempty"`
	Conflict *Conflict         `json:"conflict,omitempty"`
	Ask      *Ask              `json:"ask,omitempty"`
	Report   *statesync.Report `json:"report,omitempty"`
	Status   *Status           `json:"status,omitempty"`
	Error    *Error            `json:"error,omitempty"`
}

// The message types.
const (
	TypeHello    = "hello"
	TypeConflict = "conflict"
	TypeAsk      = "ask"
	TypeReport   = "report"
	TypeStatus   = "status"
	TypeError    = "error"
)

// Conflict is a file both machines changed in ways that could not be
// merged (statesync.Conflict), as the host needs it to ask: not its sides,
// which the agent keeps, but Markers for an edit by hand. The host answers
// with a Reply: Take a side, a Resolution, or Abort.
type Conflict struct {
	Path string   `json:"path"`
	JSON bool     `json:"json,omitempty"`
	Keys []string `json:"keys,omitempty"`
	Note string   `json:"note,omitempty"`
	// OursDeleted and TheirsDeleted say a side deleted the file.
	OursDeleted   bool `json:"ours_deleted,omitempty"`
	TheirsDeleted bool `json:"theirs_deleted,omitempty"`
	// Markers is the file with diff3 conflict markers; MarkersErr why
	// there are none (an edit is then not offered).
	Markers    []byte `json:"markers,omitempty"`
	MarkersErr string `json:"markers_err,omitempty"`
}

// Ask is a prompt of git's or ssh's: a host key to accept, a username, a
// password or a passphrase. The host decides whether what is typed is
// shown, and answers with a Reply: Answer, or Abort to refuse.
type Ask struct {
	Prompt string `json:"prompt"`
}

// The sides a Reply can Take.
const (
	TakeOurs   = "ours"
	TakeTheirs = "theirs"
)

// Reply is the host's answer to a Conflict (Take, Resolution or Abort) or
// an Ask (Answer, or Abort).
type Reply struct {
	Take       string                `json:"take,omitempty"`
	Resolution *statesync.Resolution `json:"resolution,omitempty"`
	Answer     *string               `json:"answer,omitempty"`
	Abort      bool                  `json:"abort,omitempty"`
}

// Error is a sync that did not go through, by kind, so the host can say
// what to do next.
type Error struct {
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	Paths   []string `json:"paths,omitempty"`
}

// The error kinds.
const (
	// KindAborted is a conflict nobody settled: nothing live changed.
	KindAborted = "aborted"
	// KindSecrets is an export holding what looks like a credential;
	// Paths are the files.
	KindSecrets = "secrets"
	// KindNoRemote is a sync repo with no remote yet.
	KindNoRemote = "noremote"
	// KindSandboxConfig is a sandbox config that cannot be used.
	KindSandboxConfig = "sandboxconfig"
	// KindNoAnswer is a remote that did not answer within the budget.
	KindNoAnswer = "noanswer"
	// KindVersion is a request of another protocol version.
	KindVersion = "version"
	// KindBusy is another sync running in the sandbox: one whose host
	// went away, say, finishing.
	KindBusy = "busy"
	// KindStopped is a sync stopped because its host went away.
	KindStopped = "stopped"
	// KindSync is a sync that failed on its way; KindSetup one that
	// could not start (the repo, the remote).
	KindSync  = "sync"
	KindSetup = "setup"
)

func (e *Error) Error() string { return e.Message }

// Status is what caboose sync status and doctor ask: what this machine
// would send (PendingStatus), and what the remote has.
type Status struct {
	// Changed and Deleted are the repo paths waiting to be sent.
	Changed []string `json:"changed,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
	// Refused, Shadowed and Unmounted are as statesync.Report has them.
	Refused   []string `json:"refused,omitempty"`
	Shadowed  []string `json:"shadowed,omitempty"`
	Unmounted []string `json:"unmounted,omitempty"`
	// Secrets are files a sync refuses to send for holding what looks
	// like a credential.
	Secrets []string `json:"secrets,omitempty"`
	// Remote is set when what follows was asked: the sync repo's own
	// state and the remote's, which only the sandbox can read.
	Remote bool `json:"remote,omitempty"`
	// Dirty is a sync that stopped halfway (statesync.Syncer.Dirty).
	Dirty bool `json:"dirty,omitempty"`
	// Unsent is what was committed here and never pushed.
	Unsent statesync.Unsent `json:"unsent"`
	// FetchErr is why the fetch failed; "" when it went through, and then
	// Divergence and Taken and Others say what it brought.
	FetchErr      string               `json:"fetch_err,omitempty"`
	Divergence    statesync.Divergence `json:"divergence"`
	DivergenceErr string               `json:"divergence_err,omitempty"`
	Taken         []string             `json:"taken,omitempty"`
	Others        []string             `json:"others,omitempty"`
}

// PendingStatus is what s's home has not sent: Pending, and what the next
// sync would make of it. It reads, and writes nothing.
func PendingStatus(s *statesync.Syncer) (*Status, error) {
	p, err := s.Pending()
	if err != nil {
		return nil, err
	}
	st := &Status{Changed: p.Changed, Deleted: p.Deleted, Refused: p.Export.Refused, Shadowed: p.Export.Shadowed}
	var se *statesync.SecretsError
	if err := p.Export.ScanSecrets(); errors.As(err, &se) {
		st.Secrets = se.Paths
	} else if err != nil {
		return nil, err
	}
	return st, nil
}

// ErrTooLong is a line longer than MaxLine.
var ErrTooLong = errors.New("message too long")

// Reader reads messages, a line each.
type Reader struct {
	r   *bufio.Reader
	max int
}

// NewReader reads lines of at most max bytes from r.
func NewReader(r io.Reader, max int) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), max: max}
}

// Read decodes the next line into v. io.EOF is the end, with nothing read.
func (r *Reader) Read(v any) error {
	var line []byte
	for {
		chunk, err := r.r.ReadSlice('\n')
		if len(line)+len(chunk) > r.max {
			return ErrTooLong
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		break
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("a message that does not parse: %w", err)
	}
	return nil
}

// Write encodes v as one line to w.
func Write(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxLine {
		return ErrTooLong
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
