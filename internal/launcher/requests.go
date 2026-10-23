package launcher

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

// Budgets for the broker commands' calls: they start the daemon if need
// be, then ask it once or twice.
const (
	brokerBudget  = 40 * time.Second
	brokerRequest = 15 * time.Second
)

// brokerClient dials the daemon, starting it when nothing answers, as
// every command of the request broker does.
func (a *App) brokerClient() (DaemonCaller, context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), brokerBudget)
	c, err := a.dialDaemon(ctx, true)
	if err != nil {
		cancel()
		return nil, nil, nil, Die("the caboose daemon could not be reached: %s\n  run: %s", daemonErrText(err), runDaemonHint)
	}
	return c, ctx, cancel, nil
}

// brokerFailure is err, from the daemon, as the message a person reads,
// with what to run when there is something to.
func (a *App) brokerFailure(err error) error {
	var de *daemonproto.Error
	if !errors.As(err, &de) {
		return Die("%s", proposal.Printable(firstLine(err.Error())))
	}
	msg := proposal.Printable(or(de.Message, de.Kind))
	switch de.Kind {
	case daemonproto.KindUnknownMethod:
		return Die("the running caboose daemon is older than this caboose and does not serve requests;\n  run: %s", runDaemonHint)
	case daemonproto.KindNotOwner:
		return Die("%s", msg)
	case daemonproto.KindNotFound:
		return Die("%s\n  '%s' lists them", msg, envCommand(a.Cfg.Env, "requests --all"))
	case daemonproto.KindAlreadyAnswered:
		return Die("%s; the first answer stands", msg)
	case daemonproto.KindStaleDiff:
		return Die("config.toml changed since the change was shown, so nothing was written:\n  run the command again to see it as it is now")
	case daemonproto.KindNotAllowed, daemonproto.KindBrokerUnavailable, daemonproto.KindUnknownEnv, daemonproto.KindBadRequest:
		return Die("%s", msg)
	case daemonproto.KindProtocolMismatch, daemonproto.KindUnavailable, daemonproto.KindTimeout, daemonproto.KindBadFrame:
		return Die("the caboose daemon: %s\n  run: %s", msg, runDaemonHint)
	}
	return Die("%s", msg)
}

// requestNumber reads a request's number, with or without the #.
func requestNumber(s string) (uint64, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || n == 0 {
		return 0, Die("%q is not a request number (caboose requests lists them)", s)
	}
	return n, nil
}

var hoursRe = regexp.MustCompile(`^([0-9]{1,4})([hd])$`)

