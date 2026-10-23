package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/conformance"
)

// The mutants of the host link and the launcher, which Link acts on.
const (
	// UnknownAliasRuns runs the marker for a name no alias has.
	UnknownAliasRuns Mutant = "unknown alias runs"
	// AliasArgsUnchecked runs an alias whatever arguments it is given.
	AliasArgsUnchecked Mutant = "alias arguments unchecked"
	// SandboxDefinesAlias takes aliases from the sandbox's own files.
	SandboxDefinesAlias Mutant = "sandbox defines aliases"
	// ProposalApplied applies a proposal the sandbox wrote, unasked.
	ProposalApplied Mutant = "proposal applied"
	// HostileWritableRuns runs an alias whose cwd is writable under hostile.
	HostileWritableRuns Mutant = "hostile runs writable aliases"
	// HostileOffersHostExec offers host exec under hostile when host_exec is set.
	HostileOffersHostExec Mutant = "hostile offers host exec"
	// AliasEnvLeaks hands an alias the daemon's whole environment.
	AliasEnvLeaks Mutant = "alias environment leaks"
	// RemainsSurvive leaves an alias's children running after its caller ends.
	RemainsSurvive Mutant = "alias remains survive"
	// HostileOnContainer creates a hostile sandbox under a container.
	HostileOnContainer Mutant = "hostile created without vm"
	// AttachUnchecked attaches under hostile to a sandbox made otherwise.
	AttachUnchecked Mutant = "attach unchecked"
	// AttachNoAdvice refuses the attach without saying caboose restart.
	AttachNoAdvice Mutant = "attach refused without advice"
	// TrustFromSandbox takes trust from the sandbox's own files.
	TrustFromSandbox Mutant = "trust from sandbox"
	// TrustLabelWrong labels a sandbox with another trust than it has.
	TrustLabelWrong Mutant = "trust label wrong"
	// HostileEgressOffLoads loads hostile with egress = false.
	HostileEgressOffLoads Mutant = "hostile egress off loads"
	// RefusedAliasesRun runs the marker on every alias run it refuses,
	// and still reports the refusal: only the host's record tells.
	RefusedAliasesRun Mutant = "refused aliases run anyway"
	// RefusedHostExecRuns runs a host command it reports as refused.
	RefusedHostExecRuns Mutant = "refused host commands run anyway"
	// RefusalsExitOdd refuses alias runs (other than an ask) with exit 1.
	RefusalsExitOdd Mutant = "alias refusals exit 1"
	// HostRefusalExitsOdd refuses a host command with exit 1.
	HostRefusalExitsOdd Mutant = "host refusal exits 1"
	// SandboxWritesConfig lets the sandbox's own files change config.toml.
	SandboxWritesConfig Mutant = "sandbox writes config.toml"
	// AskRunsLater runs an alias that asks, unanswered, a while later.
	AskRunsLater Mutant = "ask runs later"
	// AttachRelabels changes the sandbox's labels on a refused attach.
	AttachRelabels Mutant = "attach relabels"
	// LaunchStartsClaude starts a Claude Code process on a launch decision.
	LaunchStartsClaude Mutant = "launch starts claude"
	// HostileForwardsAgent forwards the host's default SSH agent under
	// hostile, as fallible does.
	HostileForwardsAgent Mutant = "hostile forwards the default ssh agent"
)

// HostMutants are the mutants Link acts on, beside AliasRunsWithoutGrant
// and HostExecOn of the model's own list.
var HostMutants = []Mutant{
	HostExecOn, AliasRunsWithoutGrant,
	UnknownAliasRuns, AliasArgsUnchecked, SandboxDefinesAlias, ProposalApplied,
	HostileWritableRuns, HostileOffersHostExec, AliasEnvLeaks, RemainsSurvive,
	HostileOnContainer, AttachUnchecked, AttachNoAdvice, TrustFromSandbox,
	TrustLabelWrong, HostileEgressOffLoads, LaunchStartsClaude,
	RefusedAliasesRun, RefusedHostExecRuns, RefusalsExitOdd, HostRefusalExitsOdd,
	SandboxWritesConfig, AskRunsLater, AttachRelabels, HostileForwardsAgent,
}

