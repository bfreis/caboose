package conformance

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Class is how much a check can change. The runner runs a group's checks
// in class order, observe then write, and every destructive check of the
// run after all of them.
type Class int

const (
	Observe     Class = iota // reads only
	Write                    // tries to change a canary, which must be refused
	Destructive              // tries to remove or replace canaries
)

func (c Class) String() string {
	switch c {
	case Observe:
		return "observe"
	case Write:
		return "write"
	case Destructive:
		return "destructive"
	}
	return fmt.Sprintf("class(%d)", int(c))
}

// Status is a check's outcome.
type Status string

const (
	Pass  Status = "PASS"
	Fail  Status = "FAIL"
	Error Status = "ERROR"
	Gap   Status = "GAP"
	Skip  Status = "SKIP"
	Info  Status = "INFO" // a fact recorded, not a verdict
)

// Outcome is what a check returns, decided from the host.
type Outcome struct {
	Status Status
	Reason string
}

// Step is one check as the runner sees it.
type Step struct {
	ID, Group string
	Class     Class
	// Wipe marks the deliberately broad wipe, which runs only when the
	// run allows it.
	Wipe bool
	// Fresh asks for a freshly restarted sandbox, re-audited and
	// re-pinned, before it runs.
	Fresh bool
	// Canaries are the canary paths (relative to B) the step may change.
	Canaries []string
	Run      func(ctx context.Context) Outcome
}

// Result is one step's line.
type Result struct {
	ID    string
	Class Class
	Outcome
}

// Envelope is what the runner needs of the Guard (and what its tests fake).
type Envelope interface {
	Verify(ctx context.Context) error
	VerifyDestructive(ctx context.Context) error
	Snapshot() (Snapshot, error)
	PostCheck(before Snapshot, allowed []string) (Snapshot, error)
}

// Runner runs steps inside the envelope.
type Runner struct {
	Env Envelope
	// Wipe allows the wipe step; off is --skip-wipe.
	Wipe bool
	// Restart restarts the sandbox and re-pins it, for a Fresh step.
	// Nil makes a Fresh step an ERROR.
	Restart func(ctx context.Context) error
}

// Order orders steps: first every step that is not destructive, groups in
// the order they first appear and each group's by class (observe, then
// write), then every destructive step that is not a wipe, by group in that
// same order and in the given order inside a group, then every wipe, in
// the order given. So a destructive step runs only once every observe and
// write step of the run has, and an ERROR in any of them stops every
// destructive one; a wipe runs after every other step. It refuses a group
// made only of destructive steps: a destructive step is never first in its
// group.
func Order(steps []Step) ([]Step, error) {
	var groups []string
	by := map[string][]Step{}
	ids := map[string]bool{}
	for _, s := range steps {
		if s.Run == nil {
			return nil, fmt.Errorf("conformance: step %s has nothing to run", s.ID)
		}
		if ids[s.ID] {
			return nil, fmt.Errorf("conformance: step %s twice", s.ID)
		}
		ids[s.ID] = true
		if s.Wipe && s.Class != Destructive {
			return nil, fmt.Errorf("conformance: wipe step %s is not destructive", s.ID)
		}
		if _, ok := by[s.Group]; !ok {
			groups = append(groups, s.Group)
		}
		by[s.Group] = append(by[s.Group], s)
	}
	var out, destructive, wipes []Step
	for _, g := range groups {
		ss := by[g]
		slices.SortStableFunc(ss, func(a, b Step) int { return int(a.Class) - int(b.Class) })
		if ss[0].Class == Destructive {
			return nil, fmt.Errorf("conformance: group %q starts with a destructive step", g)
		}
		i := slices.IndexFunc(ss, func(s Step) bool { return s.Class == Destructive })
		if i < 0 {
			i = len(ss)
		}
		out = append(out, ss[:i]...)
		for _, x := range ss[i:] {
			if x.Wipe {
				wipes = append(wipes, x)
			} else {
				destructive = append(destructive, x)
			}
		}
	}
	return append(append(out, destructive...), wipes...), nil
}

