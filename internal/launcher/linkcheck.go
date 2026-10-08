package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/chainguard-dev/clog"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// While the container runs, the link also checks each proposal as it
// arrives, so that the session that wrote it learns in seconds, not at the
// user's next caboose apply, whether caboose apply would refuse it and,
// for [packages], whether its names exist and what the list would come
// to: it resolves the list the proposal would make against the repository
// indexes alone (apkobuild.Check: no package downloaded) and writes the
// outcome beside the proposal as NAME.check (proposal.WriteCheck), through
// nofollow. One that cannot be read as a proposal gets one saying why; one
// with nothing to check (no [packages]) gets none. Each version of a file,
// by its contents' hash, is checked once, one check at a time, each
// bounded; one already checked when the link started (a check file newer
// than it) is not checked again. Nothing a proposal holds is run or
// fetched: only names that passed pkgset.CheckName go to the resolver, and
// what is written back is made printable and cut short.
//
// The sandbox decides how many proposals there are and how often they
// change, so everything the checker does is bounded: a poll reads at most
// proposalPollMax files, oldest first, and leaves the rest to the next;
// it writes at most proposalWriteBudget checks an hour, and checks at most
// proposalCheckBudget against the network; and what it logs is
// rate-limited (checkLog).

// proposalCheckTimeout bounds one check: the indexes are fetched once and
// kept in the cache, so a check is seconds.
const proposalCheckTimeout = 2 * time.Minute

// proposalCheckBudget is how many proposals the link checks against the
// package index in an hour: each check fetches the index. Past it, apply
// checks them.
const proposalCheckBudget = 30

// proposalWriteBudget is how many proposals the link checks in an hour,
// whatever the outcome, refusals and removed checks included; past it, it stops until the hour rolls
// over, and the proposals waiting are checked then.
const proposalWriteBudget = 300

// proposalPollMax is the most proposal files one poll reads.
const proposalPollMax = 64

// checkLogMax is the most lines the checker logs in checkLogEvery; the
// rest are counted, and the count logged once the period is over.
const (
	checkLogMax   = 20
	checkLogEvery = time.Minute
)

// proposalChecker checks proposals as they arrive.
type proposalChecker struct {
	dataDir string
	// load reads the configuration as it stands now: the image may have
	// changed since the link started.
	load func() (*config.Config, error)
	// check resolves a spec (apkobuild.Check); opts says how, for c.
	check   func(ctx context.Context, spec apkobuild.Spec, o apkobuild.Options) (apkobuild.CheckResult, error)
	opts    func(c *config.Config) apkobuild.Options
	timeout time.Duration
	logf    func(format string, args ...any)

	started bool
	// initial are the files' times at the first poll, for telling one
	// already checked before the link started.
	initial map[string]time.Time
	// prev are the files' times at the last poll; checkedAt and done the
	// time and contents' hash each file was last checked at.
	prev, checkedAt map[string]time.Time
	done            map[string]string
	// baseline are the checks of profiles' specs as they stand, by
	// architecture and package list, for the deltas.
	baseline map[string]apkobuild.CheckResult
	// spent are the times of the network checks in the last hour
	// (proposalCheckBudget), written those of the checks written
	// (proposalWriteBudget); paused is whether the latter ran out, and
	// was logged.
	spent, written []time.Time
	paused         bool
	// log is the rate limit on logf.
	log checkLog
	now func() time.Time
}

// checkLog limits what a proposalChecker logs to checkLogMax lines in
// checkLogEvery, as the host link does for the outbound proxy: the
// sandbox decides how often there is something to say, and link.log is
// the host's disk.
type checkLog struct {
	from               time.Time
	logged, suppressed int
}

// logLine logs through c.logf, within the rate limit.
func (c *proposalChecker) logLine(format string, args ...any) {
	now := c.clock()
	l := &c.log
	if l.from.IsZero() || now.Sub(l.from) >= checkLogEvery {
		if l.suppressed > 0 {
			c.logf("... and %d more lines about checking proposals not logged (at most %d a minute)", l.suppressed, checkLogMax)
		}
		l.from, l.logged, l.suppressed = now, 0, 0
	}
	if l.logged >= checkLogMax {
		l.suppressed++
		return
	}
	l.logged++
	c.logf(format, args...)
}

