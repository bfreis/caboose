// Package policy decides what the sandbox may have this machine do: run an
// alias, connect somewhere through the outbound proxy, open a URL, run a
// command through host exec. It only compares: callers gather the facts
// (resolved paths, resolved addresses, this machine's own addresses, the
// time) and pass them in, and Decide does no I/O and keeps no state.
//
// Deny wins. Every kind of request is decided by a list of rules, deny
// rules first, and the first rule that applies decides; no grant, allow
// entry or "allow" setting is looked at before every deny rule has passed.
// Whatever Decide does not recognise -- a kind, a trust level, a policy
// value, a request that does not hang together -- is refused.
package policy

import (
	"fmt"
	"io/fs"
	"net/netip"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
)

// Trust is how far the sandbox is trusted: config.toml's trust.
type Trust string

// The trust levels. Only TrustFallible is fallible: any other value,
// the empty one included, is decided as TrustHostile.
const (
	TrustFallible Trust = config.TrustFallible
	TrustHostile  Trust = config.TrustHostile
)

// Verdict is what Decide says of a request. The zero value is
// VerdictDeny.
type Verdict int

const (
	// VerdictDeny refuses the request.
	VerdictDeny Verdict = iota
	// VerdictAllow lets it go ahead.
	VerdictAllow
	// VerdictAsk refuses it for now, as a request a person may approve.
	VerdictAsk
)

func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictAsk:
		return "ask"
	case VerdictDeny:
		return "deny"
	}
	return fmt.Sprintf("Verdict(%d)", int(v))
}

// Kind is what a request asks for.
type Kind string

// The kinds of request.
const (
	KindAlias    Kind = "alias"
	KindEgress   Kind = "egress"
	KindOpenURL  Kind = "open_url"
	KindHostExec Kind = "host_exec"
	// KindCheckpoint is a checkpoint taken now of paths [checkpoints]
	// already copies on its schedule: it widens no reach, so it is decided
	// by checkpoint_requests alone, under either trust, and never asks.
	KindCheckpoint Kind = "checkpoint"
)

// Request is one thing the sandbox asks for, with the facts the caller
// resolved for it on this machine.
type Request struct {
	Kind Kind

	// Alias is the alias's name, and Args the KEY=VALUE pairs the sandbox
	// gave, checked against the alias's params.
	Alias string
	Args  map[string]string
	// Command is the alias's command[0] with every symlink resolved, a
	// physical host path. Cwd is its cwd resolved the same way, "" when
	// the alias runs in a fresh empty directory. ArgPaths holds, in
	// order, every argument after substitution that starts with "/",
	// resolved: one entry per such argument.
	//
	// Decide compares these paths with Policy.Writable as strings (case
	// aside), so both sides must be spelled the one way the disk names
	// them: a path that reaches a writable directory by another spelling
	// (a firmlink such as /System/Volumes/Data on a Mac, another Unicode
	// normalization of a name) is not recognised as writable.
	Command  string
	Cwd      string
	ArgPaths []string
	// CommandVia holds every path the resolution of Command went through
	// on its way: each symlink followed, as the caller met it. A command
	// reached through a writable path is the sandbox's to repoint, so
	// the caller lists them all, and Decide refuses when any is writable.
	CommandVia []string
	// CommandIDs, CwdIDs and ArgIDs are the identities (FileID) of
	// Command, Cwd and each of ArgPaths, and of every directory above
	// each, as the caller found them on the disk; CommandIDs also those
	// of every CommandVia path and the directories above it. Decide
	// refuses wherever one is the identity of a Policy.WritableIDs entry,
	// as it refuses a path within a Writable one: a spelling the string
	// comparison cannot recognise (a firmlink, another Unicode
	// normalization, a second mount of the same directory) still names
	// the same file. CommandIDs may be empty only when the policy has no
	// WritableIDs; CwdIDs only without a Cwd, ArgIDs only without
	// ArgPaths.
	CommandIDs, CwdIDs, ArgIDs []FileID

	// Host is the name to connect to, lower case with no final dot, or an
	// address literal when Literal is set. Addrs are what the name
	// resolved to on this machine (the literal alone when Literal); Own
	// are this machine's own addresses.
	Host    string
	Literal bool
	Port    int
	Addrs   []netip.Addr
	Own     []netip.Addr

	// URL is the URL to open, already checked, and Origin its
	// scheme://host[:port].
	URL    string
	Origin string
}

