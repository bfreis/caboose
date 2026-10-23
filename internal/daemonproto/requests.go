package daemonproto

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The methods that answer for an environment's request broker. They are
// additions to protocol 1: a daemon that predates them answers
// KindUnknownMethod, and the client says to run `caboose daemon restart`.
const (
	MethodRequests    = "requests"    // the environment's requests, pending or all
	MethodRequest     = "request"     // one request, in full
	MethodAnswer      = "answer"      // allow or deny a pending request
	MethodGrants      = "grants"      // the grants in force
	MethodRevoke      = "revoke"      // end a grant
	MethodPermissions = "permissions" // what the sandbox may do, as configured and granted
)

// Limits of the broker methods' params and results.
const (
	// DefaultRequestsLimit is how many requests a listing gives when the
	// client names no limit, and MaxRequestsLimit the most it may ask for.
	DefaultRequestsLimit = 50
	MaxRequestsLimit     = 200
	// MaxHours is the longest an answer may grant, in hours.
	MaxHours = 168
	// MaxGrants is the most grants a listing holds.
	MaxGrants = 500
	// MaxDiff is the longest config.toml diff an answer carries.
	MaxDiff = 64 << 10

	// resultBudget is what a list in a result may take of a frame: the
	// rest of MaxLine is the frame around it and the other fields.
	resultBudget = 900 << 10
)

// Answer verdicts and scopes, as the broker spells them.
const (
	VerdictAllow = "allow"
	VerdictDeny  = "deny"
	ScopeOnce    = "once"
	ScopeHours   = "hours"
	ScopeAlways  = "always"
)

// RequestsParams are the params of requests.
type RequestsParams struct {
	Env string `json:"env"`
	// All lists decided requests too; otherwise only pending ones.
	All bool `json:"all,omitempty"`
	// Limit is the most to list: 0 is DefaultRequestsLimit.
	Limit int `json:"limit,omitempty"`
}

// Validate checks the params.
func (p *RequestsParams) Validate() error {
	if !ValidEnv(p.Env) {
		return fmt.Errorf("bad environment name")
	}
	if p.Limit < 0 || p.Limit > MaxRequestsLimit {
		return fmt.Errorf("limit is 1 to %d", MaxRequestsLimit)
	}
	return nil
}

// RequestParams are the params of request.
type RequestParams struct {
	Env string `json:"env"`
	ID  uint64 `json:"id"`
}

// Validate checks the params.
func (p *RequestParams) Validate() error {
	if !ValidEnv(p.Env) {
		return fmt.Errorf("bad environment name")
	}
	if p.ID == 0 {
		return fmt.Errorf("a request number starts at 1")
	}
	return nil
}

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AnswerParams are the params of answer. There is no field for who
// answers: the daemon takes that from the caller (a socket connection's
// hello), never from what the client says.
type AnswerParams struct {
	Env     string `json:"env"`
	ID      uint64 `json:"id"`
	Verdict string `json:"verdict"`
	// Scope and Hours say how far an allow reaches; a deny has neither.
	Scope string `json:"scope,omitempty"`
	Hours int    `json:"hours,omitempty"`
	// ConfirmDiff is, for scope always, the hash of the diff the person
	// was shown (the "always" Offer's Hash), and required: the daemon
	// writes only when it equals the hash of the diff it would write now,
	// and answers KindStaleDiff otherwise.
	ConfirmDiff string `json:"confirm_diff,omitempty"`
}