func (c *proposalChecker) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (a *App) newProposalChecker(logf func(string, ...any)) *proposalChecker {
	return &proposalChecker{
		dataDir: a.Cfg.DataDir,
		load: func() (*config.Config, error) {
			return config.Load(a.Cfg.Getenv, config.OSFS{}, a.Cfg.Env)
		},
		check:   a.apkoTools().check,
		opts:    a.checkOptions,
		timeout: proposalCheckTimeout,
		logf:    logf,
	}
}

// checkOptions are how a proposal is checked under c: for the architecture
// of the profile's lock, else this machine's, with the build's cache.
func (a *App) checkOptions(c *config.Config) apkobuild.Options {
	arch := ""
	if l, err := apkobuild.ReadLock(c.LockPath()); err == nil {
		arch = l.Arch()
	}
	if arch == "" {
		arch, _ = apkobuild.ArchFor(runtime.GOARCH)
	}
	return apkobuild.Options{Arch: arch, CacheDir: a.apkCacheDir()}
}

// poll looks at the proposals once, and checks each new version of one
// that has held still since the last poll: oldest first, reading at most
// proposalPollMax of them, and none once proposalWriteBudget is spent.
func (c *proposalChecker) poll(ctx context.Context) {
	if c.done == nil {
		c.prev, c.checkedAt, c.done = map[string]time.Time{}, map[string]time.Time{}, map[string]string{}
		c.baseline = map[string]apkobuild.CheckResult{}
	}
	stamps, _ := proposal.Stamps(c.dataDir)
	cur := map[string]time.Time{}
	for _, s := range stamps {
		cur[s.File] = s.Modified
	}
	if !c.started {
		c.started = true
		c.initial = maps.Clone(cur)
	}
	slices.SortFunc(stamps, func(a, b proposal.Stamp) int {
		if n := a.Modified.Compare(b.Modified); n != 0 {
			return n
		}
		return strings.Compare(a.File, b.File)
	})
	read := 0
	for _, s := range stamps {
		f, m := s.File, s.Modified
		if p, ok := c.prev[f]; !ok || !p.Equal(m) || c.checkedAt[f].Equal(m) {
			continue
		}
		if ctx.Err() != nil || read >= proposalPollMax || !c.mayWrite() {
			break
		}
		read++
		pr, h, err := proposal.ReadHashed(c.dataDir, f)
		c.checkedAt[f] = m
		if h == "" || c.done[f] == h {
			continue
		}
		c.done[f] = h
		// Checked before the link started: its check is newer than it.
		if im, ok := c.initial[f]; ok && im.Equal(m) {
			delete(c.initial, f)
			if cm, ok := proposal.CheckModified(c.dataDir, f); ok && !cm.Before(m) {
				continue
			}
		}
		c.checkOne(ctx, f, pr, err)
	}
	for f := range c.done {
		if _, ok := cur[f]; !ok {
			delete(c.done, f)
			delete(c.checkedAt, f)
		}
	}
	for f := range c.initial {
		if _, ok := cur[f]; !ok {
			delete(c.initial, f)
		}
	}
	c.prev = cur
}

