package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

const checkpointUsage = "usage: caboose checkpoint list [PATH|KEY] [--json] | create [PATH]... | restore ID --to DIR | release ID"

// checkpointOpts are the checkpoint commands' seams, for the tests.
type checkpointOpts struct {
	// apps makes another environment's App; nil is envApp.
	apps func(env string) (*App, error)
	// now is the clock; nil is time.Now.
	now func() time.Time
	// lockWait is how long create waits for the store's lock; 0 is the
	// store's default.
	lockWait time.Duration
	// take carries Take's test switches; restore carries Restore's.
	take    checkpoint.TakeOptions
	restore checkpoint.RestoreOptions
	// restored, when set, is given what checkpoint.Restore returned, as
	// it returned it: the conformance suite tells the product's refusals
	// of a target (ErrTargetExists, ErrTargetRefused) from other failures.
	restored func(checkpoint.Restored, error)
}

// Checkpoint is `caboose checkpoint`: list, create and restore the
// environment's checkpoints, all done here, on this machine, never by the
// daemon (each can outlast a request to it).
func (a *App) Checkpoint(args []string) error { return a.checkpointCmd(args, checkpointOpts{}) }

func (a *App) checkpointCmd(args []string, o checkpointOpts) error {
	if o.now == nil {
		o.now = time.Now
	}
	if len(args) == 0 {
		return Die("%s", checkpointUsage)
	}
	switch args[0] {
	case "list":
		return a.checkpointList(args[1:], o)
	case "create":
		return a.checkpointCreate(args[1:], o)
	case "restore":
		return a.checkpointRestore(args[1:], o)
	case "release":
		return a.checkpointRelease(args[1:], o)
	}
	return Die("%s (got %q)", checkpointUsage, args[0])
}

// normalCheckpointPath is p as [checkpoints] paths holds it: a container
// path, or ~/REL for one under the home.
func normalCheckpointPath(p string) (string, error) {
	n, err := config.CheckCheckpoints([]string{p})
	if err != nil {
		return "", err
	}
	return n[0], nil
}

// checkpointList is `caboose checkpoint list [PATH|KEY] [--json]`: the
// store as it is (read only, no lock: a checkpoint appears complete, by a
// rename), and, from the daemon when it answers, the schedule.
func (a *App) checkpointList(args []string, o checkpointOpts) error {
	asJSON, filter := false, ""
	for _, x := range args {
		switch {
		case x == "--json":
			asJSON = true
		case strings.HasPrefix(x, "-") || filter != "":
			return Die("usage: caboose checkpoint list [PATH|KEY] [--json] (got %q)", x)
		default:
			filter = x
		}
	}
	res, err := a.checkpointListing(nil)
	if err != nil {
		return Die("%v", err)
	}
	if filter != "" {
		want := filter
		if strings.HasPrefix(filter, "/") || strings.HasPrefix(filter, "~") {
			if want, err = normalCheckpointPath(filter); err != nil {
				return Die("%v", err)
			}
		}
		match := func(l daemonproto.CheckpointListed) bool { return l.Path == want || l.Key == want }
		res.Paths = slices.DeleteFunc(res.Paths, func(l daemonproto.CheckpointListed) bool { return !match(l) })
		res.Orphans = slices.DeleteFunc(res.Orphans, func(l daemonproto.CheckpointListed) bool { return !match(l) })
		if len(res.Paths)+len(res.Orphans) == 0 {
			return Die("no path in [checkpoints] paths, and no key of the store, is %s", proposal.Printable(filter))
		}
	}
	if asJSON {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return Die("%v", err)
		}
		fmt.Fprintf(a.Stdout, "%s\n", b)
		return nil
	}
	a.printListing(res, a.checkpointSchedule(), o.now())
	return nil
}

// checkpointSchedule is the daemon's account of this environment's
// schedule, nil when it does not answer (it is never started for this),
// or a note of why there is none.
func (a *App) checkpointSchedule() *daemonproto.CheckpointStatus {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStatusBudget)
	defer cancel()
	c, err := a.dialDaemon(ctx, false)
	if err != nil {
		return nil
	}
	defer c.Close()
	st, err := c.Status(ctx)
	if err != nil {
		return nil
	}
	for _, e := range st.Envs {
		if e.Name == a.Cfg.Env && e.Checkpoints != nil {
			return e.Checkpoints
		}
	}
	return &daemonproto.CheckpointStatus{}
}