// Validate checks the params.
func (p *AnswerParams) Validate() error {
	if !ValidEnv(p.Env) {
		return fmt.Errorf("bad environment name")
	}
	if p.ID == 0 {
		return fmt.Errorf("a request number starts at 1")
	}
	switch p.Verdict {
	case VerdictDeny:
		if p.Scope != "" || p.Hours != 0 || p.ConfirmDiff != "" {
			return fmt.Errorf("a deny has no scope, hours or confirmation")
		}
		return nil
	case VerdictAllow:
	default:
		return fmt.Errorf("verdict is %q or %q", VerdictAllow, VerdictDeny)
	}
	switch p.Scope {
	case ScopeOnce:
		if p.Hours != 0 {
			return fmt.Errorf("hours go with scope %q", ScopeHours)
		}
	case ScopeHours:
		if p.Hours < 1 || p.Hours > MaxHours {
			return fmt.Errorf("hours is 1 to %d", MaxHours)
		}
	case ScopeAlways:
		if p.Hours != 0 {
			return fmt.Errorf("hours go with scope %q", ScopeHours)
		}
		if p.ConfirmDiff == "" {
			return fmt.Errorf("scope %q needs confirm_diff, the hash of the diff shown (the request's %q offer)", ScopeAlways, ScopeAlways)
		}
	default:
		return fmt.Errorf("scope is %q, %q or %q", ScopeOnce, ScopeHours, ScopeAlways)
	}
	if p.Scope != ScopeAlways && p.ConfirmDiff != "" {
		return fmt.Errorf("a confirmation goes with scope %q", ScopeAlways)
	}
	if p.ConfirmDiff != "" && !hashRe.MatchString(p.ConfirmDiff) {
		return fmt.Errorf("a confirmation is a sha256 in hex")
	}
	return nil
}

var grantIDRe = regexp.MustCompile(`^g[0-9]{1,18}$`)

// RevokeParams are the params of revoke.
type RevokeParams struct {
	Env   string `json:"env"`
	Grant string `json:"grant"`
}

// Validate checks the params.
func (p *RevokeParams) Validate() error {
	if !ValidEnv(p.Env) {
		return fmt.Errorf("bad environment name")
	}
	if !grantIDRe.MatchString(p.Grant) {
		return fmt.Errorf("a grant is named g and a number")
	}
	return nil
}

// Ask is what the sandbox said of a request: its words, kept apart from
// the host's own account of it (RequestView.Effect), and shown under
// "the sandbox says:". The daemon makes it printable and clips it; the
// client checks it again.
type Ask struct {
	Context        string   `json:"context,omitempty"`
	Options        []string `json:"options,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Meanwhile      string   `json:"meanwhile,omitempty"`
}

// Bounds of the sandbox's text in a result (the broker holds it to less).
const (
	maxAskText    = 2048
	maxAskOptions = 5
	maxAskOption  = 256
	maxViewText   = 4096
	maxKeyText    = 640
)

func (a *Ask) validate() error {
	if err := checkText("ask context", a.Context, maxAskText); err != nil {
		return err
	}
	if err := checkText("ask recommendation", a.Recommendation, maxAskText); err != nil {
		return err
	}
	if err := checkText("ask meanwhile", a.Meanwhile, maxAskText); err != nil {
		return err
	}
	if len(a.Options) > maxAskOptions {
		return bad("more than %d ask options", maxAskOptions)
	}
	for _, o := range a.Options {
		if err := checkText("ask option", o, maxAskOption); err != nil {
			return err
		}
	}
	return nil
}

// RequestView is one request. Effect and Reason are the host's words, Ask
// the sandbox's; Key and Session come from the sandbox too, so a client
// shows them as claims.
type RequestView struct {
	ID        uint64    `json:"id"`
	Kind      string    `json:"kind"`
	Key       string    `json:"key,omitempty"`
	Effect    string    `json:"effect,omitempty"`
	State     string    `json:"state"`
	Scope     string    `json:"scope,omitempty"`
	Hours     int       `json:"hours,omitempty"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires,omitzero"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
	// DecidedBy is who answered: "cli" (the kind of client the answer
	// came through, which the daemon sets, never the answer), "dialog"
	// (the URL dialog) or "expiry".
	DecidedBy string `json:"decided_by,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Seq       uint64 `json:"seq,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
	Session   string `json:"session,omitempty"`
	Ask       *Ask   `json:"ask,omitempty"`
}

// Validate checks the strings of a view, which the client prints.
func (r *RequestView) Validate() error {
	for _, c := range []struct {
		what, s string
		max     int
	}{{"request kind", r.Kind, maxName}, {"request state", r.State, maxName}, {"request scope", r.Scope, maxName},
		{"request key", r.Key, maxKeyText}, {"request effect", r.Effect, maxViewText}, {"request reason", r.Reason, maxViewText},
		{"request decider", r.DecidedBy, maxName}, {"request session", r.Session, 128}} {
		if err := checkText(c.what, c.s, c.max); err != nil {
			return err
		}
	}
	if r.ID == 0 {
		return bad("request without a number")
	}
	if r.Ask != nil {
		return r.Ask.validate()
	}
	return nil
}