// Run orders steps (Order) and runs them. Verify runs at start and
// whenever the group changes (so again when a group's destructive steps
// come, after every other group's observe and write steps), and, as
// VerifyDestructive, immediately before each destructive step; the
// post-check walk runs after every step. It returns the results so far
// and an error when the run was refused or ended early: a refusal, a step
// that touched what it was not handed, or a destructive FAIL. An ERROR in
// any step that is not destructive, in whichever group, skips every
// destructive one; an ERROR in any step at all skips the wipe.
func (r *Runner) Run(ctx context.Context, steps []Step) ([]Result, error) {
	order, err := Order(steps)
	if err != nil {
		return nil, err
	}
	if r.Env == nil {
		return nil, refuse("runner", "no envelope")
	}
	if err := r.Env.Verify(ctx); err != nil {
		return nil, err
	}
	before, err := r.Env.Snapshot()
	if err != nil {
		return nil, err
	}
	var out []Result
	skipRest := func(from int, why string) {
		for _, s := range order[from:] {
			out = append(out, Result{s.ID, s.Class, Outcome{Skip, why}})
		}
	}
	noDestructive, noWipe := "", ""
	group := ""
	for i, s := range order {
		if err := ctx.Err(); err != nil {
			skipRest(i, "interrupted")
			return out, err
		}
		if i == 0 || s.Group != group {
			group = s.Group
			if err := r.Env.Verify(ctx); err != nil {
				skipRest(i, "the envelope refused")
				return out, err
			}
		}
		if s.Class == Destructive {
			switch {
			case noDestructive != "":
				out = append(out, Result{s.ID, s.Class, Outcome{Skip, noDestructive}})
				continue
			case s.Wipe && !r.Wipe:
				out = append(out, Result{s.ID, s.Class, Outcome{Skip, "the wipe is turned off"}})
				continue
			case s.Wipe && noWipe != "":
				out = append(out, Result{s.ID, s.Class, Outcome{Skip, noWipe}})
				continue
			}
		}
		if s.Fresh {
			if r.Restart == nil {
				out = append(out, Result{s.ID, s.Class, Outcome{Error, "no way to restart the sandbox"}})
				noDestructive = "an earlier check was an ERROR"
				noWipe = noDestructive
				continue
			}
			if err := r.Restart(ctx); err != nil {
				skipRest(i, "the sandbox could not be restarted")
				return out, err
			}
			if before, err = r.Env.Snapshot(); err != nil {
				skipRest(i, "the base could not be walked")
				return out, err
			}
		}
		if s.Class == Destructive {
			if err := r.Env.VerifyDestructive(ctx); err != nil {
				skipRest(i, "the envelope refused")
				return out, err
			}
		}
		o := s.Run(ctx)
		if !validStatus(o.Status) {
			o = Outcome{Error, fmt.Sprintf("the check returned %q", o.Status)}
		}
		out = append(out, Result{s.ID, s.Class, o})
		after, err := r.Env.PostCheck(before, s.Canaries)
		if err != nil {
			var t *TouchedError
			if errors.As(err, &t) {
				reason := t.Error()
				if o.Reason != "" {
					reason += " (the check said " + string(o.Status) + ": " + o.Reason + ")"
				} else if o.Status != "" {
					reason += " (the check said " + string(o.Status) + ")"
				}
				out[len(out)-1].Outcome = Outcome{Error, reason}
			}
			skipRest(i+1, "the run ended")
			return out, err
		}
		before = after
		if o.Status == Error && s.Class != Destructive && noDestructive == "" {
			noDestructive = "an earlier check was an ERROR"
		}
		if o.Status == Error && noWipe == "" {
			noWipe = "an earlier check was an ERROR"
		}
		if o.Status == Fail && s.Class == Destructive {
			skipRest(i+1, "a destructive check failed")
			return out, fmt.Errorf("conformance: destructive check %s failed: %s", s.ID, o.Reason)
		}
	}
	return out, nil
}

func validStatus(s Status) bool {
	switch s {
	case Pass, Fail, Error, Gap, Skip, Info:
		return true
	}
	return false
}
