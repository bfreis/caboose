package policy

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/proposal"
)

// How much of what the sandbox sent an effect shows.
const (
	maxShownArg   = 200
	maxShownArgs  = 16
	maxShownAddrs = 8
	maxShownURL   = 500
	maxShownHost  = 100
)

// printable is s as a person may be shown it: control and invisible
// characters escaped.
func printable(s string) string { return proposal.Printable(s) }

// clip cuts s to at most n bytes, on a character boundary.
func clip(s string, n int) string { return proposal.Clip(s, n) }

// shown is s, which the sandbox sent, made printable and clipped to n.
func shown(s string, n int) string { return clip(printable(s), n) }

// word is an argument as an effect lists it: quoted when it is empty or
// holds a space or a quote, so the words can be told apart.
func word(s string) string {
	if s == "" || strings.ContainsAny(s, " \"'\\") {
		return strconv.Quote(s)
	}
	return s
}

// Effect says what allowing r would do, from the host's facts alone:
// never the agent's description of it. What came from the sandbox is
// made printable and clipped.
func Effect(p Policy, r Request) string {
	switch r.Kind {
	case KindAlias:
		return aliasEffect(p, r)
	case KindEgress:
		return egressEffect(r)
	case KindOpenURL:
		return fmt.Sprintf("open %s in the browser", shown(r.URL, maxShownURL))
	case KindHostExec:
		return "run a command on the host, through host_exec"
	case KindCheckpoint:
		return "take a checkpoint now of paths [checkpoints] already copies"
	}
	return "an unknown request, which the host refuses"
}

func aliasEffect(p Policy, r Request) string {
	name := shown(r.Alias, 64)
	a, ok := p.Aliases[r.Alias]
	if !ok {
		return fmt.Sprintf("run alias %s on the host, which has no such alias", name)
	}
	argv, err := Substitute(a, r.Args)
	if err != nil {
		return fmt.Sprintf("run alias %s on the host, which refuses its arguments", name)
	}
	cmd := r.Command
	if cmd == "" {
		cmd = argv[0]
	}
	words := []string{word(shown(cmd, maxShownArg))}
	for i, e := range argv[1:] {
		if i == maxShownArgs {
			words = append(words, fmt.Sprintf("… (%d more)", len(argv)-1-i))
			break
		}
		words = append(words, word(shown(e, maxShownArg)))
	}
	dir := "a fresh empty directory"
	if r.Cwd != "" {
		dir = shown(r.Cwd, maxShownArg)
	} else if a.Cwd != "" {
		dir = shown(a.Cwd, maxShownArg)
	}
	return fmt.Sprintf("run alias %s on the host: %s in %s", name, strings.Join(words, " "), dir)
}

func egressEffect(r Request) string {
	target := fmt.Sprintf("%s:%d", shown(r.Host, maxShownHost), r.Port)
	if a, err := netip.ParseAddr(r.Host); r.Literal && err == nil && a.Is6() {
		target = fmt.Sprintf("[%s]:%d", shown(r.Host, maxShownHost), r.Port)
	}
	var notes []string
	if !r.Literal && len(r.Addrs) > 0 {
		var addrs []string
		for i, a := range r.Addrs {
			if i == maxShownAddrs {
				addrs = append(addrs, fmt.Sprintf("%d more", len(r.Addrs)-i))
				break
			}
			addrs = append(addrs, shown(a.String(), maxShownHost))
		}
		notes = append(notes, "resolves to "+strings.Join(addrs, ", "))
	}
	own := make([]netip.Addr, 0, len(r.Own))
	for _, a := range r.Own {
		own = append(own, a.Unmap().WithZone(""))
	}
	local, private := false, false
	for _, a := range r.Addrs {
		if _, _, ok := localRange(a, own); ok {
			local = true
		} else if Denied(a) {
			private = true
		}
	}
	if local {
		notes = append(notes, "this machine")
	}
	if private {
		notes = append(notes, "private")
	}
	if len(notes) == 0 {
		return "connect to " + target
	}
	return fmt.Sprintf("connect to %s (%s)", target, strings.Join(notes, "; "))
}

// Scope is how far an answer to a request reaches.
type Scope int

const (
	// ScopeOnce allows the request asked about, and nothing after it:
	// the same alias with the same arguments, the same host and port
	// (ExactKey).
	ScopeOnce Scope = iota + 1
	// ScopeHours allows requests of its GrantKey for some hours: the alias
	// with any arguments, the host on any port.
	ScopeHours
	// ScopeAlways writes the permission into config.toml.
	ScopeAlways
)