// RequestsResult is the result of requests.
type RequestsResult struct {
	Requests []RequestView `json:"requests"`
	// Truncated is set when the daemon left requests out to fit a frame.
	Truncated bool `json:"truncated,omitempty"`
}

// Validate checks a listing.
func (r *RequestsResult) Validate() error {
	if len(r.Requests) > MaxRequestsLimit {
		return bad("more than %d requests", MaxRequestsLimit)
	}
	for i := range r.Requests {
		if err := r.Requests[i].Validate(); err != nil {
			return err
		}
	}
	return nil
}

// FitRequests keeps of views, in order, as many as fit a frame with room
// to spare, and says whether it left any out. The sandbox decides how
// long its requests' text is, up to the broker's own bounds; a listing of
// the longest is far over MaxLine.
func FitRequests(views []RequestView) ([]RequestView, bool) {
	used := 0
	for i := range views {
		b, err := json.Marshal(&views[i])
		if err != nil || used+len(b)+1 > resultBudget {
			return views[:i], true
		}
		used += len(b) + 1
	}
	return views, false
}

// RequestResult is the result of request.
type RequestResult struct {
	Request RequestView `json:"request"`
	// Offers are the ways of allowing the request the host offers now,
	// once, hours, always in that order, each with its effect; a pending
	// request's alone. An answer that allows with a scope not among them
	// is refused.
	Offers []Offer `json:"offers,omitempty"`
	// Withheld are the scopes not offered, and why.
	Withheld []Withheld `json:"withheld,omitempty"`
}

// MaxOfferDiffLines is the most lines an "always" offer's diff has.
const MaxOfferDiffLines = 200

// Offer is one way of allowing a request.
type Offer struct {
	Scope string `json:"scope"`
	// Effect is what allowing so does, in the host's words; for scope
	// hours it holds "{hours}" where the hours go.
	Effect string `json:"effect"`
	// HoursMin and HoursMax bound an hours answer.
	HoursMin int `json:"hours_min,omitempty"`
	HoursMax int `json:"hours_max,omitempty"`
	// Diff is, for scope always, the change to config.toml exactly as it
	// would be written now, line by line, and Hash what an answer's
	// confirm_diff must be to have it written.
	Diff []string `json:"diff,omitempty"`
	Hash string   `json:"hash,omitempty"`
}

// Withheld is a scope the host does not offer for a request.
type Withheld struct {
	Scope string `json:"scope"`
	Why   string `json:"why"`
}

func validScope(s string) bool { return s == ScopeOnce || s == ScopeHours || s == ScopeAlways }

func (o *Offer) validate() error {
	if !validScope(o.Scope) {
		return bad("bad offer scope")
	}
	if err := checkText("offer effect", o.Effect, maxViewText); err != nil {
		return err
	}
	if o.Effect == "" {
		return bad("an offer without its effect")
	}
	if o.Scope == ScopeHours {
		if o.HoursMin < 1 || o.HoursMax > MaxHours || o.HoursMin > o.HoursMax {
			return bad("bad offer hours")
		}
	} else if o.HoursMin != 0 || o.HoursMax != 0 {
		return bad("hours go with scope %q", ScopeHours)
	}
	if o.Scope != ScopeAlways {
		if len(o.Diff) > 0 || o.Hash != "" {
			return bad("a diff goes with scope %q", ScopeAlways)
		}
		return nil
	}
	if !hashRe.MatchString(o.Hash) {
		return bad("an always offer without its diff's hash")
	}
	if len(o.Diff) > MaxOfferDiffLines {
		return bad("an offer's diff over %d lines", MaxOfferDiffLines)
	}
	n := 0
	for _, l := range o.Diff {
		n += len(l) + 1
		if err := checkText("offer diff", l, MaxDiff); err != nil {
			return err
		}
	}
	if n > MaxDiff {
		return bad("an offer's diff over %d bytes", MaxDiff)
	}
	return nil
}

// DiffLines is a config.toml diff as the protocol carries it: one
// printable line each, tabs made spaces.
func DiffLines(diff string) []string {
	var out []string
	if d := strings.TrimSuffix(diff, "\n"); d != "" {
		for _, l := range strings.Split(d, "\n") {
			out = append(out, Text(strings.ReplaceAll(l, "\t", " "), MaxDiff))
		}
	}
	return out
}