// printListing prints a listing for a terminal, with the schedule when
// the daemon gave one (sched), and ages as of now.
func (a *App) printListing(res *daemonproto.CheckpointsResult, sched *daemonproto.CheckpointStatus, now time.Time) {
	w := a.Stdout
	if len(a.Cfg.Checkpoints) == 0 {
		fmt.Fprintln(w, "No [checkpoints] paths in config.toml: none are taken. List the paths to copy there.")
	}
	byPath := map[string]daemonproto.CheckpointPathStatus{}
	switch {
	case len(a.Cfg.Checkpoints) == 0:
	case a.Cfg.Getenv != nil && a.Cfg.Getenv("CABOOSE_DATA_DIR") != "":
		fmt.Fprintln(w, "CABOOSE_DATA_DIR is set: the daemon takes checkpoints only for an environment at its default data dir.")
	case sched == nil:
		fmt.Fprintln(w, "The daemon does not answer: checkpoints are taken only while it runs, which it does while the sandbox runs.")
	case sched.Served == "" && len(sched.Paths) == 0:
		fmt.Fprintln(w, "The daemon runs no schedule for this environment now: checkpoints are taken while the sandbox runs.")
	default:
		for _, p := range sched.Paths {
			byPath[p.Path] = p
		}
		if !sched.InProgressSince.IsZero() {
			fmt.Fprintf(w, "A checkpoint of %s is being taken, since %s.\n", sched.InProgressPath, ckAgo(sched.InProgressSince, now))
		}
		if sched.Exposed != "" {
			fmt.Fprintf(w, "No checkpoint is taken: %s\n", sched.Exposed)
		}
		switch {
		case sched.SpaceSkipped != "":
			fmt.Fprintf(w, "Scheduled checkpoints are skipped: the disk holding them is below its soft floor (%s). 'caboose checkpoint create' still takes one; nothing is pruned for it.\n", strings.TrimPrefix(sched.SpaceSkipped, checkpoint.ErrLowSpace.Error()+": "))
		case sched.LowSpace:
			fmt.Fprintln(w, "The disk holding the checkpoints is low on space.")
		}
		if sched.LastError != "" {
			fmt.Fprintf(w, "Last error (%s): %s\n", ckAgo(sched.LastErrorAt, now), sched.LastError)
		}
	}
	for i, l := range res.Paths {
		if i > 0 || len(a.Cfg.Checkpoints) == 0 {
			fmt.Fprintln(w)
		}
		head := fmt.Sprintf("%s  (key %s)", l.Path, l.Key)
		if l.Resolved {
			head += ": copied from this machine"
		} else {
			head += ": not copied now: " + l.Reason
		}
		fmt.Fprintln(w, head)
		if s, ok := byPath[l.Path]; ok {
			switch {
			case s.Failing != "":
				fmt.Fprintf(w, "  failing: %s\n", s.Failing)
			case !s.NextAt.IsZero():
				fmt.Fprintf(w, "  next %s\n", until(s.NextAt, now))
			}
		}
		printCheckpoints(a, l, now)
	}
	if len(res.Orphans) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Kept for paths no longer in [checkpoints] paths (pruned by the same rule):")
		for _, l := range res.Orphans {
			fmt.Fprintf(w, "%s  (key %s)\n", l.Path, l.Key)
			printCheckpoints(a, l, now)
		}
	}
	if res.Truncated {
		fmt.Fprintln(w, "\n(Only the newest are listed.)")
	}
}