// Fault is an answer of the product's, or of the setup, that is not what
// a check was written for: no control is broken, but the check cannot tell
// it held, so it must be an ERROR.
type Fault string

const (
	// UsageUnprefixed: exit 2 without caboose-agent's refusal.
	UsageUnprefixed Fault = "usage unprefixed"
	// HostRefusalUnnamed: host exec refused without naming host_exec.
	HostRefusalUnnamed Fault = "host refusal unnamed"
	// AskDenied: an alias that asks is denied instead of pending.
	AskDenied Fault = "ask denied"
	// TrustUnnamed: the hostile refusal names no trust.
	TrustUnnamed Fault = "trust unnamed"
	// AttachAlwaysRefused: the attach is refused under fallible too.
	AttachAlwaysRefused Fault = "attach always refused"
	// ConfigNeverLoads: no configuration loads.
	ConfigNeverLoads Fault = "config never loads"
	// HostExecNeverOffered: no configuration offers host exec.
	HostExecNeverOffered Fault = "host exec never offered"
	// NoAliasesListed: the listing is empty.
	NoAliasesListed Fault = "no aliases listed"
	// EnvWithoutPath: the alias's environment has no PATH.
	EnvWithoutPath Fault = "env without PATH"
	// CreatedUnreported: Create makes the sandbox and says it did not.
	CreatedUnreported Fault = "created unreported"
	// AgentNeverCarried: no configuration carries an SSH agent.
	AgentNeverCarried Fault = "agent never carried"
)

// Faults are every Fault.
var Faults = []Fault{UsageUnprefixed, HostRefusalUnnamed, AskDenied, TrustUnnamed,
	AttachAlwaysRefused, ConfigNeverLoads, HostExecNeverOffered, NoAliasesListed, EnvWithoutPath, CreatedUnreported,
	AgentNeverCarried}

// SetFault turns fault f on.
func (l *Link) SetFault(f Fault) { l.mu.Lock(); l.faults[f] = true; l.mu.Unlock() }

// Link is a Fake with the host's side of host commands and aliases, the
// suite's stubs and the launcher's trust decisions: what the host and
// trust checks talk to. Nothing in it runs anything; the alias scripts'
// output is written into the alias-out canary, as the real scripts would.
type Link struct {
	*Fake

	trust string // config.toml's
	made  string // the trust the sandbox was created under
	// absent is a sandbox not created (yet): T1's world; stopped one
	// created but never started.
	absent, stopped bool
	// faults are the setup's or the product's answers gone odd.
	faults map[Fault]bool

	// Faults of the setup.
	linkDown, stubsMissing, scriptsSilent bool

	stubCalls int
	// leaked counts notifications or dialogs that reached no stub: what
	// must never happen.
	leaked  int
	claude  int
	pending int
	// sleeping is a conf-sleep whose caller was ended; its child writes
	// the done line when the time passes, if it survived.
	sleeping bool
	// later is an ask that a mutant runs at the next Wait.
	later bool
	// relabel is a label a mutant changed.
	relabel bool
	// home is the sandbox's own files the checks write, by path.
	home map[string]string
	// applied are aliases a proposal added to config.toml (a mutant).
	applied []string
}

// NewLink is a Link over a new Fake, under trust, with the host checks'
// canaries made.
func NewLink(n8, isolation, trust string) (*Link, error) {
	f := New(n8)
	f.SetIsolation(isolation)
	l := &Link{Fake: f, trust: trust, made: trust, home: map[string]string{}, faults: map[Fault]bool{}}
	if trust == conformance.TrustHostile && isolation != "vm" {
		l.absent = true
	}
	if _, err := f.MakeCanary("canary", "alias-out", conformance.Dir); err != nil {
		return nil, err
	}
	if _, err := f.MakeCanary(f.RO(), "h1-out", conformance.Dir); err != nil {
		return nil, err
	}
	return l, nil
}

// Env is a conformance.Env over the Link.
func (l *Link) Env(isolation, trust string) conformance.Env {
	return conformance.NewEnv(l, l, l, l, isolation, trust)
}

// Faults.
func (l *Link) SetLinkDown(b bool)      { l.mu.Lock(); l.linkDown = b; l.mu.Unlock() }
func (l *Link) SetStubsMissing(b bool)  { l.mu.Lock(); l.stubsMissing = b; l.mu.Unlock() }
func (l *Link) SetScriptsSilent(b bool) { l.mu.Lock(); l.scriptsSilent = b; l.mu.Unlock() }

