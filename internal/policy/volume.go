package policy

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
)

// The rules a volume resize's decision names.
const (
	RuleAskVolumeResize = "ask:volume_resize"
	RuleVolumeLimit     = "deny:volume_limit"
)

// SizeText is n bytes as config.toml writes a size: "64G", or "10.5G"
// for what is not a whole number of GiB.
func SizeText(n int64) string {
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%dG", n>>30)
	}
	return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
}

// VolumeResizeRefusal is why config.toml, as p has it, refuses a resize of
// volume name to size, "" when it does not: no vm profile, no such volume
// in it, a size outside what a disk can be, or a new total of the
// profile's volume sizes past volume_limit. A resize that does not grow
// the volume's size in config.toml is never refused by the limit: it
// brings the total down, or leaves it. rule names the refusal's rule.
func VolumeResizeRefusal(p Policy, name string, size int64) (rule, reason string) {
	vol := shown(name, 64)
	if p.VMProfile == "" {
		return "deny:volume-no-vm", "only a vm profile's volumes have a size, and the sandbox runs under none: there is nothing to resize"
	}
	cur, ok := p.Volumes[name]
	if name == "" || !ok {
		have := "none"
		if len(p.Volumes) > 0 {
			have = strings.Join(slices.Sorted(maps.Keys(p.Volumes)), ", ")
		}
		return "deny:volume-unknown", fmt.Sprintf("profile %s has no volume %q (its volumes: %s); a session may propose one ([volume] in a proposal, the caboose-propose skill)", p.VMProfile, vol, have)
	}
	if size < config.EmptyDiskSize || size > config.MaxScratchSize {
		return "deny:volume-size", fmt.Sprintf("%s is outside %s to %s, the sizes a volume can have", SizeText(size), SizeText(config.EmptyDiskSize), SizeText(config.MaxScratchSize))
	}
	if size <= cur {
		return "", ""
	}
	var others int64
	for n, s := range p.Volumes {
		if n != name {
			others += s
		}
	}
	limit := p.VolumeLimit
	if limit <= 0 {
		limit = config.DefaultVolumeLimit
	}
	if total := others + size; total > limit {
		most := limit - others
		next := fmt.Sprintf("ask for at most %s", SizeText(most))
		if most < max(cur, config.EmptyDiskSize) {
			next = "the other volumes leave no room to grow it"
		}
		return RuleVolumeLimit, fmt.Sprintf("with volume %s at %s, the volumes of %s would total %s, %s over volume_limit (%s) in [%s] of config.toml on the host: %s, or the user raises volume_limit",
			vol, SizeText(size), p.VMProfile, SizeText(total), SizeText(total-limit), SizeText(limit), p.VMProfile, next)
	}
	return "", ""
}

// decideVolumeResize decides a new size for a volume of the vm profile in
// use, which the running VM takes at once: refused when config.toml
// refuses it (VolumeResizeRefusal), when the VM does not have the volume
// on a disk whose size can change while it runs, or when the disk has that
// size already; asked otherwise, whatever the trust. No grant allows one
// (Answer makes none): a person decides each resize.
func decideVolumeResize(p Policy, r Request) Decision {
	if rule, why := VolumeResizeRefusal(p, r.Volume, r.VolumeSize); why != "" {
		return deny(rule, "%s", why)
	}
	vol := shown(r.Volume, 64)
	if !r.VolumeLive {
		return deny("deny:volume-not-live", "the running VM does not have volume %s on one of caboose-vmm's own disks, the only ones whose size changes while it runs: the user resizes it on the host ('caboose volume resize %s %s'), which takes effect at the VM's next start", vol, vol, SizeText(r.VolumeSize))
	}
	if r.VolumeSize == r.VolumeFrom {
		return deny("deny:volume-same", "volume %s is %s already: nothing to change", vol, SizeText(r.VolumeSize))
	}
	return ask(RuleAskVolumeResize, "a new size for volume %s waits for a person's approval on the host", vol)
}

// volumeResizeEffect says what allowing r does, from the host's facts.
func volumeResizeEffect(p Policy, r Request) string {
	vol := shown(r.Volume, 64)
	prof := p.VMProfile
	if prof == "" {
		prof = "no vm profile"
	}
	how := "grows its disk and then its filesystem, which stays mounted"
	if r.VolumeSize < r.VolumeFrom {
		how = "shrinks it: the volume is unmounted in the sandbox meanwhile (refused while anything holds it), its disk cloned first as a backup, then checked and shrunk"
	}
	free := "unknown"
	if r.VolumeFree >= 0 {
		free = SizeText(r.VolumeFree)
	}
	return fmt.Sprintf("resize volume %s of %s from %s to %s while the VM runs: %s; free on this Mac where its disk is: %s",
		vol, prof, SizeText(r.VolumeFrom), SizeText(r.VolumeSize), how, free)
}