func printCheckpoints(a *App, l daemonproto.CheckpointListed, now time.Time) {
	w := a.Stdout
	if l.Shrunk != "" {
		when := "before it ages out"
		for _, c := range l.Checkpoints {
			if c.ID == l.Shrunk && c.Pinned {
				when = "(it is pinned, so it does not age out)"
			}
		}
		fmt.Fprintf(w, "  The newest holds half or less of what %s held: if files were lost, restore it %s:\n    caboose -e %s checkpoint restore %s --to DIR\n", l.Shrunk, when, a.Cfg.Env, l.Shrunk)
	}
	if l.PinsError != "" {
		fmt.Fprintf(w, "  The pins cannot be read (%s): nothing is pruned until that is fixed.\n", l.PinsError)
	}
	for _, c := range l.Checkpoints {
		if c.Pinned {
			fmt.Fprintf(w, "  %s is pinned: %s held half or less of it. It is kept until a later checkpoint holds as much again, or until:\n    caboose -e %s checkpoint release %s\n", c.ID, c.PinnedBy, a.Cfg.Env, c.ID)
		}
	}
	if len(l.Checkpoints) == 0 {
		fmt.Fprintln(w, "  no checkpoints")
		return
	}
	for i, c := range l.Checkpoints {
		if c.Error != "" {
			fmt.Fprintf(w, "  %s  %s\n", c.ID, c.Error)
			continue
		}
		line := fmt.Sprintf("  %s  %s (%s)  %s  %s  %s", c.ID, c.Started.Local().Format("2006-01-02 15:04"), ckAgo(c.Started, now), c.Reason, c.Method, entries(c.Entries))
		if c.Linked > 0 {
			line += fmt.Sprintf(", %d linked", c.Linked)
		}
		if c.Skipped > 0 {
			line += fmt.Sprintf(", %d skipped", c.Skipped)
		}
		if c.Shrunk != "" {
			line += ", shrank from " + c.Shrunk
		}
		if c.Pinned {
			line += "  (pinned)"
		}
		if i == 0 {
			line += "  (latest)"
		}
		fmt.Fprintln(w, line)
	}
}

// entries is a count of entries in words.
func entries(n int64) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

// ckAgo is how long before now t was, in words.
func ckAgo(t, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		return "in the future"
	}
	return roughly(d) + " ago"
}

// until is how long after now t is, in words.
func until(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "now"
	}
	return "in " + roughly(d)
}

func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

// checkpointCreate is `caboose checkpoint create [PATH]...`: a checkpoint
// now, reason manual, of each named path, or of every configured one,
// taken here under the store's lock, whether the sandbox runs or not. A
// path not in [checkpoints] paths is refused before anything is taken.
func (a *App) checkpointCreate(args []string, o checkpointOpts) error {
	if len(a.Cfg.Checkpoints) == 0 {
		return Die("no [checkpoints] paths in config.toml: add the paths to copy there first")
	}
	want := slices.Clone(a.Cfg.Checkpoints)
	if len(args) > 0 {
		want = nil
		for _, x := range args {
			if strings.HasPrefix(x, "-") {
				return Die("usage: caboose checkpoint create [PATH]... (got %q)", x)
			}
			p, err := normalCheckpointPath(x)
			if err != nil {
				return Die("%v", err)
			}
			if !slices.Contains(a.Cfg.Checkpoints, p) {
				return Die("%s is not in [checkpoints] paths: add it to [checkpoints] paths first", proposal.Printable(p))
			}
			if !slices.Contains(want, p) {
				want = append(want, p)
			}
		}
	}
	s, err := a.openCheckpointStore(o.apps)
	if err != nil {
		return Die("%v", err)
	}
	defer s.Close()
	ctx := context.Background()
	l, err := s.Lock(ctx, o.lockWait)
	if err != nil {
		return Die("%v", err)
	}
	defer l.Unlock()
	// Resolved with the lock held, so what is copied is what the sandbox
	// can write now.
	srcs, bad := a.checkpointSources()
	failed := 0
	for _, p := range want {
		if i := slices.IndexFunc(bad, func(u UnresolvedPath) bool { return u.Path == p }); i >= 0 {
			fmt.Fprintf(a.Stdout, "%s: not taken: %s\n", p, proposal.Printable(bad[i].Reason))
			failed++
			continue
		}
		i := slices.IndexFunc(srcs, func(s checkpoint.Source) bool { return s.Path == p })
		if i < 0 {
			fmt.Fprintf(a.Stdout, "%s: not taken: it did not resolve\n", p)
			failed++
			continue
		}
		to := o.take
		to.Now = o.now
		r, err := checkpoint.TakeLocked(ctx, l, s, srcs[i], checkpoint.ReasonManual, to)
		if err != nil {
			fmt.Fprintf(a.Stdout, "%s: not taken: %s\n", p, proposal.Printable(err.Error()))
			failed++
			continue
		}
		m := r.Meta
		line := fmt.Sprintf("%s: took %s (%s, %s", p, r.ID, m.Method, entries(m.Entries))
		if m.Linked > 0 {
			line += fmt.Sprintf(", %d linked", m.Linked)
		}
		if m.Skipped > 0 {
			line += fmt.Sprintf(", %d skipped", m.Skipped)
		}
		fmt.Fprintln(a.Stdout, line+")")
		if r.LowSpace {
			fmt.Fprintln(a.Stdout, "  the disk holding the checkpoints is below its soft floor: taken, since you asked, but the scheduled ones are skipped until space is freed")
		}
		switch {
		case m.Shrunk != nil && r.Pinned != nil:
			fmt.Fprintf(a.Stdout, "  it holds half or less of what %s held: that one is pinned, kept until a later checkpoint holds as much again or 'caboose -e %s checkpoint release %s'; if files were lost, restore it\n", m.Shrunk.ID, a.Cfg.Env, m.Shrunk.ID)
		case m.Shrunk != nil:
			fmt.Fprintf(a.Stdout, "  it holds half or less of what %s held: if files were lost, restore that one before it ages out\n", m.Shrunk.ID)
		}
		for _, u := range r.Unpinned {
			fmt.Fprintf(a.Stdout, "  %s is no longer pinned: this one holds as much again\n", u)
		}
		if r.PruneErr != nil {
			fmt.Fprintf(a.Stdout, "  pruning the older ones failed: %s\n", proposal.Printable(r.PruneErr.Error()))
		}
	}
	if failed > 0 {
		return Die("%d of %d checkpoints not taken", failed, len(want))
	}
	return nil
}