// Validate checks the view and the offers.
func (r *RequestResult) Validate() error {
	if err := r.Request.Validate(); err != nil {
		return err
	}
	if len(r.Offers) > 3 || len(r.Withheld) > 3 {
		return bad("too many offers")
	}
	seen := map[string]bool{}
	for i := range r.Offers {
		if err := r.Offers[i].validate(); err != nil {
			return err
		}
		if seen[r.Offers[i].Scope] {
			return bad("scope %q offered twice", r.Offers[i].Scope)
		}
		seen[r.Offers[i].Scope] = true
	}
	for _, w := range r.Withheld {
		if !validScope(w.Scope) || seen[w.Scope] {
			return bad("bad withheld scope")
		}
		if err := checkText("withheld why", w.Why, maxViewText); err != nil {
			return err
		}
		seen[w.Scope] = true
	}
	return nil
}

// Offered is the offer of scope among r's, or nil.
func (r *RequestResult) Offered(scope string) *Offer {
	for i := range r.Offers {
		if r.Offers[i].Scope == scope {
			return &r.Offers[i]
		}
	}
	return nil
}

// AnswerResult is what an answer did.
type AnswerResult struct {
	// Request is the request as it stands after the answer.
	Request RequestView `json:"request"`
	// Effect says what the answer does, or did, in the host's words.
	Effect string `json:"effect,omitempty"`
	// Diff and DiffHash are, for scope always, the change to config.toml
	// that was written and its hash. Recorded is set once the answer is.
	Diff     string `json:"diff,omitempty"`
	DiffHash string `json:"diff_hash,omitempty"`
	Recorded bool   `json:"recorded"`
	// Grant is the grant the answer made.
	Grant string `json:"grant,omitempty"`
	// Also are the other requests the answer decided with it.
	Also []uint64 `json:"also,omitempty"`
	// OpenError is why the URL an allowed URL request named did not open.
	OpenError string `json:"open_error,omitempty"`
}

// Validate checks an answer's strings.
func (r *AnswerResult) Validate() error {
	if err := r.Request.Validate(); err != nil {
		return err
	}
	if err := checkText("effect", r.Effect, maxViewText); err != nil {
		return err
	}
	if err := checkLines("diff", r.Diff, MaxDiff); err != nil {
		return err
	}
	if r.DiffHash != "" && !hashRe.MatchString(r.DiffHash) {
		return bad("bad diff hash")
	}
	if r.Grant != "" && !grantIDRe.MatchString(r.Grant) {
		return bad("bad grant name")
	}
	if len(r.Also) > MaxRequestsLimit*4 {
		return bad("too many requests decided together")
	}
	return checkText("open error", r.OpenError, maxViewText)
}

// Lines is s as a peer takes a text of several lines: like Text, but a
// newline stays and a tab becomes a space. It is cut to at most max bytes.
func Lines(s string, max int) string {
	parts := strings.Split(strings.ReplaceAll(s, "\t", " "), "\n")
	for i, p := range parts {
		parts[i] = Text(p, len(p)+1)
	}
	s = strings.Join(parts, "\n")
	if len(s) > max {
		s = Text(s[:max-3], max) + "..."
	}
	return s
}

// checkLines is checkText for a text of several lines: newlines are fine.
func checkLines(what, s string, max int) error {
	if len(s) > max {
		return bad("%s over %d bytes", what, max)
	}
	for _, line := range strings.Split(s, "\n") {
		if !printable(line) {
			return bad("%s has control or invalid characters", what)
		}
	}
	return nil
}