// parseHours reads --for's value: whole hours (8h) or days (2d), from one
// hour to a week.
func parseHours(s string) (int, error) {
	m := hoursRe.FindStringSubmatch(s)
	if m == nil {
		return 0, Die("--for takes whole hours or days, such as 8h or 2d, not %q", s)
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "d" {
		n *= 24
	}
	if n < 1 || n > daemonproto.MaxHours {
		return 0, Die("--for is from 1h to %dh (7d), not %q", daemonproto.MaxHours, s)
	}
	return n, nil
}

const requestsUsage = "usage: caboose requests [--all | show N | approve N [--for DURATION | --always] | deny N]"

// Requests is `caboose requests`: what the sandbox has asked for and is
// waiting on, and the answers.
func (a *App) Requests(args []string) error {
	if len(args) == 0 {
		return a.requestsList(false)
	}
	switch args[0] {
	case "--all":
		if len(args) > 1 {
			return Die("%s", requestsUsage)
		}
		return a.requestsList(true)
	case "show":
		if len(args) != 2 {
			return Die("%s", requestsUsage)
		}
		id, err := requestNumber(args[1])
		if err != nil {
			return err
		}
		return a.requestShow(id)
	case "deny":
		if len(args) != 2 {
			return Die("%s", requestsUsage)
		}
		id, err := requestNumber(args[1])
		if err != nil {
			return err
		}
		return a.requestAnswer(daemonproto.AnswerParams{Env: a.Cfg.Env, ID: id, Verdict: daemonproto.VerdictDeny})
	case "approve":
		return a.requestApprove(args[1:])
	}
	return Die("%s (got %q)", requestsUsage, args[0])
}

func (a *App) requestsList(all bool) error {
	c, ctx, cancel, err := a.brokerClient()
	if err != nil {
		return err
	}
	defer cancel()
	defer c.Close()
	r, err := c.Requests(ctx, a.Cfg.Env, all, 0)
	if err != nil {
		return a.brokerFailure(err)
	}
	if len(r.Requests) == 0 {
		what := "pending requests"
		if all {
			what = "requests"
		}
		fmt.Fprintf(a.Stdout, "no %s in environment %s\n", what, a.Cfg.Env)
		return nil
	}
	now := a.now()
	pending := 0
	for _, q := range r.Requests {
		a.printRequest(q, now, false)
		if q.State == "pending" {
			pending++
		}
	}
	if r.Truncated {
		fmt.Fprintf(a.Stdout, "\n(more requests than fit one answer: the oldest are left out; decide these first)\n")
	}
	if pending > 0 {
		fmt.Fprintf(a.Stdout, "\nAnswer with:\n  %s\n  %s\n  %s\n",
			envCommand(a.Cfg.Env, "requests approve N [--for 8h | --always]"), envCommand(a.Cfg.Env, "requests deny N"),
			envCommand(a.Cfg.Env, "requests show N"))
	}
	return nil
}

func (a *App) requestShow(id uint64) error {
	c, ctx, cancel, err := a.brokerClient()
	if err != nil {
		return err
	}
	defer cancel()
	defer c.Close()
	r, err := c.Request(ctx, a.Cfg.Env, id)
	if err != nil {
		return a.brokerFailure(err)
	}
	a.printRequest(r.Request, a.now(), true)
	return nil
}

// printRequest writes q: its number, age and kind, what allowing it does
// in the host's words, then what the sandbox says of it, indented under
// that line, all of it printable.
func (a *App) printRequest(q daemonproto.RequestView, now time.Time, full bool) {
	out := a.Stdout
	pr := proposal.Printable
	head := fmt.Sprintf("#%d  %s  %s", q.ID, ago(now, q.Created), pr(q.Kind))
	if q.State != "pending" {
		head += "  " + pr(q.State)
		if q.DecidedBy != "" {
			head += " by " + pr(q.DecidedBy)
		}
		if q.Scope != "" {
			head += " (" + pr(q.Scope)
			if q.Hours > 0 {
				head += fmt.Sprintf(" %dh", q.Hours)
			}
			head += ")"
		}
	}
	if q.Attempts > 1 {
		head += fmt.Sprintf("  asked %d times", q.Attempts)
	}
	fmt.Fprintln(out, head)
	if q.Effect != "" {
		fmt.Fprintf(out, "     %s\n", pr(q.Effect))
	}
	if full {
		if q.Key != "" {
			fmt.Fprintf(out, "     key: %s\n", pr(q.Key))
		}
		fmt.Fprintf(out, "     filed %s", q.Created.Local().Format(time.DateTime))
		if !q.Expires.IsZero() && q.State == "pending" {
			fmt.Fprintf(out, ", expires %s", q.Expires.Local().Format(time.DateTime))
		}
		fmt.Fprintln(out)
		if q.Reason != "" && q.State != "pending" {
			fmt.Fprintf(out, "     %s\n", pr(q.Reason))
		}
		if q.Session != "" {
			fmt.Fprintf(out, "     session (as the sandbox names it): %s\n", pr(q.Session))
		}
	}
	if q.Ask == nil {
		return
	}
	k := q.Ask
	fmt.Fprintln(out, "     the sandbox says:")
	if k.Context != "" {
		fmt.Fprintf(out, "       %s\n", pr(k.Context))
	}
	if len(k.Options) > 0 {
		fmt.Fprintln(out, "       options:")
		for _, o := range k.Options {
			fmt.Fprintf(out, "         - %s\n", pr(o))
		}
	}
	if k.Recommendation != "" {
		fmt.Fprintf(out, "       recommends: %s\n", pr(k.Recommendation))
	}
	if k.Meanwhile != "" {
		fmt.Fprintf(out, "       meanwhile: %s\n", pr(k.Meanwhile))
	}
}

// requestApprove is `requests approve N [--for DURATION | --always]`.
func (a *App) requestApprove(args []string) error {
	if len(args) == 0 {
		return Die("%s", requestsUsage)
	}
	id, err := requestNumber(args[0])
	if err != nil {
		return err
	}
	p := daemonproto.AnswerParams{Env: a.Cfg.Env, ID: id, Verdict: daemonproto.VerdictAllow, Scope: daemonproto.ScopeOnce}
	set := false
	for rest := args[1:]; len(rest) > 0; rest = rest[1:] {
		arg := rest[0]
		switch {
		case arg == "--always" || arg == "--for" || strings.HasPrefix(arg, "--for="):
			if set {
				return Die("--for and --always are alternatives: give one")
			}
			set = true
			if arg == "--always" {
				p.Scope = daemonproto.ScopeAlways
				continue
			}
			v, ok := strings.CutPrefix(arg, "--for=")
			if !ok {
				if len(rest) < 2 {
					return Die("--for needs a duration, such as 8h or 2d")
				}
				v, rest = rest[1], rest[1:]
			}
			h, err := parseHours(v)
			if err != nil {
				return err
			}
			p.Scope, p.Hours = daemonproto.ScopeHours, h
		default:
			return Die("%s (got %q)", requestsUsage, arg)
		}
	}
	if p.Scope == daemonproto.ScopeAlways {
		return a.requestAlways(p)
	}
	return a.requestAnswer(p)
}

// requestAnswer answers once or for some hours, or denies, and says what
// came of it.
func (a *App) requestAnswer(p daemonproto.AnswerParams) error {
	c, ctx, cancel, err := a.brokerClient()
	if err != nil {
		return err
	}
	defer cancel()
	defer c.Close()
	r, err := c.Answer(ctx, p)
	if err != nil {
		return a.brokerFailure(err)
	}
	a.printAnswer(r)
	return nil
}

// requestAlways answers "always": the daemon offers the change to
// config.toml it would make, which is shown whole; only a typed yes on a
// terminal has it written, by answering with the diff's hash, which the
// daemon refuses if config.toml would now change otherwise.
func (a *App) requestAlways(p daemonproto.AnswerParams) error {
	term, err := a.openTerminal()
	if err != nil {
		return Die("--always writes to config.toml, and asks you to confirm the change first; there is no terminal to ask on (%v).\n"+
			"       Run it in one, or approve once or with --for. Nothing was changed.", err)
	}
	defer term.Close()
	c, ctx, cancel, err := a.brokerClient()
	if err != nil {
		return err
	}
	defer cancel()
	defer c.Close()
	rr, err := c.Request(ctx, p.Env, p.ID)
	if err != nil {
		return a.brokerFailure(err)
	}
	o := rr.Offered(daemonproto.ScopeAlways)
	if o == nil {
		why := "it is " + proposal.Printable(rr.Request.State) + ", not pending"
		if rr.Request.State == "pending" {
			// A pending request offers or withholds always: a daemon that
			// does neither predates offers.
			why = "the running daemon is older than this caboose; run: caboose daemon restart"
		}
		for _, w := range rr.Withheld {
			if w.Scope == daemonproto.ScopeAlways {
				why = proposal.Printable(w.Why)
			}
		}
		return Die("request #%d cannot be allowed always: %s. Nothing was changed.", p.ID, why)
	}
	fmt.Fprintf(a.Stdout, "Request #%d would be allowed always.\n", p.ID)
	fmt.Fprintf(a.Stdout, "  %s\n", proposal.Printable(o.Effect))
	fmt.Fprintf(a.Stdout, "\nThis is the change to config.toml, exactly as it would be written:\n\n")
	shown := false
	for _, l := range o.Diff {
		if l != "" {
			fmt.Fprintf(a.Stdout, "  %s\n", proposal.Printable(l))
			shown = true
		}
	}
	if !shown {
		fmt.Fprintln(a.Stdout, "  (no change: config.toml says this already)")
	}
	fmt.Fprintln(a.Stdout)
	pr := a.newSetupPrompter(term)
	ans, err := pr.ask("Write this to config.toml? Type yes to write it", "")
	if err != nil {
		return Die("not confirmed: nothing was written (request #%d is still pending)", p.ID)
	}
	if ans != "yes" {
		return Die("not confirmed: nothing was written (request #%d is still pending)", p.ID)
	}
	p.ConfirmDiff = o.Hash
	// The person took as long as they took: the confirming call gets a
	// budget of its own, not what is left of the first call's.
	ctx2, cancel2 := context.WithTimeout(context.Background(), brokerBudget)
	defer cancel2()
	r, err := c.Answer(ctx2, p)
	if err != nil {
		return a.brokerFailure(err)
	}
	a.printAnswer(r)
	return nil
}

// printAnswer says what an answer did: the verdict, what it means in the
// host's words, the grant, the requests decided with it and a URL that did
// not open.
func (a *App) printAnswer(r *daemonproto.AnswerResult) {
	out := a.Stdout
	pr := proposal.Printable
	q := r.Request
	switch q.State {
	case "allowed":
		scope := q.Scope
		if q.Hours > 0 {
			scope += fmt.Sprintf(" %dh", q.Hours)
		}
		fmt.Fprintf(out, "Request #%d allowed (%s).\n", q.ID, pr(scope))
	case "denied":
		fmt.Fprintf(out, "Request #%d denied.\n", q.ID)
	default:
		fmt.Fprintf(out, "Request #%d is %s.\n", q.ID, pr(q.State))
	}
	if r.Effect != "" {
		fmt.Fprintf(out, "  %s\n", pr(r.Effect))
	}
	if r.Grant != "" {
		fmt.Fprintf(out, "  grant %s (caboose grants lists it, grants revoke %s ends it)\n", r.Grant, r.Grant)
	}
	if len(r.Also) > 0 {
		ids := make([]string, len(r.Also))
		for i, id := range r.Also {
			ids[i] = "#" + strconv.FormatUint(id, 10)
		}
		fmt.Fprintf(out, "  also decided with it: %s\n", strings.Join(ids, ", "))
	}
	if r.OpenError != "" {
		fmt.Fprintf(out, "  the URL did not open: %s\n", pr(r.OpenError))
	}
}

// Grants is `caboose grants [revoke ID]`.
func (a *App) Grants(args []string) error {
	revoke := ""
	switch {
	case len(args) == 0:
	case len(args) == 2 && args[0] == "revoke":
		revoke = args[1]
	default:
		return Die("usage: caboose grants [revoke ID]")
	}
	c, ctx, cancel, err := a.brokerClient()
	if err != nil {
		return err
	}
	defer cancel()
	defer c.Close()
	if revoke != "" {
		r, err := c.Revoke(ctx, a.Cfg.Env, revoke)
		if err != nil {
			return a.brokerFailure(err)
		}
		fmt.Fprintf(a.Stdout, "Grant %s ended.\n", r.Revoked)
		return nil
	}
	r, err := c.Grants(ctx, a.Cfg.Env)
	if err != nil {
		return a.brokerFailure(err)
	}
	if len(r.Grants) == 0 {
		fmt.Fprintf(a.Stdout, "no grants in force in environment %s\n", a.Cfg.Env)
		return nil
	}
	a.printGrants(r.Grants)
	if r.Truncated {
		fmt.Fprintln(a.Stdout, "(more grants than fit one answer)")
	}
	fmt.Fprintf(a.Stdout, "\nEnd one with: %s\n", envCommand(a.Cfg.Env, "grants revoke ID"))
	return nil
}

func (a *App) printGrants(gs []daemonproto.GrantView) {
	for _, g := range gs {
		what := "until " + g.Expires.Local().Format(time.DateTime)
		if g.Once {
			what = "once, within " + leftText(g.LeftSeconds)
		} else {
			what += " (" + leftText(g.LeftSeconds) + " left)"
		}
		line := fmt.Sprintf("%-6s %-9s %s  %s", g.ID, proposal.Printable(g.Kind), proposal.Printable(g.Key), what)
		if g.Request != 0 {
			line += fmt.Sprintf("  (request #%d)", g.Request)
		}
		fmt.Fprintln(a.Stdout, line)
		if len(g.Addrs) > 0 {
			fmt.Fprintf(a.Stdout, "       only the addresses %s\n", proposal.Printable(strings.Join(g.Addrs, ", ")))
		}
	}
}

// leftText is a time left, roughly.
func leftText(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Permissions is `caboose permissions`: what the environment's sandbox may
// do, as config.toml and the grants have it.
func (a *App) Permissions() error {
	c, ctx, cancel, err := a.brokerClient()
	if err != nil {
		return err
	}
	defer cancel()
	defer c.Close()
	r, err := c.Permissions(ctx, a.Cfg.Env)
	if err != nil {
		return a.brokerFailure(err)
	}
	a.printPermissions(r)
	return nil
}

func (a *App) printPermissions(r *daemonproto.PermissionsResult) {
	out := a.Stdout
	pr := proposal.Printable
	list := func(l []string) string {
		if len(l) == 0 {
			return "none"
		}
		return pr(strings.Join(l, ", "))
	}
	fmt.Fprintf(out, "environment : %s\n", r.Env)
	fmt.Fprintf(out, "trust       : %s\n", pr(r.Trust))
	iso := r.Isolation
	if r.Profile != "" {
		iso += " (profile " + r.Profile + ")"
	}
	fmt.Fprintf(out, "isolation   : %s\n", pr(iso))
	for i, d := range r.Drift {
		label := "drift       :"
		if i > 0 {
			label = "             "
		}
		fmt.Fprintf(out, "%s %s\n", label, pr(d))
	}
	if len(r.Drift) > 0 {
		fmt.Fprintf(out, "              the configuration takes effect at 'caboose -e %s restart' (this kills running sessions)\n", r.Env)
	}
	for _, ro := range r.Roots {
		access := "read-only"
		switch {
		case len(ro.Writable) == 1 && ro.Writable[0] == ".":
			access = "writable"
		case len(ro.Writable) > 0:
			access = "read-only, except " + list(ro.Writable)
		}
		fmt.Fprintf(out, "root        : %s -> %s: %s\n", pr(ro.Host), pr(ro.Container), access)
	}
	for _, al := range r.Aliases {
		fmt.Fprintf(out, "alias       : %s: %s, in %s, policy %s\n", pr(al.Name), pr(strings.Join(al.Command, " ")), or(pr(al.Cwd), "an empty directory"), pr(al.Policy))
		if al.Description != "" {
			fmt.Fprintf(out, "              %s\n", pr(al.Description))
		}
	}
	he := "off"
	if r.HostExec.Effective {
		he = "on: sessions can run commands on this machine as you"
	}
	if r.HostExec.Why != "" {
		he += " (" + pr(r.HostExec.Why) + ")"
	}
	fmt.Fprintf(out, "host exec   : %s\n", he)
	fmt.Fprintf(out, "open urls   : %s; allowed without asking: %s\n", pr(r.OpenURLs.Mode), list(r.OpenURLs.Allow))
	if r.OpenURLs.Why != "" {
		fmt.Fprintf(out, "              %s\n", pr(r.OpenURLs.Why))
	}
	fmt.Fprintf(out, "network     : %s\n", pr(r.Egress.Network))
	if r.Egress.On {
		fmt.Fprintf(out, "egress      : ports %s; allow: %s; deny: %s\n", pr(r.Egress.Ports), list(r.Egress.Allow), list(r.Egress.Deny))
		fmt.Fprintf(out, "              otherwise: %s\n", pr(r.Egress.Default))
	}
	if len(r.Grants) == 0 {
		fmt.Fprintln(out, "grants      : none in force")
	} else {
		fmt.Fprintln(out, "grants      :")
		a.printGrants(r.Grants)
	}
	if r.Mods != nil {
		fmt.Fprintf(out, "sessions    : %s\n", modsLine(r.Mods.Sessions, r.Mods.WithMod))
	}
}

// modsLine says how many Claude Code sessions run in the sandbox and how
// many of them have caboose's mod, which is what tells them of a decision
// (the host enforces it either way): the sandbox's own count.
func modsLine(sessions, withMod int) string {
	if sessions == 0 {
		return "no Claude Code sessions running"
	}
	line := fmt.Sprintf("%d Claude Code sessions", sessions)
	if sessions == 1 {
		line = "1 Claude Code session"
	}
	if without := sessions - withMod; without > 0 {
		return fmt.Sprintf("%s, %d without caboose's mod (a settings env replacing CLAUDE_CODE_PLUGIN_DIRS drops it)", line, without)
	}
	return line + ", all with caboose's mod"
}