// checkpointRestore is `caboose checkpoint restore ID --to DIR`: the
// checkpoint copied into DIR, a new directory, never over anything, and
// made safe to open on this machine (checkpoint.Restore), after a
// checkpoint of the live path. Restore checks everything before it writes
// anything; this says what it did, and what to mind.
func (a *App) checkpointRestore(args []string, o checkpointOpts) error {
	const usage = "usage: caboose checkpoint restore ID --to DIR"
	var id, to string
	for i := 0; i < len(args); i++ {
		x := args[i]
		switch {
		case x == "--to":
			if i+1 >= len(args) || to != "" {
				return Die("%s", usage)
			}
			i++
			to = args[i]
		case strings.HasPrefix(x, "--to="):
			if to != "" {
				return Die("%s", usage)
			}
			to = strings.TrimPrefix(x, "--to=")
		case strings.HasPrefix(x, "-") || id != "":
			return Die("%s (got %q)", usage, x)
		default:
			id = x
		}
	}
	if id == "" || to == "" {
		return Die("%s", usage)
	}
	if _, err := checkpoint.ParseID(id); err != nil {
		return Die("%v", err)
	}
	if _, err := os.Lstat(checkpoint.StoreDir(a.Cfg.EnvDir)); errors.Is(err, fs.ErrNotExist) {
		return Die("environment %s has no checkpoints", a.Cfg.Env)
	}
	s, err := a.openCheckpointStore(o.apps)
	if err != nil {
		return Die("%v", err)
	}
	defer s.Close()

	ro := o.restore
	ro.CabooseHome = a.Cfg.CabooseHome
	paths := &restorePaths{a: a, apps: o.apps}
	ro.Paths = paths
	if ro.Now == nil {
		ro.Now = o.now
	}
	var notes []string
	ro.Live = func(p string) (*checkpoint.Source, error) {
		if !slices.Contains(a.Cfg.Checkpoints, p) {
			return nil, errors.New("it is no longer in [checkpoints] paths")
		}
		srcs, bad := a.checkpointSources()
		for _, s := range srcs {
			if s.Path == p {
				return &s, nil
			}
		}
		for _, u := range bad {
			if u.Path == p {
				return nil, errors.New(u.Reason)
			}
		}
		return nil, errors.New("it did not resolve")
	}

	res, err := checkpoint.Restore(context.Background(), s, id, to, ro)
	if o.restored != nil {
		o.restored(res, err)
	}
	w := a.Stdout
	if res.BeforeRestore != nil {
		fmt.Fprintf(w, "First took %s, a checkpoint of %s as it is now: restore it to undo what you do with the copy.\n", res.BeforeRestore, res.Path)
	} else if res.NoBeforeRestore != "" {
		fmt.Fprintf(w, "No checkpoint of the live path was taken first: %s.\n", proposal.Printable(res.NoBeforeRestore))
	}
	if paths.why != "" {
		fmt.Fprintf(w, "The daemon could not say what the sandboxes write (%s).\n", proposal.Printable(paths.why))
	}
	for _, n := range paths.notes {
		fmt.Fprintf(w, "  %s\n", proposal.Printable(n))
	}
	for _, n := range notes {
		fmt.Fprintln(w, n)
	}
	for _, x := range res.Warnings {
		if x == res.NoBeforeRestore {
			continue // said above
		}
		fmt.Fprintf(w, "Warning: %s.\n", proposal.Printable(x))
	}
	if err != nil {
		return Die("%s", proposal.Printable(err.Error()))
	}
	target := proposal.Printable(res.Target)
	fmt.Fprintf(w, "Restored %s (%s) to %s: %s, %s.\n", res.ID, res.Path, target, res.Method, entries(res.Entries))
	if res.Skipped > 0 {
		fmt.Fprintf(w, "Left out %d entries the copy could not take (special files, unreadable directories, another filesystem):\n", res.Skipped)
		for _, e := range res.SkippedExamples {
			fmt.Fprintf(w, "  %s\n", proposal.Printable(e))
		}
	}
	if len(res.Readers) > 0 {
		fmt.Fprintf(w, "The sandboxes of %s can read it (it is in a root they mount read-only).\n", strings.Join(res.Readers, ", "))
	}
	if res.HomePath {
		fmt.Fprintf(w, "It is a copy of %s: Claude Code's login is not in it, but any other secret that path held (a token, a key) is. Keep it private.\n", res.Path)
	}
	if len(res.Sanitized) > 0 {
		fmt.Fprintf(w, "Made safe to open on this machine (%d changes):\n", len(res.Sanitized))
		for _, z := range res.Sanitized {
			line := fmt.Sprintf("  %s: %s", proposal.Printable(z.Path), sanitizedWords(z.Action))
			if z.Reason != "" {
				line += ": " + proposal.Printable(z.Reason)
			}
			fmt.Fprintln(w, line)
		}
	}
	return nil
}