// GrantView is a grant in force.
type GrantView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Key         string    `json:"key"`
	Once        bool      `json:"once,omitempty"`
	Addrs       []string  `json:"addrs,omitempty"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
	LeftSeconds int64     `json:"left_seconds"`
	Request     uint64    `json:"request,omitempty"`
}

func (g *GrantView) validate() error {
	if !grantIDRe.MatchString(g.ID) {
		return bad("bad grant name")
	}
	if err := checkText("grant kind", g.Kind, maxName); err != nil {
		return err
	}
	if err := checkText("grant key", g.Key, maxKeyText); err != nil {
		return err
	}
	if len(g.Addrs) > 64 {
		return bad("too many addresses in a grant")
	}
	for _, a := range g.Addrs {
		if err := checkText("grant address", a, 64); err != nil {
			return err
		}
	}
	return nil
}

// GrantsResult is the result of grants.
type GrantsResult struct {
	Grants    []GrantView `json:"grants"`
	Truncated bool        `json:"truncated,omitempty"`
}

// Validate checks a listing.
func (r *GrantsResult) Validate() error {
	if len(r.Grants) > MaxGrants {
		return bad("more than %d grants", MaxGrants)
	}
	for i := range r.Grants {
		if err := r.Grants[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

// RevokeResult is the result of revoke.
type RevokeResult struct {
	Revoked string `json:"revoked"`
}

// Validate checks the name.
func (r *RevokeResult) Validate() error {
	if !grantIDRe.MatchString(r.Revoked) {
		return bad("bad grant name")
	}
	return nil
}

// RootPerm is a root the sandbox mounts, and how far it can write it.
type RootPerm struct {
	Name      string `json:"name,omitempty"`
	Host      string `json:"host"`
	Container string `json:"container"`
	// ReadOnly is set when the sandbox can write none of the root.
	ReadOnly bool `json:"read_only,omitempty"`
	// Writable are the parts of the root it can write, as written in
	// config.toml ("." is the whole root).
	Writable []string `json:"writable,omitempty"`
}

// AliasPerm is an alias: a host command the sandbox may ask to run.
type AliasPerm struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Command     []string `json:"command"`
	Cwd         string   `json:"cwd,omitempty"`
	Policy      string   `json:"policy"`
}

// HostExecPerm is whether sessions may run commands on this machine.
type HostExecPerm struct {
	// Configured is host_exec in config.toml, Effective what is in force
	// after the trust level, and Why says when they differ.
	Configured bool   `json:"configured"`
	Effective  bool   `json:"effective"`
	Why        string `json:"why,omitempty"`
}

// OpenURLsPerm is how the sandbox's URLs are opened.
type OpenURLsPerm struct {
	// Mode is open_urls: "ask", "allow" or "off".
	Mode string `json:"mode"`
	// Allow are the origins opened without asking (open_urls_allow).
	Allow []string `json:"allow,omitempty"`
	// Why says when the trust level overrides them.
	Why string `json:"why,omitempty"`
}

// EgressPerm is the sandbox's outbound connections.
type EgressPerm struct {
	// On is whether the VM's connections are dialled from this machine.
	On    bool     `json:"on"`
	Ports string   `json:"ports,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
	// Default says what happens to a host no entry names, at the trust
	// level in force.
	Default string `json:"default,omitempty"`
	// Network says how the sandbox reaches the network at all.
	Network string `json:"network,omitempty"`
}

// ModsPerm is what the agent last counted of Claude Code sessions.
type ModsPerm struct {
	Sessions int `json:"sessions"`
	WithMod  int `json:"with_mod"`
}

// PermissionsResult is what the sandbox may do, as configured and as
// granted.
type PermissionsResult struct {
	Env   string `json:"env"`
	Trust string `json:"trust"`
	// Isolation is container, gvisor or vm, and Profile the profile in use
	// ("vm.default"), "" when config.toml defines none.
	Isolation string       `json:"isolation"`
	Profile   string       `json:"profile,omitempty"`
	Roots     []RootPerm   `json:"roots,omitempty"`
	Aliases   []AliasPerm  `json:"aliases,omitempty"`
	HostExec  HostExecPerm `json:"host_exec"`
	OpenURLs  OpenURLsPerm `json:"open_urls"`
	Egress    EgressPerm   `json:"egress"`
	// Grants are the timed and once grants in force.
	Grants []GrantView `json:"grants,omitempty"`
	// Mods is set when the agent has counted sessions.
	Mods *ModsPerm `json:"mods,omitempty"`
	// Drift says, a sentence each, how the sandbox as it was created
	// differs from config.toml in what only a restart changes: its roots
	// and their writable parts, its trust level and the VM's network.
	// The rest of the report is config.toml's, so what Drift names takes
	// effect at the next restart. Empty when they agree or there is no
	// sandbox to ask.
	Drift []string `json:"drift,omitempty"`
}