// Leaked is how many notifications or dialogs reached no stub.
func (l *Link) Leaked() int { l.mu.Lock(); defer l.mu.Unlock(); return l.leaked }

// Claude is how many Claude Code processes the model started.
func (l *Link) Claude() int { l.mu.Lock(); defer l.mu.Unlock(); return l.claude }

// Trust is config.toml's trust now.
func (l *Link) Trust() string { l.mu.Lock(); defer l.mu.Unlock(); return l.trust }

func (l *Link) State() string {
	l.mu.Lock()
	absent, stopped := l.absent, l.stopped
	l.mu.Unlock()
	switch {
	case absent:
		return "absent"
	case stopped:
		return "created"
	}
	return l.Fake.State()
}

func (l *Link) Labels() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.absent {
		return nil
	}
	ro, dev := "/work/conf-"+l.n8+"-ro", "/work/conf-"+l.n8+"-dev"
	trust := l.made
	if l.mutants[TrustLabelWrong] {
		trust = conformance.TrustFallible
		if l.made == conformance.TrustFallible {
			trust = conformance.TrustHostile
		}
	}
	network := ""
	if l.isolate == "vm" {
		network = "nat"
		if l.made == conformance.TrustHostile {
			network = "none"
		}
	}
	labels := map[string]string{
		assets.LabelIsolation: l.isolate,
		assets.LabelProfile:   l.isolate + ".conformance",
		assets.LabelTrust:     trust,
		assets.LabelNetwork:   network,
		assets.LabelRoots:     `["` + ro + `","` + dev + `"]`,
		assets.LabelWritable:  `{"` + ro + `":[],"` + dev + `":["part"]}`,
	}
	if l.relabel {
		labels[assets.LabelTrust] = l.trust
	}
	l.labelUser(labels)
	return labels
}

// Exec runs the host checks' commands in the model and hands the rest to
// the Fake.
func (l *Link) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if rc, o, e, ok := l.agentShut(user); ok {
		return rc, o, e, nil
	}
	body, ok := conformance.Unwrap(argv)
	if !ok || !l.mine(body) {
		return l.Fake.Exec(ctx, user, argv, stdin)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.down || l.absent || l.stopped {
		return -1, "", "", errors.New("the sandbox is not running")
	}
	if l.shellNo {
		return 127, "", "exec: the shell is not there: no such file or directory", nil
	}
	l.event("exec")
	var out []string
	rc := l.runChain(tokens(body), &out)
	return 0, fmt.Sprintf("\nrc=%d\n", rc), strings.Join(out, "\n"), nil
}

func (l *Link) mine(body string) bool {
	return strings.HasPrefix(body, "caboose-agent ") || strings.HasPrefix(body, "timeout ") ||
		strings.Contains(body, " >> /home/agent/")
}

func (l *Link) runChain(toks []string, out *[]string) int {
	var cur []string
	rc := 0
	for i := 0; i <= len(toks); i++ {
		if i < len(toks) && toks[i] != "&&" {
			cur = append(cur, toks[i])
			continue
		}
		rc = l.runOne(cur, out)
		if rc != 0 {
			return rc
		}
		cur = nil
	}
	return rc
}

func (l *Link) runOne(a []string, out *[]string) int {
	say := func(rc int, format string, args ...any) int {
		*out = append(*out, fmt.Sprintf(format, args...))
		return rc
	}
	switch {
	case len(a) == 4 && a[0] == "echo" && a[2] == ">>" && strings.HasPrefix(a[3], "/home/agent/"):
		l.home[a[3]] += a[1] + "\n"
		return 0
	case len(a) >= 4 && a[0] == "timeout" && a[2] == "caboose-agent" && a[3] == "run":
		if l.linkDown {
			return say(126, "caboose-agent: run: no host is linked")
		}
		if rc := l.run(a[4:], out, true); rc != 0 {
			return rc
		}
		return exitTimeout
	case len(a) >= 2 && a[0] == "caboose-agent":
		if l.linkDown {
			return say(126, "caboose-agent: %s: no host is linked", a[1])
		}
		switch a[1] {
		case "run":
			return l.run(a[2:], out, false)
		case "host":
			return l.host(a[2:], out)
		case "aliases":
			if len(a) == 4 && a[2] == "--json" && a[3] == "1>&2" {
				var list []agentproto.AliasInfo
				for _, n := range l.aliasNames() {
					if l.faults[NoAliasesListed] {
						break
					}
					list = append(list, agentproto.AliasInfo{Name: n})
				}
				b, _ := json.Marshal(list)
				return say(0, "%s", b)
			}
		case "notify":
			if len(a) == 3 {
				l.notify()
				return 0
			}
		}
	}
	return say(127, "sh: 1: %s: not found", a[0])
}

