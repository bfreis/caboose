package conformance

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// HostOps are the host-side calls of the host-command, alias and trust
// checks: the suite's stubs, the alias scripts' output, the throwaway
// configuration and the launcher's decisions, all in the suite's own
// environment under B. The real Host implements them in the launcher's
// conformance command; a Host that does not makes every check that needs
// them an ERROR, never a PASS.
//
// None of them may start a session or Claude Code, attach a terminal, or
// reach anything outside B: Create and Attach stop at the launcher's
// decision (and, for Create, at a sandbox created but never started).
type HostOps interface {
	// StubsInPlace is nil when the suite's stub opener, notifier and
	// dialog (open, osascript, notify-send and the like) are first on the
	// PATH of the link and the daemon, so nothing a check sets off can
	// reach the person at the host; else why not.
	StubsInPlace() error
	// StubCalls is how many times the stubs have been called this run.
	StubCalls() (int, error)
	// ReadCanary reads a regular file at rel, a canary or below one,
	// without following a symlink, bounded; a file that is not there is an
	// error wrapping fs.ErrNotExist.
	ReadCanary(rel string) ([]byte, error)
	// ConfigHash is the sha256, in hex, of the environment's config.toml.
	ConfigHash() (string, error)
	// AliasCommand is the host path of the suite's script for alias name.
	AliasCommand(name string) (string, error)
	// EnvCanary is the name of a variable the suite exports in its own
	// environment, which no alias may see.
	EnvCanary() string
	// Wait waits for d, or until ctx ends.
	Wait(ctx context.Context, d time.Duration) error
	// EffectiveTrust is the trust the host enforces for the environment
	// now: what the running link and daemon decide by.
	EffectiveTrust() (string, error)
	// HostExecOffered is whether a link under configuration v would offer
	// host exec in its hello, and why not.
	HostExecOffered(v ConfigVariant) (offered bool, why string, err error)
	// SSHAgentCarried is the SSH agent socket a link under configuration
	// v, with ssh_agent unset, would hand the sandbox were the host's
	// default agent socket standIn (its ssh config naming no other): ""
	// for none, and what chose it. It connects to nothing.
	SSHAgentCarried(v ConfigVariant, standIn string) (sock, why string, err error)
	// TryConfig loads a copy of the environment's configuration changed as
	// v says, under B, with the launcher's own loader, and returns its
	// refusal; it changes nothing.
	TryConfig(v ConfigVariant) error
	// Create runs the launcher's decision to create the environment's
	// sandbox and, only when it passes, creates the sandbox without
	// starting it.
	Create(ctx context.Context) (LaunchResult, error)
	// Attach runs the launcher's decision to attach to the existing
	// sandbox, without attaching or starting anything.
	Attach(ctx context.Context) (LaunchResult, error)
	// SetTrust rewrites trust in the environment's config.toml.
	SetTrust(trust string) error
	// ClaudeProcesses counts the Claude Code processes the environment
	// started, in its sandbox or on the host (never the person's own).
	ClaudeProcesses() (int, error)
}

// ConfigVariant is a change to the throwaway configuration: the trust,
// the isolation kind (its profile is <kind>.conformance), host_exec, and
// the profile's egress key when Egress is set.
type ConfigVariant struct {
	Trust, Isolation string
	HostExec         bool
	Egress           *bool
}

// LaunchResult is a launcher decision: refused, and with what message;
// whether a sandbox was created.
type LaunchResult struct {
	Refused bool
	Message string
	Created bool
}

// The trust levels.
const (
	TrustFallible = "fallible"
	TrustHostile  = "hostile"
)

// HostOps are the host calls, inside the envelope only.
func (e Env) HostOps() (HostOps, error) {
	if err := e.sealed(); err != nil {
		return nil, err
	}
	ops, ok := e.host.(HostOps)
	if !ok || ops == nil {
		return nil, errors.New("conformance: this host offers no host-command or launch calls")
	}
	return ops, nil
}

// The suite's aliases, the only ones its configuration defines.
const (
	// AliasMarker appends RanLine to OutRan and writes its environment to
	// OutEnv. It takes one parameter, AliasParam, whose only value is
	// AliasParamValue.
	AliasMarker = "conf-marker"
	// AliasAsk is the marker with policy "ask".
	AliasAsk = "conf-ask"
	// AliasWritable is the marker with its cwd in the writable part.
	AliasWritable = "conf-writable"
	// AliasSleep appends StartLine to OutStart, then, from a background
	// child after 60 seconds, DoneLine to OutDone.
	AliasSleep = "conf-sleep"

	AliasParam      = "mode"
	AliasParamValue = "one"
)

// What the aliases write, inside the alias-out canary.
const (
	OutRan   = "ran"
	OutEnv   = "env"
	OutStart = "sleep-start"
	OutDone  = "sleep-done"

	RanLine   = "ran\n"
	StartLine = "start\n"
	DoneLine  = "done\n"
)

// SuiteAlias is one [alias.NAME] table of the throwaway configuration.
type SuiteAlias struct {
	Name, Policy, Cwd string
	Command           []string
	Params            map[string][]string
	// Script is the alias's script, written at Command[0].
	Script string
}

var hostPathRe = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

func hostPathOK(p string) bool {
	if !hostPathRe.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// SuiteAliases are the throwaway configuration's aliases: scripts in
// scriptDir (outside every root), writing only into outDir (the alias-out
// canary's host path); conf-writable runs in part (the writable part's
// host path). Each is absolute and clean.
func SuiteAliases(scriptDir, outDir, part string) ([]SuiteAlias, error) {
	for _, p := range []string{scriptDir, outDir, part} {
		if !hostPathOK(p) {
			return nil, fmt.Errorf("conformance: %q is not a path the suite's aliases may use", p)
		}
	}
	marker, err := AliasScript(AliasMarker, outDir)
	if err != nil {
		return nil, err
	}
	sleeper, err := AliasScript(AliasSleep, outDir)
	if err != nil {
		return nil, err
	}
	m := scriptDir + "/" + AliasMarker
	s := scriptDir + "/" + AliasSleep
	return []SuiteAlias{
		{Name: AliasMarker, Policy: "allow", Command: []string{m, "{" + AliasParam + "}"},
			Params: map[string][]string{AliasParam: {AliasParamValue}}, Script: marker},
		{Name: AliasAsk, Policy: "ask", Command: []string{m}, Script: marker},
		{Name: AliasWritable, Policy: "allow", Cwd: part, Command: []string{m}, Script: marker},
		{Name: AliasSleep, Policy: "allow", Command: []string{s}, Script: sleeper},
	}, nil
}

// AliasScript is the script of the suite's alias name, writing into
// outDir only.
func AliasScript(name, outDir string) (string, error) {
	if !hostPathOK(outDir) {
		return "", fmt.Errorf("conformance: %q is not a path the suite's aliases may use", outDir)
	}
	q := func(f string) string { return "'" + outDir + "/" + f + "'" }
	switch name {
	case AliasMarker, AliasAsk, AliasWritable:
		return scriptHeader + "printf 'ran\\n' >> " + q(OutRan) + "\nenv > " + q(OutEnv) + "\n", nil
	case AliasSleep:
		return scriptHeader + "printf 'start\\n' >> " + q(OutStart) +
			"\n( sleep 60; printf 'done\\n' >> " + q(OutDone) + " ) &\nwait\n", nil
	}
	return "", fmt.Errorf("conformance: no alias %q in the suite", name)
}