const maxPermList = 256

// MaxPermDrift is the most drift sentences a permissions report carries.
const MaxPermDrift = 8

func checkList(what string, l []string, max int) error {
	if len(l) > maxPermList {
		return bad("%s over %d entries", what, maxPermList)
	}
	for _, s := range l {
		if err := checkText(what, s, max); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the strings of a permissions report, which the client
// prints: config.toml's, and the sandbox's own claims in none of them.
func (r *PermissionsResult) Validate() error {
	if !ValidEnv(r.Env) {
		return bad("bad environment name")
	}
	for _, c := range []struct {
		what, s string
		max     int
	}{{"trust", r.Trust, maxName}, {"isolation", r.Isolation, maxName}, {"profile", r.Profile, maxName},
		{"host exec why", r.HostExec.Why, maxViewText}, {"open_urls", r.OpenURLs.Mode, maxName}, {"open_urls why", r.OpenURLs.Why, maxViewText},
		{"egress ports", r.Egress.Ports, 512}, {"egress default", r.Egress.Default, maxViewText},
		{"egress network", r.Egress.Network, maxViewText}} {
		if err := checkText(c.what, c.s, c.max); err != nil {
			return err
		}
	}
	if len(r.Roots) > maxPermList || len(r.Aliases) > maxPermList {
		return bad("too many roots or aliases")
	}
	for _, ro := range r.Roots {
		if err := checkText("root", ro.Name+ro.Host+ro.Container, 3072); err != nil {
			return err
		}
		if err := checkList("root part", ro.Writable, 1024); err != nil {
			return err
		}
	}
	for _, a := range r.Aliases {
		if err := checkText("alias", a.Name+a.Description+a.Cwd+a.Policy, 4096); err != nil {
			return err
		}
		if err := checkList("alias command", a.Command, 1024); err != nil {
			return err
		}
	}
	for what, l := range map[string][]string{"open_urls_allow": r.OpenURLs.Allow, "egress_allow": r.Egress.Allow, "egress_deny": r.Egress.Deny} {
		if err := checkList(what, l, 512); err != nil {
			return err
		}
	}
	if len(r.Drift) > MaxPermDrift {
		return bad("more than %d drift notes", MaxPermDrift)
	}
	for _, d := range r.Drift {
		if d == "" {
			return bad("an empty drift note")
		}
		if err := checkText("drift", d, maxViewText); err != nil {
			return err
		}
	}
	if len(r.Grants) > MaxGrants {
		return bad("more than %d grants", MaxGrants)
	}
	for i := range r.Grants {
		if err := r.Grants[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

// checkParams refuses params the daemon would refuse, before anything is
// sent.
func checkParams(p interface{ Validate() error }) error {
	if err := p.Validate(); err != nil {
		return NewError(KindBadRequest, "%v", err)
	}
	return nil
}

// Requests lists env's requests.
func (c *Client) Requests(ctx context.Context, env string, all bool, limit int) (*RequestsResult, error) {
	var r RequestsResult
	p := &RequestsParams{Env: env, All: all, Limit: limit}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, MethodRequests, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Request returns request id of env, in full.
func (c *Client) Request(ctx context.Context, env string, id uint64) (*RequestResult, error) {
	var r RequestResult
	p := &RequestParams{Env: env, ID: id}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, MethodRequest, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Answer answers request id of env.
func (c *Client) Answer(ctx context.Context, p AnswerParams) (*AnswerResult, error) {
	var r AnswerResult
	if err := checkParams(&p); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, MethodAnswer, &p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Grants lists the grants in force in env.
func (c *Client) Grants(ctx context.Context, env string) (*GrantsResult, error) {
	var r GrantsResult
	p := &EnvParams{Env: env}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, MethodGrants, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Revoke ends grant id of env.
func (c *Client) Revoke(ctx context.Context, env, grant string) (*RevokeResult, error) {
	var r RevokeResult
	p := &RevokeParams{Env: env, Grant: grant}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, MethodRevoke, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Permissions returns what env's sandbox may do.
func (c *Client) Permissions(ctx context.Context, env string) (*PermissionsResult, error) {
	var r PermissionsResult
	p := &EnvParams{Env: env}
	if err := checkParams(p); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, MethodPermissions, p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