const exitTimeout = 124

func (l *Link) notify() {
	if l.stubsMissing {
		l.leaked++
		return
	}
	l.stubCalls++
}

// The suite's aliases, as its configuration defines them.
var suiteAliases = map[string]struct {
	policy   string
	writable bool // cwd in the writable part
	sleeps   bool
	params   map[string][]string
}{
	conformance.AliasMarker:   {policy: "allow", params: map[string][]string{conformance.AliasParam: {conformance.AliasParamValue}}},
	conformance.AliasAsk:      {policy: "ask"},
	conformance.AliasWritable: {policy: "allow", writable: true},
	conformance.AliasSleep:    {policy: "allow", sleeps: true},
}

// sandboxAliases are the [alias.NAME] tables the sandbox wrote in its own
// files: only a mutant reads them.
func (l *Link) sandboxAliases(proposals bool) []string {
	var out []string
	for p, text := range l.home {
		isProp := strings.Contains(p, "/.caboose-proposals/")
		if isProp != proposals {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			if n, ok := strings.CutPrefix(line, "[alias."); ok {
				out = append(out, strings.TrimSuffix(n, "]"))
			}
		}
	}
	slices.Sort(out)
	return out
}

func (l *Link) aliasNames() []string {
	names := slices.Sorted(maps.Keys(suiteAliases))
	names = append(names, l.applied...)
	if l.mutants[SandboxDefinesAlias] {
		names = append(names, l.sandboxAliases(false)...)
	}
	return names
}

// trustNow is the trust the host decides by.
func (l *Link) trustNow() string {
	if l.mutants[TrustFromSandbox] {
		for _, text := range l.home {
			if strings.Contains(text, `trust = "fallible"`) {
				return conformance.TrustFallible
			}
		}
	}
	return l.trust
}

func (l *Link) applyProposals() {
	if !l.mutants[ProposalApplied] {
		return
	}
	for _, n := range l.sandboxAliases(true) {
		if !slices.Contains(l.applied, n) {
			l.applied = append(l.applied, n)
		}
	}
}

