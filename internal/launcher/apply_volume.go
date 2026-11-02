package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// A proposed volume part adds [PROFILE.volume.NAME] to config.toml, as
// caboose volume create does: PROFILE is the isolation profile in use (the
// session never names it), the name, path and size are checked as
// config.toml checks them, the file it would make must load (the roots,
// the other volumes, how many a VM takes), and it is refused whole
// otherwise. It reaches nothing of the host's beyond a disk of the size
// shown, so it is confirmed by the diff alone; the VM gets the volume when
// it is next created ('caboose restart').

// volumePlan is a proposed volume part as it would be applied.
type volumePlan struct {
	vol     proposal.Volume
	profile string
	table   string
	// size is the size written, "" outside vm.
	size string
	edit config.Edit
	diff string
}

// planVolume is planProposal's for a volume part: refusals are why it
// cannot be applied at all, warnings what to know first.
func (a *App) planVolume(v proposal.Volume) (pl *volumePlan, refusals, warnings []string) {
	c := a.Cfg
	// Profile is set only from a profile config.toml defines.
	if c.Profile == "" {
		return nil, []string{fmt.Sprintf("A volume is a table of an isolation profile, and config.toml defines none in use (isolation %s): "+
			"the user adds one first ('caboose setup isolation').", isolationOf(c))}, nil
	}
	profile := c.Profile
	kind := profileKind(profile)
	table := profile + ".volume." + v.Name
	if fv, ok := a.fileVolume(profile, v.Name); ok {
		next := "a different name, or 'caboose volume rm " + v.Name + "' on the host first"
		if kind == config.KindVM {
			next = "a new size is asked for with caboose-agent request volume-resize, not proposed"
		}
		return nil, []string{fmt.Sprintf("[%s] is in config.toml already, at %s: %s.", table, proposal.Printable(fv.Path), next)}, nil
	}
	size := v.Size
	switch {
	case kind != config.KindVM && size != "":
		return nil, []string{fmt.Sprintf("Under %s a volume has no size (only a vm profile's volumes are disks of their own): propose it without one.", kind)}, nil
	case kind == config.KindVM && size == "":
		size = sizeValue(config.DefaultVolumeSize)
	}
	set := map[string]any{table + ".path": v.Path}
	if size != "" {
		set[table+".size"] = size
	}
	pl = &volumePlan{vol: v, profile: profile, table: table, size: size, edit: config.Edit{Tables: []string{table}, Set: set}}
	_, data, edited, _, err := a.checkVolumeEdit(pl.edit)
	if err != nil {
		return nil, []string{fmt.Sprintf("config.toml would refuse it: %s.", proposal.Printable(err.Error()))}, nil
	}
	pl.diff = lineDiff(string(data), string(edited))

	if kind != config.KindVM {
		warnings = append(warnings, fmt.Sprintf("Under %s caboose makes no volumes yet, only under vm: the sandbox gets none at that path, and what it writes there stays in the container's own files.", kind))
		return pl, nil, warnings
	}
	n, _ := config.ParseScratchSize(size)
	disk := a.volumeDisk(v.Name)
	if fi, err := os.Stat(disk); err == nil {
		warnings = append(warnings, fmt.Sprintf("Its disk, %s, is here already (%s): the volume gets it as it is, with what it holds.", disk, gibString(fi.Size())))
	} else if w := diskSpaceWarning(c.DataDir, "["+table+"] size", "the volume's disk", n); w != "" {
		warnings = append(warnings, proposal.Printable(w)+".")
	}
	return pl, nil, warnings
}

// volumeProposalSummary is what applying pl adds, in plain words; what the view's
// hash covers beside the diff.
func (a *App) volumeProposalSummary(pl *volumePlan) string {
	s := fmt.Sprintf("add volume %s to %s, mounted at %s", pl.vol.Name, pl.profile, proposal.Printable(pl.vol.Path))
	if pl.size != "" {
		s += ", of up to " + pl.size
	}
	return s + " (" + a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)) + ")"
}

// volumeWhen says when the sandbox gets the volume.
func (a *App) volumeWhen(pl *volumePlan) string {
	if profileKind(pl.profile) != config.KindVM {
		return "it is not attached under " + profileKind(pl.profile)
	}
	return "the VM gets it, its disk made then, when " + envCommand(a.Cfg.Env, "restart") + " recreates it (which ends running sessions), or at its creation when there is none"
}

// showVolume shows what applying pl changes.
func (a *App) showVolume(p *prompter, pl *volumePlan) {
	p.say("A volume of %s ([%s]):", pl.profile, pl.table)
	p.diff(a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), "with the change", pl.diff)
	p.blank()
	p.note("%s: %s", strconv.Quote(proposal.Printable(pl.vol.Path)), a.volumeWhen(pl))
}

// applyVolume writes pl's change to config.toml, provided it is still the
// one shown and still loads. ok is false when it was not written.
func (a *App) applyVolume(p *prompter, v proposal.Volume, pl *volumePlan) (ok bool, err error) {
	if a.beforeProposalWrite != nil {
		a.beforeProposalWrite()
	}
	now, refusals, _ := a.planVolume(v)
	switch {
	case len(refusals) > 0:
		for _, r := range refusals {
			p.fail("%s", r)
		}
		p.fail("It changed since it was shown: nothing was applied, and the proposal is left pending (apply shows it again)")
		return false, nil
	case now.diff != pl.diff:
		p.fail("config.toml changed since it was shown: nothing was applied, and the proposal is left pending (apply shows it again)")
		return false, nil
	}
	if err := a.writeVolumeEdit(pl.edit); err != nil {
		return false, err
	}
	if err := a.rereadConfigFile(); err != nil {
		return false, err
	}
	p.ok("Wrote [%s] to %s: %s", pl.table, a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), a.volumeWhen(pl))
	return true, nil
}
