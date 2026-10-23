package daemonproto

import (
	"context"
	"time"
)

// The read-only methods behind the web page's details: an environment's
// link log, what status shows of it, and doctor's findings. All are
// bounded, and everything in a result that came from a sandbox is
// printable.
const (
	MethodLinkLog    = "link_log"    // params LinkLogParams, result LinkLogResult
	MethodEnvDetails = "env_details" // params EnvParams, result EnvDetailsResult
	MethodDoctor     = "doctor"      // params EnvParams, result DoctorResult
)

// Bounds of these methods' fields.
const (
	DefaultLinkLogLines = 200
	MaxLinkLogLines     = 500
	// MaxLinkLogLine is the longest line of the link log, in bytes.
	MaxLinkLogLine = 1000
	// MaxDetailSessions, MaxDetailPorts, MaxDetailKeep and MaxDetailErrors
	// bound the lists of an env_details result.
	MaxDetailSessions = 64
	MaxDetailPorts    = 64
	MaxDetailKeep     = 256
	MaxDetailErrors   = 16
	// MaxDoctorFindings is the most findings a doctor result holds.
	MaxDoctorFindings = 500

	maxDetailText = 1024
)

// Image states, as env_details reports them.
const (
	ImageCurrent  = "current"
	ImageStale    = "stale"
	ImageSwitched = "switched"
	ImageUnknown  = "unknown"
)

// Doctor finding levels.
const (
	LevelOK        = "ok"
	LevelNote      = "note"
	LevelUnchecked = "unchecked"
	LevelProblem   = "problem"
)

// DoctorAsDaemon is the As of a result the daemon made: its checks ran in
// the daemon's environment (its PATH, its SSH agent), which can differ from
// a terminal's.
const DoctorAsDaemon = "daemon"

// LinkLogParams are the params of link_log. Lines is 1 to MaxLinkLogLines,
// and 0 (absent) is DefaultLinkLogLines.
type LinkLogParams struct {
	Env   string `json:"env"`
	Lines int    `json:"lines,omitempty"`
}

// Validate checks the params.
func (p *LinkLogParams) Validate() error {
	if !ValidEnv(p.Env) {
		return bad("bad environment name")
	}
	if p.Lines < 0 || p.Lines > MaxLinkLogLines {
		return bad("lines must be 1 to %d", MaxLinkLogLines)
	}
	return nil
}

// LinkLogResult is the result of link_log: the last lines of the link's
// log, oldest first. Truncated is set when the log held more than these.
type LinkLogResult struct {
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
}

// Validate checks the lines.
func (r *LinkLogResult) Validate() error {
	if len(r.Lines) > MaxLinkLogLines {
		return bad("more than %d link log lines", MaxLinkLogLines)
	}
	for _, l := range r.Lines {
		if err := checkText("link log line", l, MaxLinkLogLine); err != nil {
			return err
		}
	}
	return nil
}

// ImageDetail is the launcher's classification of the environment's image.
type ImageDetail struct {
	State string `json:"state"` // ImageCurrent, ImageStale, ImageSwitched or ImageUnknown
	Why   string `json:"why,omitempty"`
}

// ResourcesDetail is what is known of the sandbox's resources; a field
// that is not known is left out, never guessed.
type ResourcesDetail struct {
	MemoryBytes      int64 `json:"memory_bytes,omitempty"`
	MemoryLimitBytes int64 `json:"memory_limit_bytes,omitempty"`
	Vnodes           int   `json:"vnodes,omitempty"`
	VnodesMax        int   `json:"vnodes_max,omitempty"`
}

// PortDetail is a port the link forwards to the host.
type PortDetail struct {
	Port int    `json:"port"`
	URL  string `json:"url"`
}

// KeepDetail is a sandbox config [[keep]] entry: its path (~/...), and
// whether any sync rule applies to it.
type KeepDetail struct {
	Path string `json:"path"`
	Sync bool   `json:"sync"`
}

// DetailError says a part of env_details could not be had.
type DetailError struct {
	Part    string `json:"part"`
	Message string `json:"message"`
}

