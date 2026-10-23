package conformance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/assets"
)

// StateRunning is what Sandbox.State says of a sandbox that is up.
const StateRunning = "running"

// Sandbox is the sandbox as a check sees it. The real one wraps the
// launcher's backend; the fake is a model filesystem.
type Sandbox interface {
	// Exec runs argv in the sandbox as user ("agent" or "root").
	Exec(ctx context.Context, user string, argv []string, stdin []byte) (rc int, stdout, stderr string, err error)
	Mounts() []Mount
	Labels() map[string]string
	State() string
}

// Mutation is one change the host was asked for, by path relative to B.
type Mutation struct {
	Op, Rel string
}

// Host is the host side of a run: canaries and their identities, all
// rooted at B. Verdicts are decided from it, never from the sandbox's
// report.
type Host interface {
	MakeCanary(parent, role string, kind Kind) (Canary, error)
	// Canary is the canary made for role.
	Canary(role string) (Canary, error)
	// Identity is rel's identity now, without following a symlink; a path
	// that is gone is an error wrapping fs.ErrNotExist.
	Identity(rel string) (Identity, error)
	// Snapshot is the identity of the canary's path and of everything below
	// it. A canary that is gone has an empty snapshot.
	Snapshot(c Canary) (map[string]Identity, error)
	// Mutations is every change the host was asked for, in order.
	Mutations() []Mutation
}

// Clock is the time the checks see.
type Clock interface{ Now() time.Time }

// Listener is a loopback TCP listener the suite runs.
type Listener interface {
	Addr() string
	Accepted() int
	Close() error
}

// Net opens loopback listeners.
type Net interface {
	Listen(ctx context.Context) (Listener, error)
}

// seal marks an Env as made by Steps for one check.
type seal struct {
	id    string
	class Class
	wipe  bool
}

// Env is what a check runs with. Every method but the plain getters fails
// with ErrNoEnvelope on an Env that Steps did not make for the check, so a
// check cannot do anything outside the runner.
type Env struct {
	sandbox          Sandbox
	host             Host
	clock            Clock
	net              Net
	isolation, trust string
	seal             *seal
}

// NewEnv bundles the interfaces. It is not usable until Steps seals it.
func NewEnv(sb Sandbox, h Host, c Clock, n Net, isolation, trust string) Env {
	return Env{sandbox: sb, host: h, clock: c, net: n, isolation: isolation, trust: trust}
}

func (e Env) Isolation() string { return e.isolation }
func (e Env) Trust() string     { return e.trust }

func (e Env) sealed() error {
	if e.seal == nil {
		return ErrNoEnvelope
	}
	return nil
}

func (e Env) Mounts() []Mount {
	if e.sealed() != nil || e.sandbox == nil {
		return nil
	}
	return e.sandbox.Mounts()
}

func (e Env) Labels() map[string]string {
	if e.sealed() != nil || e.sandbox == nil {
		return nil
	}
	return e.sandbox.Labels()
}

// ProbeHash is the sha256 of the probe binary the suite placed in the
// writable part, as it placed it: what the probe there must still be.
func (e Env) ProbeHash() (string, error) {
	if err := e.sealed(); err != nil {
		return "", err
	}
	h, ok := e.host.(interface{ ProbeHash() string })
	if !ok || h.ProbeHash() == "" {
		return "", errors.New("conformance: the suite placed no probe")
	}
	return h.ProbeHash(), nil
}

// Nonce is the run's nonce, which the probe's gate is given.
func (e Env) Nonce() (string, error) {
	if err := e.sealed(); err != nil {
		return "", err
	}
	h, ok := e.host.(interface{ Nonce() string })
	if !ok || !nonceWordRe.MatchString(h.Nonce()) {
		return "", errors.New("conformance: the host has no nonce for the run")
	}
	return h.Nonce(), nil
}

// Lister is a Host that lists every canary it made.
type Lister interface {
	// Canaries are every canary made, each once, by Rel.
	Canaries() []Canary
}

// Canaries are every canary the host made, sorted by Rel: what a check
// that must account for all of them (the wipe) walks.
func (e Env) Canaries() ([]Canary, error) {
	if err := e.sealed(); err != nil {
		return nil, err
	}
	l, ok := e.host.(Lister)
	if !ok {
		return nil, errors.New("conformance: the host cannot list its canaries")
	}
	out := l.Canaries()
	slices.SortFunc(out, func(a, b Canary) int { return strings.Compare(a.Rel, b.Rel) })
	return out, nil
}

// Recorder is a Host that keeps what it last recorded of each canary
// path: as made, then as each post-check found it after a check it was
// handed to.
type Recorder interface {
	Recorded(rel string) (id Identity, gone bool, err error)
}