// AnswerEffect says what approving r with scope s would do: hours is how
// long for ScopeHours, env the environment, profile the vm profile in use
// and configPath where config.toml is, for ScopeAlways. It is "" for an
// answer that cannot be given. Each says how far it reaches: once is the
// very request shown, the others the whole alias or host.
func AnswerEffect(r Request, s Scope, hours int, env, profile, configPath string) string {
	if GrantKey(r) == "" {
		return ""
	}
	switch s {
	case ScopeOnce:
		switch r.Kind {
		case KindOpenURL:
			return "opens that URL now, and allows nothing more"
		case KindAlias:
			return fmt.Sprintf("allows this one run of alias %s, %s: another run, or other arguments, asks again", shown(r.Alias, 64), argsNote(r))
		case KindEgress:
			return fmt.Sprintf("allows one connection to %s%s on port %d: another connection, or another port, asks again", shown(r.Host, maxShownHost), privateNote(r), r.Port)
		}
		return "allows that once"
	case ScopeHours:
		if hours < 1 {
			return ""
		}
		unit := "hours"
		if hours == 1 {
			unit = "hour"
		}
		var what string
		switch r.Kind {
		case KindAlias:
			what = "alias " + shown(r.Alias, 64) + " with any arguments"
		case KindEgress:
			what = "connections to " + shown(r.Host, maxShownHost) + privateNote(r) + " on any port egress_ports allows"
		case KindOpenURL:
			what = "opening URLs of " + shown(r.Origin, maxShownHost)
		}
		return fmt.Sprintf("allows %s in environment %s for %d %s", what, env, hours, unit)
	case ScopeAlways:
		switch r.Kind {
		case KindAlias:
			return fmt.Sprintf(`sets policy = "allow" in [alias.%s] of %s: every run of it, with any arguments, without asking`, shown(r.Alias, 64), configPath)
		case KindEgress:
			return fmt.Sprintf("adds %s to egress_allow in [vm.%s] of %s: connections to it on any port egress_ports allows, without asking", strconv.Quote(shown(r.Host, maxShownHost)), profile, configPath)
		case KindOpenURL:
			return fmt.Sprintf("adds %s to open_urls_allow in [link] of %s", strconv.Quote(shown(r.Origin, maxShownHost)), configPath)
		}
	}
	return ""
}

// NonPublicAddrs are the addresses of an egress request that only an
// egress_allow address or CIDR, or a grant holding them, lets through:
// those Denied holds, unmapped, without zones, each once. A grant made
// for r carries them (Grant.Addrs), since a grant by name alone never
// reaches a private address.
func NonPublicAddrs(r Request) []netip.Addr {
	if r.Kind != KindEgress {
		return nil
	}
	var out []netip.Addr
	for _, a := range r.Addrs {
		if !a.IsValid() || !Denied(a) {
			continue
		}
		a = a.Unmap().WithZone("")
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// argsNote is the arguments of an alias request as an effect names them.
func argsNote(r Request) string {
	if len(r.Args) == 0 {
		return "with no arguments"
	}
	keys := slices.Sorted(maps.Keys(r.Args))
	var words []string
	for i, k := range keys {
		if i == maxShownArgs {
			words = append(words, fmt.Sprintf("… (%d more)", len(keys)-i))
			break
		}
		words = append(words, word(shown(k+"="+r.Args[k], maxShownArg)))
	}
	return "with exactly " + strings.Join(words, " ")
}

// privateNote names the private addresses an answer for r lets through.
func privateNote(r Request) string {
	ps := NonPublicAddrs(r)
	if len(ps) == 0 {
		return ""
	}
	var shownAddrs []string
	for i, a := range ps {
		if i == maxShownAddrs {
			shownAddrs = append(shownAddrs, fmt.Sprintf("%d more", len(ps)-i))
			break
		}
		shownAddrs = append(shownAddrs, a.String())
	}
	return " at " + strings.Join(shownAddrs, ", ")
}

// EgressRuleEffect says what adding (add) or removing an entry of
// egress_deny (deny) or egress_allow does to environment env under trust
// t, in the host's words. entry is one entry as ParseAllowList takes it.
// Under trust = fallible a host that resolves only to public addresses
// connects whatever egress_allow says, so removing an allow entry changes
// nothing for it, and the effect says so.
func EgressRuleEffect(t Trust, deny, add bool, entry, env string) string {
	target, byName := ruleTarget(entry)
	fallible := t == TrustFallible
	switch {
	case deny && add:
		return fmt.Sprintf("denies connections to %s from environment %s, whatever egress_allow or a grant says", target, env)
	case deny:
		return fmt.Sprintf("stops denying connections to %s from environment %s, which the other rules decide again", target, env)
	case add:
		s := fmt.Sprintf("allows connections to %s from environment %s", target, env)
		if !fallible {
			s += " without asking"
		}
		if byName {
			s += " at any address it resolves to, private ones included"
		}
		if fallible {
			s += " (public hosts connect anyway under trust = fallible)"
		}
		return s
	}
	if fallible {
		return fmt.Sprintf("changes nothing for public hosts, which connect anyway under trust = fallible: connections to %s from environment %s that reach a private or local address ask again", target, env)
	}
	return fmt.Sprintf("connections to %s from environment %s ask again (trust = %s asks for every host egress_allow does not name)", target, env, TrustHostile)
}

// ruleTarget is what an egress_allow or egress_deny entry names, and
// whether it names hosts by name.
func ruleTarget(entry string) (string, bool) {
	e := shown(entry, maxShownHost)
	if strings.Contains(entry, "/") {
		return "addresses in " + e, false
	}
	if _, err := netip.ParseAddr(entry); err == nil {
		return "address " + e, false
	}
	if rest, ok := strings.CutPrefix(e, "*."); ok {
		return "every name under " + rest, true
	}
	return e, true
}
