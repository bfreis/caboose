package launcher

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// A proposed settings part sets a few keys of config.toml: [session] tz,
// hostname and tmux, [build] auto_build, and the cpus and memory of the
// [vm.NAME] profile in use. It widens nothing the sandbox reaches, so it
// is confirmed by the diff alone, but it is still the host's to judge: the
// VM keys need a vm isolation and a profile to write them in (the session
// never names the profile), and a VM cannot be sized past this machine.

// This machine's CPUs and memory, which a VM's size is judged against.
var (
	settingsHostCPUs   = runtime.NumCPU
	settingsHostMemory = hostMemory
)

// settingKey is one key a settings plan sets: its dotted name in
// config.toml, the value as config.toml will have it, and when the
// running sandbox takes it.
type settingKey struct {
	key, value, when string
}

// settingsPlan is a proposed settings part as it would be applied.
type settingsPlan struct {
	keys []settingKey
	edit config.Edit
	diff string
	// restart is set when a key only takes effect at the sandbox's next
	// restart.
	restart bool
}

// planSettings is planProposal's for a settings part: refusals are why it
// cannot be applied at all, warnings what to know first (a value already
// in config.toml is dropped with one). The plan is nil when there are
// refusals, and when nothing is left to set.
func (a *App) planSettings(s proposal.Settings) (pl *settingsPlan, refusals, warnings []string) {
	c := a.Cfg
	pl = &settingsPlan{edit: config.Edit{Set: map[string]any{}}}
	set := func(key, shown string, value any, same bool, when string, restart bool) {
		if same {
			warnings = append(warnings, fmt.Sprintf("%s is %s already: left as it is.", key, shown))
			return
		}
		pl.edit.Set[key] = value
		pl.keys = append(pl.keys, settingKey{key: key, value: shown, when: when})
		pl.restart = pl.restart || restart
	}
	str := func(v string) string { return strconv.Quote(proposal.Printable(v)) }

	if s.TZ != nil {
		set("session.tz", str(*s.TZ), *s.TZ, c.TZ == *s.TZ,
			"sessions started from the next launch use it; what already runs in the "+a.noun()+" keeps its zone until the next restart", false)
	}
	if s.Hostname != nil {
		set("session.hostname", str(*s.Hostname), *s.Hostname, c.Hostname == *s.Hostname,
			"from the "+a.noun()+"'s next restart", true)
	}
	if s.Tmux != nil {
		set("session.tmux", strconv.FormatBool(*s.Tmux), *s.Tmux, c.Tmux == *s.Tmux,
			"sessions started from the next launch", false)
	}
	if s.AutoBuild != nil {
		set("build.auto_build", strconv.FormatBool(*s.AutoBuild), *s.AutoBuild, c.AutoBuild == *s.AutoBuild,
			"from the next launch that has to create the "+a.noun(), false)
	}
	if s.CPUs != nil || s.Memory != nil {
		switch {
		case isolationOf(c) != config.KindVM:
			refusals = append(refusals, fmt.Sprintf("vm.cpus and vm.memory size a VM, and this environment's isolation is %s.", isolationOf(c)))
		case c.Profile == "":
			refusals = append(refusals, "config.toml has no [vm.NAME] profile to set them in.")
		}
	}
	if len(refusals) == 0 && s.CPUs != nil {
		if host := settingsHostCPUs(); *s.CPUs > host {
			refusals = append(refusals, fmt.Sprintf("vm.cpus is %d, and this machine has %d CPUs.", *s.CPUs, host))
		} else {
			set(c.Profile+".cpus", strconv.Itoa(*s.CPUs), *s.CPUs, c.VMCPUs == *s.CPUs, "from the VM's next restart", true)
		}
	}
	if len(refusals) == 0 && s.Memory != nil {
		mib, err := config.ParseMemory(*s.Memory)
		switch host := settingsHostMemory(); {
		case err != nil:
			refusals = append(refusals, fmt.Sprintf("vm.memory: %s.", proposal.Printable(err.Error())))
		case host > 0 && uint64(mib) > host>>20:
			refusals = append(refusals, fmt.Sprintf("vm.memory is %s, and this machine has %d MiB.", str(*s.Memory), host>>20))
		default:
			cur, cerr := config.ParseMemory(c.VMMemory)
			set(c.Profile+".memory", str(*s.Memory), *s.Memory, c.VMMemory != "" && cerr == nil && cur == mib, "from the VM's next restart", true)
		}
	}
	if len(refusals) > 0 {
		return nil, refusals, nil
	}
	if len(pl.keys) == 0 {
		return nil, nil, warnings
	}
	d, err := a.configDiff(pl.edit)
	if err != nil {
		return nil, []string{fmt.Sprintf("It cannot be written into config.toml safely: %s.", proposal.Printable(err.Error()))}, nil
	}
	pl.diff = d
	return pl, nil, warnings
}

// settingsSummary is what applying pl sets, in plain words; what the
// view's hash covers beside the diff.
func (a *App) settingsSummary(pl *settingsPlan) string {
	var parts []string
	for _, k := range pl.keys {
		parts = append(parts, k.key+" = "+k.value)
	}
	return fmt.Sprintf("set %s in %s", strings.Join(parts, ", "), a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)))
}

// showSettings shows what applying pl changes, and when each key takes
// effect.
func (a *App) showSettings(p *prompter, pl *settingsPlan) {
	p.say("Settings in config.toml:")
	p.diff(a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), "with the change", pl.diff)
	p.blank()
	for _, k := range pl.keys {
		p.note("%s: %s", k.key, k.when)
	}
}

// applySettings writes pl's change to config.toml, provided it is still the
// one shown. ok is false when it was not written.
func (a *App) applySettings(p *prompter, s proposal.Settings, pl *settingsPlan) (ok bool, err error) {
	if a.beforeProposalWrite != nil {
		a.beforeProposalWrite()
	}
	// Planned again from what is there now: config.toml edited, or the
	// isolation changed, since it was shown is not written over.
	now, refusals, _ := a.planSettings(s)
	switch {
	case len(refusals) > 0:
		for _, r := range refusals {
			p.fail("%s", r)
		}
		p.fail("It changed since it was shown: nothing was applied, and the proposal is left pending (apply shows it again)")
		return false, nil
	case now == nil || now.diff != pl.diff:
		p.fail("config.toml changed since it was shown: nothing was applied, and the proposal is left pending (apply shows it again)")
		return false, nil
	}
	if _, err := a.writeConfig(pl.edit); err != nil {
		return false, err
	}
	if err := a.rereadConfigFile(); err != nil {
		return false, err
	}
	for _, k := range pl.keys {
		p.ok("Wrote %s to %s: %s", k.key, a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), k.when)
	}
	return true, nil
}