// Recorded is the host's record of the canary path rel: its identity, and
// whether a check it was handed to removed it. What the runner's
// post-checks accepted, so a change made since (by a restart, or by what a
// check left running) shows against it.
func (e Env) Recorded(rel string) (Identity, bool, error) {
	if err := e.sealed(); err != nil {
		return Identity{}, false, err
	}
	r, ok := e.host.(Recorder)
	if !ok {
		return Identity{}, false, errors.New("conformance: the host keeps no record of its canaries")
	}
	return r.Recorded(rel)
}

// SameIdentity compares two identities as the post-check does: a
// directory by device, inode and mode alone, anything else in full.
func SameIdentity(a, b Identity) bool { return a.sameAs(b) }

// InWritablePart reports whether canary c is in the writable part of the
// suite's writable root: the one place the sandbox may remove it.
func InWritablePart(c Canary) bool {
	m := relRe.FindStringSubmatch(c.Rel)
	return m != nil && strings.HasSuffix(m[1], "-dev") && strings.HasPrefix(m[2], WritablePart+"/")
}

// RootUser is the sandbox's LabelUser when it runs as root: under vm, and
// under gVisor on an engine where the agent cannot write its mounts.
const RootUser = "0:0"

// SessionUser is the user the sandbox's sessions, and its link, run as:
// "agent" where the container runs as the image's user, "root" where it
// runs as root, which the label LabelUser records. A check that does what
// a session does (a positive control, git, a link command) runs as it,
// since there the agent user is no one: the link's socket and the mounts
// are root's. "" when the label is missing, says neither, or says what
// the isolation never is (root under container, whose runc root is the
// host's; the agent under vm, which runs as root), which RunCmd refuses
// and SessionUserError explains: caboose sets it on every sandbox.
func (e Env) SessionUser() string {
	u, ok := e.Labels()[assets.LabelUser]
	switch {
	case !ok:
		return ""
	case u == "" && e.isolation != "vm":
		return "agent"
	case u == RootUser && e.isolation != "container":
		return "root"
	}
	return ""
}

// SessionUserError is why SessionUser is "", nil when it is not.
func (e Env) SessionUserError() error {
	if e.SessionUser() != "" {
		return nil
	}
	u, ok := e.Labels()[assets.LabelUser]
	if !ok {
		return fmt.Errorf("the sandbox has no label %s, which says who its sessions run as", assets.LabelUser)
	}
	return fmt.Errorf("the sandbox's label %s is %q, which is no user caboose runs a %s sandbox as", assets.LabelUser, u, e.isolation)
}

// State is the sandbox's state; "unavailable" outside the envelope.
func (e Env) State() string {
	if e.sealed() != nil || e.sandbox == nil {
		return "unavailable"
	}
	return e.sandbox.State()
}

func (e Env) Canary(role string) (Canary, error) {
	if err := e.sealed(); err != nil {
		return Canary{}, err
	}
	return e.host.Canary(role)
}

func (e Env) Identity(rel string) (Identity, error) {
	if err := e.sealed(); err != nil {
		return Identity{}, err
	}
	return e.host.Identity(rel)
}

func (e Env) Snapshot(c Canary) (map[string]Identity, error) {
	if err := e.sealed(); err != nil {
		return nil, err
	}
	return e.host.Snapshot(c)
}

func (e Env) Now() time.Time {
	if e.sealed() != nil || e.clock == nil {
		return time.Time{}
	}
	return e.clock.Now()
}

func (e Env) Listen(ctx context.Context) (Listener, error) {
	if err := e.sealed(); err != nil {
		return nil, err
	}
	if e.net == nil {
		return nil, fmt.Errorf("conformance: no network")
	}
	return e.net.Listen(ctx)
}

// CheckResult is what a check returns.
type CheckResult struct {
	Status Status
	Reason string
	// Lines are extra information for the results file, one per line.
	Lines []string
}

// Check is one control's test.
type Check struct {
	ID, Title string
	// Control is the control it tests, Step the area of caboose it tests.
	Control, Step string
	// Isolations and Trusts say where it applies; empty means everywhere.
	Isolations, Trusts []string
	Class              Class
	Group              string
	// Wipe marks the deliberately broad wipe (Class must be Destructive).
	Wipe bool
	// Fresh asks for a freshly restarted sandbox first.
	Fresh bool
	// Roles are the canary roles the check may change.
	Roles []string
	Run   func(ctx context.Context, env Env) CheckResult
}

// Destructive reports whether the check removes or replaces canaries.
func (c Check) Destructive() bool { return c.Class == Destructive }

// Applies reports whether the check is for this isolation and trust.
func (c Check) Applies(isolation, trust string) bool {
	return (len(c.Isolations) == 0 || slices.Contains(c.Isolations, isolation)) &&
		(len(c.Trusts) == 0 || slices.Contains(c.Trusts, trust))
}