// EnvDetailsResult is the result of env_details. A part that fails is an
// entry of Errors; the rest still answers.
type EnvDetailsResult struct {
	Env       string `json:"env"`
	Isolation string `json:"isolation,omitempty"`
	// Started is when the running sandbox started, as its backend says;
	// absent when it does not run, or its backend cannot tell.
	Started   time.Time        `json:"started,omitzero"`
	Image     *ImageDetail     `json:"image,omitempty"`
	Sessions  []string         `json:"sessions"`
	Resources *ResourcesDetail `json:"resources,omitempty"`
	Ports     []PortDetail     `json:"ports"`
	Keep      []KeepDetail     `json:"keep"`
	Errors    []DetailError    `json:"errors,omitempty"`
}

// Validate checks the result.
func (r *EnvDetailsResult) Validate() error {
	if !ValidEnv(r.Env) {
		return bad("bad environment name in details")
	}
	if err := checkText("isolation", r.Isolation, maxDetailText); err != nil {
		return err
	}
	if r.Image != nil {
		switch r.Image.State {
		case ImageCurrent, ImageStale, ImageSwitched, ImageUnknown:
		default:
			return bad("bad image state in details")
		}
		if err := checkText("image reason", r.Image.Why, maxDetailText); err != nil {
			return err
		}
	}
	if len(r.Sessions) > MaxDetailSessions || len(r.Ports) > MaxDetailPorts || len(r.Keep) > MaxDetailKeep || len(r.Errors) > MaxDetailErrors {
		return bad("too many entries in details")
	}
	for _, s := range r.Sessions {
		if err := checkText("session", s, maxDetailText); err != nil {
			return err
		}
	}
	if x := r.Resources; x != nil && (x.MemoryBytes < 0 || x.MemoryLimitBytes < 0 || x.Vnodes < 0 || x.VnodesMax < 0) {
		return bad("negative resources")
	}
	for _, p := range r.Ports {
		if p.Port < 1 || p.Port > 65535 {
			return bad("bad port in details")
		}
		if err := checkText("port url", p.URL, maxDetailText); err != nil {
			return err
		}
	}
	for _, k := range r.Keep {
		if err := checkText("keep path", k.Path, maxDetailText); err != nil {
			return err
		}
	}
	for _, e := range r.Errors {
		if err := checkText("detail error part", e.Part, maxName); err != nil {
			return err
		}
		if err := checkText("detail error", e.Message, maxMessage); err != nil {
			return err
		}
	}
	return nil
}

// DoctorFinding is one row of doctor's checklist.
type DoctorFinding struct {
	Level   string `json:"level"` // LevelOK, LevelNote, LevelUnchecked or LevelProblem
	Label   string `json:"label"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// DoctorResult is the result of doctor: the findings caboose doctor would
// list, run by the daemon (As is DoctorAsDaemon), at most MaxDoctorFindings.
type DoctorResult struct {
	Findings []DoctorFinding `json:"findings"`
	RanAt    time.Time       `json:"ran_at"`
	As       string          `json:"as"`
	// Truncated is set when findings were left out of the limit.
	Truncated bool `json:"truncated,omitempty"`
}

// Validate checks the findings.
func (r *DoctorResult) Validate() error {
	if len(r.Findings) > MaxDoctorFindings {
		return bad("more than %d findings", MaxDoctorFindings)
	}
	if err := checkText("doctor as", r.As, maxName); err != nil {
		return err
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		switch f.Level {
		case LevelOK, LevelNote, LevelUnchecked, LevelProblem:
		default:
			return bad("bad finding level")
		}
		if err := checkText("finding label", f.Label, maxName); err != nil {
			return err
		}
		if err := checkText("finding", f.Message, maxDetailText); err != nil {
			return err
		}
		if err := checkText("finding fix", f.Fix, maxDetailText); err != nil {
			return err
		}
	}
	return nil
}

// LinkLog is the last lines of env's link log.
func (c *Client) LinkLog(ctx context.Context, env string, lines int) (*LinkLogResult, error) {
	p := &LinkLogParams{Env: env, Lines: lines}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	var r LinkLogResult
	if err := c.Call(ctx, MethodLinkLog, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// EnvDetails is env's details.
func (c *Client) EnvDetails(ctx context.Context, env string) (*EnvDetailsResult, error) {
	p := &EnvParams{Env: env}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	var r EnvDetailsResult
	if err := c.Call(ctx, MethodEnvDetails, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Doctor runs doctor for env in the daemon.
func (c *Client) Doctor(ctx context.Context, env string) (*DoctorResult, error) {
	p := &EnvParams{Env: env}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	var r DoctorResult
	if err := c.Call(ctx, MethodDoctor, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