// checkpointRelease is `caboose checkpoint release ID`: the pinned
// checkpoint ID unpinned, under the store's lock. It is then kept or
// dropped by the retention rules, at the next prune; nothing is removed
// here.
func (a *App) checkpointRelease(args []string, o checkpointOpts) error {
	const usage = "usage: caboose checkpoint release ID"
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return Die("%s", usage)
	}
	id, err := checkpoint.ParseID(args[0])
	if err != nil {
		return Die("%v", err)
	}
	if id.Latest {
		return Die("release a checkpoint by its ID (KEY@STAMP), not %s: 'caboose checkpoint list' shows the pinned ones", id)
	}
	if _, err := os.Lstat(checkpoint.StoreDir(a.Cfg.EnvDir)); errors.Is(err, fs.ErrNotExist) {
		return Die("environment %s has no checkpoints", a.Cfg.Env)
	}
	s, err := a.openCheckpointStore(o.apps)
	if err != nil {
		return Die("%v", err)
	}
	defer s.Close()
	l, err := s.Lock(context.Background(), o.lockWait)
	if err != nil {
		return Die("%v", err)
	}
	defer l.Unlock()
	if err := s.ReleasePin(l, id); err != nil {
		return Die("%s", proposal.Printable(err.Error()))
	}
	fmt.Fprintf(a.Stdout, "Released %s: it is kept or pruned by the retention rules from now on.\n", id)
	return nil
}

// sanitizedWords says what a checkpoint.Sanitized action did.
func sanitizedWords(action string) string {
	switch action {
	case checkpoint.ActionMode:
		return "setuid, setgid or sticky bit cleared"
	}
	return proposal.Printable(action)
}