// Policy is everything Decide compares a request with: config.toml's
// settings and the grants in force.
type Policy struct {
	Trust Trust
	// HostExec is [link] host_exec.
	HostExec bool
	// OpenURLs is [link] open_urls: "ask", "allow" or "off".
	OpenURLs string
	// OpenURLAllow is [link] open_urls_allow: origins opened without asking.
	OpenURLAllow []string
	// CheckpointRequests is [link] checkpoint_requests: "allow" or "off".
	// Anything else, the empty value included, refuses.
	CheckpointRequests string
	// Aliases are the [alias.NAME] tables, by name.
	Aliases map[string]Alias
	// Egress is the outbound proxy's settings; nil when there is none.
	Egress *Egress
	// Writable are the physical host paths of every part of the roots the
	// sandbox can write. An empty entry is ignored; "/" covers
	// everything, and so does a relative entry, which a caller should
	// never pass.
	Writable []string
	// WritableIDs are the identities of the Writable paths that exist,
	// for the identity check (Request.CommandIDs).
	WritableIDs []FileID
	// Grants are the timed and once grants in force; what is always
	// allowed is in the fields above.
	Grants []Grant
}

// FileID is a file's identity on this machine: its device and inode.
type FileID struct{ Dev, Ino uint64 }

// IDOf is fi's identity, when fi carries one (a Unix stat).
func IDOf(fi fs.FileInfo) (FileID, bool) {
	if fi == nil {
		return FileID{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return FileID{}, false
	}
	// Dev and Ino are of other types on other systems.
	return FileID{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, true
}

// Alias is an [alias.NAME] table.
type Alias struct {
	Name, Description string
	// Command is the argv; Command[0] is absolute. An element that is
	// exactly {KEY} for a key of Params is replaced by that parameter's
	// value.
	Command []string
	// Cwd is the host directory it runs in, "" for a fresh empty one.
	Cwd     string
	Timeout time.Duration
	// Params are the enum parameters: name to the values it may take,
	// the first being what a missing one takes.
	Params map[string][]string
	// Policy is "allow" (also when ""), "ask" or "deny".
	Policy string
}

// Egress is the outbound proxy's settings: the ports it may dial, and
// egress_allow and egress_deny.
type Egress struct {
	Ports       config.PortSet
	Allow, Deny Allow
}

// Grant is a timed or once permission for requests of one GrantKey.
type Grant struct {
	ID   string
	Kind Kind
	// Key is the GrantKey of the requests it allows.
	Key string
	// Once grants allow one request; consuming them is the caller's.
	Once bool
	// Expires is when it stops matching; a zero time never matches.
	Expires time.Time
	// Addrs, for an egress grant, are the private addresses it lets
	// through, each exactly: the ones its host resolved to when the grant
	// was made. A grant names a host, and a name can later resolve
	// anywhere, so it lifts the ask for a private address only for these
	// (unlike a name egress_allow holds, which the user wrote).
	Addrs []netip.Addr
}

// Decision is Decide's answer.
type Decision struct {
	Verdict Verdict
	// Reason is for the agent and the log, written for a person.
	Reason string
	// Rule names the rule that decided, as "deny:egress_deny",
	// "grant:g12" or "default:fallible-public".
	Rule string
	// Grant is the ID of the grant that allowed the request, "" otherwise.
	Grant string
}

// MaxArgs is the most KEY=VALUE arguments an alias request may carry.
const MaxArgs = 32

// The [alias.NAME] policies.
const (
	AliasAllow = config.AliasAllow
	AliasAsk   = config.AliasAsk
	AliasDeny  = config.AliasDeny
)

// The checkpoint_requests settings.
const (
	CheckpointAllow = config.CheckpointRequestsAllow
	CheckpointOff   = config.CheckpointRequestsOff
)

// The open_urls settings.
const (
	OpenAllow = "allow"
	OpenAsk   = "ask"
	OpenOff   = "off"
)

// GrantKey is what a grant for r names: "alias:NAME", "egress:HOST" or
// "open_url:ORIGIN"; "" for a request no grant can allow.
func GrantKey(r Request) string {
	var v string
	switch r.Kind {
	case KindAlias:
		v = r.Alias
	case KindEgress:
		v = r.Host
	case KindOpenURL:
		v = r.Origin
	default:
		return ""
	}
	if v == "" {
		return ""
	}
	return string(r.Kind) + ":" + v
}

// Decide decides r under p at now.
func Decide(p Policy, r Request, now time.Time) Decision {
	switch r.Kind {
	case KindHostExec:
		return decideHostExec(p)
	case KindAlias:
		return decideAlias(p, r, now)
	case KindOpenURL:
		return decideOpenURL(p, r, now)
	case KindEgress:
		return decideEgress(p, r, now)
	case KindCheckpoint:
		return decideCheckpoint(p)
	}
	return deny("deny:unknown-kind", "the host does not know this kind of request")
}

func deny(rule, format string, args ...any) Decision {
	return Decision{Verdict: VerdictDeny, Rule: rule, Reason: fmt.Sprintf(format, args...)}
}

func ask(rule, format string, args ...any) Decision {
	return Decision{Verdict: VerdictAsk, Rule: rule, Reason: fmt.Sprintf(format, args...)}
}

func allow(rule, format string, args ...any) Decision {
	return Decision{Verdict: VerdictAllow, Rule: rule, Reason: fmt.Sprintf(format, args...)}
}

// fallible reports whether p trusts the sandbox as fallible: only that
// exact value does.
func (p Policy) fallible() bool { return p.Trust == TrustFallible }

// grant is the live grant in p for r, if any.
func (p Policy) grant(r Request, now time.Time) (Grant, bool) {
	key := GrantKey(r)
	if key == "" {
		return Grant{}, false
	}
	for _, g := range p.Grants {
		if g.Kind == r.Kind && g.Key != "" && g.Key == key && !g.Expires.IsZero() && now.Before(g.Expires) {
			return g, true
		}
	}
	return Grant{}, false
}

// egressGrant is the live grant in p for r that holds every one of
// private among its addresses, if any.
func (p Policy) egressGrant(r Request, now time.Time, private []netip.Addr) (Grant, bool) {
	key := GrantKey(r)
	if key == "" {
		return Grant{}, false
	}
	for _, g := range p.Grants {
		if g.Kind != r.Kind || g.Key != key || g.Expires.IsZero() || !now.Before(g.Expires) {
			continue
		}
		if holdsAll(g.Addrs, private) {
			return g, true
		}
	}
	return Grant{}, false
}

// holdsAll reports whether every one of want is in have, compared
// exactly (zones dropped, IPv4-mapped forms unmapped).
func holdsAll(have, want []netip.Addr) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h.IsValid() && h.Unmap().WithZone("") == w.Unmap().WithZone("") {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func granted(g Grant, what string) Decision {
	d := allow("grant:"+g.ID, "%s is allowed by a grant", what)
	d.Grant = g.ID
	return d
}

const hostileFix = `trust = "fallible" at the top of config.toml, on the host, lifts this`

func decideHostExec(p Policy) Decision {
	if !p.fallible() {
		return deny("deny:hostile-host-exec", "trust = hostile refuses host_exec: running commands on the host is off while config.toml says so; "+hostileFix)
	}
	if !p.HostExec {
		return deny("deny:host_exec-off", "%s", agentproto.HostExecOff)
	}
	return allow("allow:host_exec", "host_exec = true in [link] of config.toml")
}

// decideCheckpoint decides a checkpoint the sandbox asks for now. The
// trust does not enter into it: it copies only what [checkpoints] paths
// already has copied on its schedule, into a store no sandbox reaches. No
// grant applies (GrantKey is "" for it), and nothing asks: no person
// waits on a checkpoint.
func decideCheckpoint(p Policy) Decision {
	switch p.CheckpointRequests {
	case CheckpointOff:
		return deny("deny:checkpoint_requests-off", `checkpoints asked for by the sandbox are off: checkpoint_requests = "off" in [link] of config.toml on the host`)
	case CheckpointAllow:
		return allow("allow:checkpoint_requests", `checkpoint_requests = "allow" in [link] of config.toml`)
	}
	return deny("deny:checkpoint_requests-policy", `checkpoint_requests %q in [link] is not "allow" or "off"`, clip(printable(p.CheckpointRequests), 32))
}

func decideAlias(p Policy, r Request, now time.Time) Decision {
	name := clip(printable(r.Alias), 64)
	a, ok := p.Aliases[r.Alias]
	if r.Alias == "" || !ok {
		return deny("deny:no-alias", "no alias %q on the host; `caboose-agent aliases` lists them", name)
	}
	pol := a.Policy
	if pol == "" {
		pol = AliasAllow
	}
	switch pol {
	case AliasAllow, AliasAsk:
	case AliasDeny:
		return deny("deny:alias-deny", `alias %q is refused: policy = "deny" in [alias.%s] of config.toml on the host`, name, name)
	default:
		return deny("deny:alias-policy", `alias %q is refused: its policy %q is not "allow", "ask" or "deny"`, name, clip(printable(a.Policy), 32))
	}
	argv, err := Substitute(a, r.Args)
	if err != nil {
		return deny("deny:alias-args", "alias %q: %v", name, err)
	}
	if !resolved(p, r, a, argv) {
		return deny("deny:alias-unresolved", "alias %q: the alias's command could not be resolved on the host", name)
	}
	for _, c := range append([]string{r.Command, path.Clean(a.Command[0])}, r.CommandVia...) {
		if w, ok := p.writable(c); ok {
			return deny("deny:alias-command-writable", "alias %q: its command %s is within %s, which the sandbox can write; an alias's command must resolve outside every writable path, through no writable path", name, clip(printable(c), 200), clip(printable(w), 200))
		}
	}
	if p.writableID(r.CommandIDs) {
		return deny("deny:alias-command-writable", "alias %q: its command %s is, by another name, within a path the sandbox can write; an alias's command must resolve outside every writable path, through no writable path", name, clip(printable(r.Command), 200))
	}
	if !p.fallible() {
		if w, ok := p.writable(path.Clean(a.Cwd)); a.Cwd != "" && ok {
			return deny("deny:hostile-alias-writable", "alias %q: trust = hostile refuses an alias whose cwd %s is within %s, which the sandbox can write; %s", name, clip(printable(a.Cwd), 200), clip(printable(w), 200), hostileFix)
		}
		if w, ok := p.writable(r.Cwd); r.Cwd != "" && ok {
			return deny("deny:hostile-alias-writable", "alias %q: trust = hostile refuses an alias whose cwd %s is within %s, which the sandbox can write; %s", name, clip(printable(r.Cwd), 200), clip(printable(w), 200), hostileFix)
		}
		for _, ap := range r.ArgPaths {
			if w, ok := p.writable(ap); ok {
				return deny("deny:hostile-alias-writable", "alias %q: trust = hostile refuses an alias whose argument %s is within %s, which the sandbox can write; %s", name, clip(printable(ap), 200), clip(printable(w), 200), hostileFix)
			}
		}
		if p.writableID(r.CwdIDs) || p.writableID(r.ArgIDs) {
			return deny("deny:hostile-alias-writable", "alias %q: trust = hostile refuses an alias whose cwd or an argument is, by another name, within a path the sandbox can write; %s", name, hostileFix)
		}
	}
	if pol == AliasAllow {
		return allow("allow:alias", `alias %q: policy = "allow"`, name)
	}
	if g, ok := p.grant(r, now); ok {
		return granted(g, fmt.Sprintf("alias %q", name))
	}
	return ask("ask:alias", `alias %q asks before it runs (policy = "ask" in [alias.%s])`, name, name)
}

// resolved reports whether the facts r carries for alias a, whose argv
// is argv, are what a caller that resolved them would pass: absolute,
// clean paths, a cwd where the alias has one, and one ArgPaths entry per
// absolute argument.
func resolved(p Policy, r Request, a Alias, argv []string) bool {
	if !absClean(r.Command) {
		return false
	}
	if len(p.WritableIDs) > 0 && (len(r.CommandIDs) == 0 || r.Cwd != "" && len(r.CwdIDs) == 0 || len(r.ArgPaths) > 0 && len(r.ArgIDs) == 0) {
		return false
	}
	if r.Cwd != "" && !absClean(r.Cwd) || a.Cwd != "" && r.Cwd == "" {
		return false
	}
	n := 0
	for _, e := range argv[1:] {
		if strings.HasPrefix(e, "/") {
			n++
		}
	}
	if n != len(r.ArgPaths) {
		return false
	}
	for _, ap := range append(r.ArgPaths, r.CommandVia...) {
		if !absClean(ap) {
			return false
		}
	}
	return true
}

func absClean(p string) bool { return path.IsAbs(p) && path.Clean(p) == p }

// writableID reports whether any of ids is a WritableIDs entry's.
func (p Policy) writableID(ids []FileID) bool {
	for _, id := range ids {
		if slices.Contains(p.WritableIDs, id) {
			return true
		}
	}
	return false
}

// writable is the first Writable entry that holds p.
func (p Policy) writable(s string) (string, bool) {
	for _, w := range p.Writable {
		if w != "" && within(s, w) {
			return w, true
		}
	}
	return "", false
}

// within reports whether p is w or under it, component by component and
// ignoring case: a Mac's disks usually ignore it, and a resolved path
// keeps the case it was asked by, so comparing case would let a path
// through that names the same file. A relative w holds everything.
func within(p, w string) bool {
	if !path.IsAbs(w) {
		return true
	}
	p, w = path.Clean(p), path.Clean(w)
	if w == "/" {
		return true
	}
	ps, ws := strings.Split(p, "/"), strings.Split(w, "/")
	if len(ps) < len(ws) {
		return false
	}
	for i := range ws {
		if !strings.EqualFold(ps[i], ws[i]) {
			return false
		}
	}
	return true
}

func decideOpenURL(p Policy, r Request, now time.Time) Decision {
	if r.URL == "" || config.CheckOrigin(r.Origin) != nil || !sameOrigin(r.URL, r.Origin) {
		return deny("deny:open-url-bad", "the request names no URL the host can open")
	}
	origin := clip(printable(r.Origin), 200)
	if p.OpenURLs == OpenOff {
		return deny("deny:open_urls-off", `opening URLs on the host is off: open_urls = "off" in [link] of config.toml on the host`)
	}
	if !p.fallible() {
		return ask("ask:hostile", "trust = hostile asks before opening any URL on the host")
	}
	switch p.OpenURLs {
	case OpenAllow:
		return allow("allow:open_urls", `open_urls = "allow" in [link] of config.toml`)
	case OpenAsk:
	default:
		return deny("deny:open_urls-value", `open_urls in [link] of config.toml is %q, not "ask", "allow" or "off"; fix it on the host`, clip(printable(p.OpenURLs), 32))
	}
	for _, o := range p.OpenURLAllow {
		if o == r.Origin {
			return allow("allow:open_urls_allow", "open_urls_allow in [link] of config.toml names %s", origin)
		}
	}
	if g, ok := p.grant(r, now); ok {
		return granted(g, "opening "+origin)
	}
	return ask("ask:open_urls", `open_urls = "ask": opening %s needs a person's approval`, origin)
}

// sameOrigin reports whether url starts with origin (ignoring case, as a
// host may come in either) and nothing but a path, query or fragment
// follows it.
func sameOrigin(url, origin string) bool {
	if len(url) < len(origin) || !strings.EqualFold(url[:len(origin)], origin) {
		return false
	}
	rest := url[len(origin):]
	return rest == "" || strings.ContainsRune("/?#", rune(rest[0]))
}

func decideEgress(p Policy, r Request, now time.Time) Decision {
	e := p.Egress
	if e == nil {
		return deny("deny:egress-off", "the host offers no outbound proxy (egress in the vm profile of config.toml on the host)")
	}
	host := clip(printable(r.Host), 100)
	if !validEgressHost(r) {
		return deny("deny:egress-bad", "%q is not a host name, or not the address it was resolved to", host)
	}
	if len(r.Addrs) == 0 {
		return deny("deny:egress-bad", "%s has no address on the host", host)
	}
	target := fmt.Sprintf("%s:%d", host, r.Port)
	if !e.Ports.Has(r.Port) {
		return deny("deny:egress_ports", "%s: port %d is not in egress_ports (config.toml on the host)", target, r.Port)
	}
	for _, a := range r.Addrs {
		if !a.IsValid() || a.Zone() != "" {
			return deny("deny:egress-bad", "%s: an address with a zone cannot be reached through the host", target)
		}
	}
	if !r.Literal && e.Deny.Name(r.Host) {
		return deny("deny:egress_deny", "%s: egress_deny (config.toml on the host) names it", target)
	}
	for _, a := range r.Addrs {
		if e.Deny.Addr(a) || anyAddr(e.Deny, embeddedV4(a.Unmap())) {
			return deny("deny:egress_deny", "%s: egress_deny (config.toml on the host) names %s, which it resolves to", target, a)
		}
	}
	own := make([]netip.Addr, 0, len(r.Own))
	for _, a := range r.Own {
		own = append(own, a.Unmap().WithZone(""))
	}
	for _, a := range r.Addrs {
		if rg, b, ok := localRange(a, own); ok && !e.Allow.Within(b, rg) {
			return deny("deny:local-address", "%s: %s is a loopback, unspecified or link-local address, or one of the host's own, which only an egress_allow address or CIDR within its range lets through", target, a)
		}
	}
	if !r.Literal && e.Allow.Name(r.Host) {
		return allow("allow:egress_allow", "%s: egress_allow names it", target)
	}
	if allAddrs(e.Allow, r.Addrs, own) {
		return allow("allow:egress_allow", "%s: egress_allow names every address it resolves to", target)
	}
	// A name config.toml's egress_allow holds reaches whatever it resolves
	// to: the user wrote it (a VPN host, say). A grant is an answer given
	// at run time about one resolution, so it lets a private address
	// through only when it holds that very address: a granted name that
	// later resolves somewhere private asks again (DNS rebinding).
	var private []netip.Addr
	for _, a := range r.Addrs {
		if Denied(a) && !allowedAddr(e.Allow, a, own) {
			private = append(private, a)
		}
	}
	if len(private) > 0 {
		if g, ok := p.egressGrant(r, now, private); ok {
			return granted(g, "connecting to "+host)
		}
		return ask("ask:private", "%s: %s is a private or local address, which egress_allow (config.toml on the host) does not name", target, private[0])
	}
	if g, ok := p.grant(r, now); ok {
		return granted(g, "connecting to "+host)
	}
	if !p.fallible() {
		return ask("ask:hostile", "%s: trust = hostile asks before connecting to any host egress_allow does not name", target)
	}
	return allow("default:fallible-public", "%s: every address it resolves to is public", target)
}

// validEgressHost reports whether r's host is a name, or, when Literal, an
// address with no zone, written as netip writes it unmapped (so that one
// address has one grant key), that is its sole address.
func validEgressHost(r Request) bool {
	if !r.Literal {
		return validName(r.Host)
	}
	a, err := netip.ParseAddr(r.Host)
	if err != nil || a.Zone() != "" || r.Host != a.Unmap().String() || len(r.Addrs) != 1 {
		return false
	}
	return r.Addrs[0].Zone() == "" && r.Addrs[0].Unmap() == a.Unmap()
}

func anyAddr(a Allow, addrs []netip.Addr) bool {
	for _, b := range addrs {
		if a.Addr(b) {
			return true
		}
	}
	return false
}

// allowedAddr reports whether egress_allow names b: for an address
// localRange holds, by an address or CIDR within that range.
func allowedAddr(a Allow, b netip.Addr, own []netip.Addr) bool {
	if rg, l, ok := localRange(b, own); ok {
		return a.Within(l, rg)
	}
	return a.Addr(b)
}

// allAddrs reports whether egress_allow names every one of addrs.
func allAddrs(a Allow, addrs, own []netip.Addr) bool {
	for _, b := range addrs {
		if !allowedAddr(a, b, own) {
			return false
		}
	}
	return len(addrs) > 0
}