func (l *Link) run(args []string, out *[]string, ended bool) int {
	say := func(rc int, format string, a ...any) int {
		prefix := "caboose-agent: run: "
		if rc == 2 && l.faults[UsageUnprefixed] {
			prefix = ""
		}
		msg := fmt.Sprintf(format, a...)
		if l.faults[TrustUnnamed] {
			msg = strings.ReplaceAll(msg, "trust = hostile", "this host")
		}
		*out = append(*out, prefix+msg)
		if rc != 0 && rc != 75 && l.mutants[RefusalsExitOdd] {
			rc = 1
		}
		if rc != 0 && l.mutants[RefusedAliasesRun] {
			l.writeOut(conformance.OutRan, conformance.RanLine, true)
		}
		return rc
	}
	if len(args) == 0 {
		return say(2, "no alias named")
	}
	name, kv, err := agentproto.ParseAliasArgv(args)
	if err != nil {
		return say(2, "%v", err)
	}
	a, ok := suiteAliases[name]
	switch {
	case ok:
	case slices.Contains(l.applied, name), l.mutants[SandboxDefinesAlias] && slices.Contains(l.sandboxAliases(false), name):
		a = suiteAliases[conformance.AliasMarker]
		a.params = nil
	case l.mutants[UnknownAliasRuns]:
		a = suiteAliases[conformance.AliasMarker]
	default:
		return say(127, "no alias %q on the host; `caboose-agent aliases` lists them", name)
	}
	if !l.mutants[AliasArgsUnchecked] {
		for k, v := range kv {
			if !slices.Contains(a.params[k], v) {
				return say(77, "alias %q: %s=%s is not a value it takes", name, k, v)
			}
		}
	}
	if a.writable && l.trustNow() == conformance.TrustHostile && !l.mutants[HostileWritableRuns] {
		return say(77, "alias %q: trust = hostile refuses an alias whose cwd is within a path the sandbox can write", name)
	}
	if a.policy == "ask" && l.faults[AskDenied] {
		return say(77, "alias %q is refused", name)
	}
	if a.policy == "ask" && !l.mutants[AliasRunsWithoutGrant] {
		l.pending++
		l.notify()
		if l.mutants[AskRunsLater] {
			l.later = true
		}
		return say(75, "pending #%d: alias %q asks before it runs", l.pending, name)
	}
	if a.sleeps {
		l.writeOut(conformance.OutStart, conformance.StartLine, true)
		if !ended {
			return say(2, "the model runs conf-sleep only under a timeout")
		}
		l.sleeping = true
		return 0
	}
	l.writeOut(conformance.OutRan, conformance.RanLine, true)
	env := "PATH=/usr/bin:/bin\nHOME=/nonexistent\nLANG=C.UTF-8\nPWD=/tmp/alias\nSHLVL=1\n_=/usr/bin/env\n"
	if l.faults[EnvWithoutPath] {
		env = strings.Replace(env, "PATH=", "NOPATH=", 1)
	}
	if l.mutants[AliasEnvLeaks] {
		env += "CONF_CANARY_" + strings.ToUpper(l.n8) + "=1\nUSER=someone\n"
	}
	l.writeOut(conformance.OutEnv, env, false)
	return 0
}

// writeOut writes into the alias-out canary, as the suite's script would.
func (l *Link) writeOut(name, text string, appendTo bool) {
	if l.scriptsSilent {
		return
	}
	c, ok := l.canary["alias-out"]
	if !ok {
		return
	}
	rel := c.Rel + "/" + name
	n := l.nodes[rel]
	if n == nil {
		n = l.newNode(kFile, "")
		l.nodes[rel] = n
	}
	if appendTo {
		n.content += text
	} else {
		n.content = text
	}
	l.touchNode(n)
	l.logMut("alias", rel)
}

func (l *Link) host(args []string, out *[]string) int {
	dir := ""
	if len(args) >= 2 && args[0] == "-C" {
		dir, args = args[1], args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	lie := l.mutants[RefusedHostExecRuns]
	if !l.mutants[HostExecOn] && !lie {
		msg := agentproto.HostExecOff
		if l.trustNow() == conformance.TrustHostile {
			msg = "trust = hostile refuses host_exec: running commands on the host is off"
		}
		if l.faults[HostRefusalUnnamed] {
			msg = "refused"
		}
		*out = append(*out, "caboose-agent: host: "+msg)
		if l.mutants[HostRefusalExitsOdd] {
			return 1
		}
		return 126
	}
	if len(args) == 2 && args[0] == "touch" && dir != "" {
		rel := "roots/" + strings.TrimPrefix(dir, "/work/")
		if p := l.nodes[rel]; p != nil && p.kind == kDir {
			l.nodes[rel+"/"+args[1]] = l.newNode(kFile, "")
			l.logMut("host-touch", rel+"/"+args[1])
		}
	}
	if lie {
		*out = append(*out, "caboose-agent: host: "+agentproto.HostExecOff)
		return 126
	}
	return 0
}

// HostOps.

func (l *Link) StubsInPlace() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stubsMissing {
		return errors.New("the stub notifier is not first on the link's PATH")
	}
	return nil
}

func (l *Link) StubCalls() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stubCalls, nil
}

func (l *Link) ReadCanary(rel string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	inCanary := false
	for _, c := range l.canary {
		if rel == c.Rel || strings.HasPrefix(rel, c.Rel+"/") {
			inCanary = true
		}
	}
	if !inCanary {
		return nil, fmt.Errorf("%s is not a canary", rel)
	}
	n := l.nodes[rel]
	if n == nil {
		return nil, fs.ErrNotExist
	}
	if n.kind != kFile {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return []byte(n.content), nil
}

func (l *Link) ConfigHash() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applyProposals()
	text := fmt.Sprintf("trust=%s aliases=%v", l.trust, l.applied)
	if l.mutants[SandboxWritesConfig] {
		for _, k := range slices.Sorted(maps.Keys(l.home)) {
			text += l.home[k]
		}
	}
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:]), nil
}