// Collector keeps each step's full CheckResult, which the runner's
// Outcome has no room for.
type Collector struct {
	mu  sync.Mutex
	got map[string]CheckResult
}

func (c *Collector) put(id string, r CheckResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.got == nil {
		c.got = map[string]CheckResult{}
	}
	c.got[id] = r
}

// Lines are the extra lines the check with id returned.
func (c *Collector) Lines(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got[id].Lines)
}

// Steps turns the checks that apply to the environment's isolation and
// trust into runner steps. The only way a check's Run is called is the
// step this returns, which the Runner runs inside the envelope, with an Env
// sealed for that check alone: Verify before every group, VerifyDestructive
// before every destructive step, PostCheck after each. The canaries a step
// may change are those of its Roles that exist; a wipe step's are every
// canary of the run, and the probe in the writable part, whatever its
// Roles: it removes the whole part, and it compares every other canary
// from the host itself, so that one it destroyed is its FAIL and not the
// post-check's ERROR. What is no canary stays the post-check's.
func Steps(checks []Check, env Env, c *Collector) ([]Step, error) {
	if env.sandbox == nil || env.host == nil {
		return nil, fmt.Errorf("conformance: an Env needs a sandbox and a host")
	}
	if c == nil {
		return nil, fmt.Errorf("conformance: no collector")
	}
	var out []Step
	for _, ck := range checks {
		if ck.Run == nil || ck.ID == "" {
			return nil, fmt.Errorf("conformance: check %q has no id or nothing to run", ck.ID)
		}
		if ck.Wipe && ck.Class != Destructive {
			return nil, fmt.Errorf("conformance: wipe check %s is not destructive", ck.ID)
		}
		if !ck.Applies(env.isolation, env.trust) {
			continue
		}
		var canaries []string
		for _, role := range ck.Roles {
			if cn, err := env.host.Canary(role); err == nil {
				canaries = append(canaries, cn.Rel)
			}
		}
		if ck.Wipe {
			l, ok := env.host.(Lister)
			if !ok {
				return nil, fmt.Errorf("conformance: wipe check %s needs a host that lists its canaries", ck.ID)
			}
			for _, cn := range l.Canaries() {
				if !slices.Contains(canaries, cn.Rel) {
					canaries = append(canaries, cn.Rel)
				}
			}
			if h, ok := env.host.(interface{ Nonce() string }); ok && nonceWordRe.MatchString(h.Nonce()) {
				if p, err := ProbeRel(h.Nonce()[:8]); err == nil {
					canaries = append(canaries, p)
				}
			}
		}
		ck := ck
		sealed := env
		sealed.seal = &seal{id: ck.ID, class: ck.Class, wipe: ck.Wipe}
		out = append(out, Step{
			ID: ck.ID, Group: ck.Group, Class: ck.Class, Wipe: ck.Wipe, Fresh: ck.Fresh,
			Canaries: canaries,
			Run: func(ctx context.Context) (o Outcome) {
				defer func() {
					if r := recover(); r != nil {
						res := CheckResult{Status: Error, Reason: fmt.Sprintf("the check panicked: %v", r)}
						c.put(ck.ID, res)
						o = Outcome{res.Status, res.Reason}
					}
				}()
				res := ck.Run(ctx, sealed)
				c.put(ck.ID, res)
				return Outcome{res.Status, res.Reason}
			},
		})
	}
	return out, nil
}

// RunOrder is the order the runner runs checks in (Order): every check
// that is not destructive, by group, then every destructive one. It runs
// nothing; the plan a run prints is in this order.
func RunOrder(checks []Check) ([]Check, error) {
	steps := make([]Step, 0, len(checks))
	byID := make(map[string]Check, len(checks))
	for _, ck := range checks {
		byID[ck.ID] = ck
		steps = append(steps, Step{ID: ck.ID, Group: ck.Group, Class: ck.Class, Wipe: ck.Wipe,
			Run: func(context.Context) Outcome { return Outcome{} }})
	}
	order, err := Order(steps)
	if err != nil {
		return nil, err
	}
	out := make([]Check, 0, len(order))
	for _, s := range order {
		out = append(out, byID[s.ID])
	}
	return out, nil
}

// RunChecks is the one way checks are run: Steps, then the Runner.
func RunChecks(ctx context.Context, r *Runner, checks []Check, env Env) ([]Result, *Collector, error) {
	col := &Collector{}
	steps, err := Steps(checks, env, col)
	if err != nil {
		return nil, col, err
	}
	res, err := r.Run(ctx, steps)
	return res, col, err
}

// diffTrees lists the paths whose identity differs between two snapshots
// of a canary: changed, gone or new.
func diffTrees(before, after map[string]Identity) []string {
	var out []string
	for p, b := range before {
		if a, ok := after[p]; !ok || !a.sameAs(b) {
			out = append(out, p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}