// mayWrite is whether proposalWriteBudget has a check left this hour,
// logging once when it runs out.
func (c *proposalChecker) mayWrite() bool {
	now := c.clock()
	c.written = slices.DeleteFunc(c.written, func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	if len(c.written) < proposalWriteBudget {
		c.paused = false
		return true
	}
	if !c.paused {
		c.paused = true
		// Once a pause, so outside the rate limit, which may have spent
		// its lines on the checks that led here.
		c.logf("checked %d proposals in the last hour, the most: checking no more until the hour is over", proposalWriteBudget)
	}
	return false
}

// checkOne checks proposal file f, read as pr or refused with perr, and
// writes what it found beside it.
func (c *proposalChecker) checkOne(ctx context.Context, f string, pr *proposal.Proposal, perr error) {
	status, facts := c.outcome(ctx, pr, perr)
	c.written = append(c.written, c.clock())
	if facts == nil {
		if err := proposal.RemoveCheck(c.dataDir, f); err != nil {
			c.logLine("cannot remove the check of %s: %s", proposal.Printable(f), proposal.Printable(err.Error()))
		}
		return
	}
	if err := proposal.WriteCheck(c.dataDir, f, status, facts); err != nil {
		c.logLine("cannot write the check of %s: %s", proposal.Printable(f), proposal.Printable(err.Error()))
		return
	}
	c.logLine("checked the proposal %s", proposal.Printable(f))
}

// outcome is what checking a proposal finds: nil facts for one there is
// nothing to check in (no [packages]). CheckError is for what is wrong
// with the proposal; CheckUnchecked for whatever kept the host from
// checking it now.
func (c *proposalChecker) outcome(ctx context.Context, pr *proposal.Proposal, perr error) (proposal.CheckStatus, []string) {
	if perr != nil {
		return proposal.CheckError, []string{"caboose apply would refuse this proposal: " + perr.Error()}
	}
	if pr.Packages == nil {
		return proposal.CheckError, nil
	}
	cfg, err := c.load()
	if err != nil {
		return proposal.CheckUnchecked, []string{fmt.Sprintf("the host cannot read its configuration to check this against (%v); caboose apply will check it", err)}
	}
	ip := cfg.ImageProfile
	if why := packagesRefusal(ip); why != "" {
		return proposal.CheckError, []string{why}
	}
	pl, refusals, warnings := planPackages(ip, *pr.Packages)
	if len(refusals) > 0 {
		return proposal.CheckError, refusals
	}
	next := ip.Spec()
	next.Packages = pl.next
	o := c.opts(cfg)

	now := c.clock()
	c.spent = slices.DeleteFunc(c.spent, func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	if len(c.spent) >= proposalCheckBudget {
		return proposal.CheckUnchecked, []string{fmt.Sprintf("the host has checked %d proposals in the last hour, its most; caboose apply will check this one", proposalCheckBudget)}
	}
	c.spent = append(c.spent, now)

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ctx = clog.WithLogger(ctx, clog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := c.check(ctx, next, o)
	var re *apkobuild.ResolutionError
	switch {
	case errors.As(err, &re):
		return proposal.CheckError, []string{err.Error()}
	case errors.Is(err, context.DeadlineExceeded):
		return proposal.CheckUnchecked, []string{fmt.Sprintf("checking took longer than %s, so it is not known whether this resolves; caboose apply will try it", c.timeout)}
	case err != nil:
		return proposal.CheckUnchecked, []string{fmt.Sprintf("the host could not check this now (%v); caboose apply will check it", err)}
	}
	fact := fmt.Sprintf("resolves to %d packages, %s installed", res.Packages, sizeString(res.InstalledBytes))
	if base, err := c.baselineOf(ctx, ip.Spec(), o); err == nil {
		fact += fmt.Sprintf(" (%+d packages, %s)", res.Packages-base.Packages, signedSize(res.InstalledBytes-base.InstalledBytes))
	}
	return proposal.CheckOK, append([]string{fact}, warnings...)
}

// baselineOf is the check of spec, the profile as it stands, remembered
// once it succeeded.
func (c *proposalChecker) baselineOf(ctx context.Context, spec apkobuild.Spec, o apkobuild.Options) (apkobuild.CheckResult, error) {
	list, err := spec.List()
	if err != nil {
		return apkobuild.CheckResult{}, err
	}
	key := o.Arch + " " + strings.Join(list, " ")
	if r, ok := c.baseline[key]; ok {
		return r, nil
	}
	r, err := c.check(ctx, spec, o)
	if err == nil {
		c.baseline[key] = r
	}
	return r, err
}

// checkProposals checks proposals as they arrive, until stop.
func (r *linkRunner) checkProposals(stop <-chan struct{}) {
	c := r.a.newProposalChecker(r.log.Printf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-stop
		cancel()
	}()
	t := time.NewTicker(proposalPollEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		c.poll(ctx)
	}
}

// sizeString is n bytes in decimal units: "2.3 GB", "210 MB".
func sizeString(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for (f >= 1000 || f <= -1000) && i < len(units)-1 {
		f /= 1000
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%d B", n)
	case f < 10 && f > -10:
		return fmt.Sprintf("%.1f %s", f, units[i])
	}
	return fmt.Sprintf("%.0f %s", f, units[i])
}

// signedSize is sizeString with a sign: "+210 MB", "-1.5 MB".
func signedSize(n int64) string {
	if n < 0 {
		return sizeString(n)
	}
	return "+" + sizeString(n)
}