func (l *Link) AliasCommand(name string) (string, error) {
	if _, ok := suiteAliases[name]; !ok {
		return "", fmt.Errorf("no alias %q", name)
	}
	return "/tmp/caboose-conf.123/canary/bin/" + name, nil
}

func (l *Link) EnvCanary() string { return "CONF_CANARY_" + strings.ToUpper(l.n8) }

// Wait passes time in the model: a conf-sleep child that survived its
// caller writes its line once 60 seconds have gone by.
func (l *Link) Wait(ctx context.Context, d time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applyProposals()
	if l.later {
		l.later = false
		l.writeOut(conformance.OutRan, conformance.RanLine, true)
	}
	if l.sleeping && d >= 60*time.Second {
		l.sleeping = false
		if l.mutants[RemainsSurvive] {
			l.writeOut(conformance.OutDone, conformance.DoneLine, true)
		}
	}
	return ctx.Err()
}

func (l *Link) EffectiveTrust() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.trustNow(), nil
}

func (l *Link) HostExecOffered(v conformance.ConfigVariant) (bool, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.faults[HostExecNeverOffered]:
		return false, "never", nil
	case !v.HostExec:
		return false, "host_exec is off", nil
	case v.Trust == conformance.TrustHostile && !l.mutants[HostileOffersHostExec]:
		return false, "trust = hostile refuses host_exec", nil
	}
	return true, "", nil
}

func (l *Link) SSHAgentCarried(v conformance.ConfigVariant, standIn string) (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.faults[AgentNeverCarried]:
		return "", "no agent", nil
	case v.Trust == conformance.TrustHostile && !l.mutants[HostileForwardsAgent]:
		return "", `trust = "hostile" forwards no SSH agent unless [link] ssh_agent names one`, nil
	}
	return standIn, "the host's default agent", nil
}

func (l *Link) TryConfig(v conformance.ConfigVariant) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.faults[ConfigNeverLoads] {
		return errors.New("egress: cannot be read")
	}
	if v.Trust == conformance.TrustHostile && v.Isolation == "vm" && v.Egress != nil && !*v.Egress && !l.mutants[HostileEgressOffLoads] {
		return errors.New(`egress = false in [vm.conformance] with trust = "hostile": egress cannot be off`)
	}
	return nil
}

func (l *Link) Create(context.Context) (conformance.LaunchResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mutants[LaunchStartsClaude] {
		l.claude++
	}
	if l.trust == conformance.TrustHostile && l.isolate != "vm" && !l.mutants[HostileOnContainer] {
		return conformance.LaunchResult{Refused: true, Message: "trust = hostile needs a vm isolation profile: the isolation is " + l.isolate}, nil
	}
	l.absent, l.stopped = false, true
	return conformance.LaunchResult{Created: !l.faults[CreatedUnreported]}, nil
}

func (l *Link) Attach(context.Context) (conformance.LaunchResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.absent {
		return conformance.LaunchResult{}, errors.New("no sandbox")
	}
	if l.faults[AttachAlwaysRefused] {
		return conformance.LaunchResult{Refused: true, Message: "refused: run 'caboose restart'"}, nil
	}
	if l.mutants[LaunchStartsClaude] {
		l.claude++
	}
	if l.trust == conformance.TrustHostile && l.made != conformance.TrustHostile && !l.mutants[AttachUnchecked] {
		msg := "trust = hostile needs a vm isolation profile: the sandbox was created otherwise; run 'caboose restart' (this kills running sessions)"
		if l.mutants[AttachRelabels] {
			l.relabel = true
		}
		if l.mutants[AttachNoAdvice] {
			msg = "trust = hostile: refused"
		}
		return conformance.LaunchResult{Refused: true, Message: msg}, nil
	}
	return conformance.LaunchResult{}, nil
}

func (l *Link) SetTrust(trust string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trust = trust
	return nil
}

func (l *Link) ClaudeProcesses() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.claude, nil
}
