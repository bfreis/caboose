package hostlink

// Volume resizes asked for by the sandbox: `caboose-agent request
// volume-resize NAME SIZE` names a volume and a size, nothing more. The
// profile, the disk and the VM are the host's: the name is checked as
// config.toml's are and looked up in the profile in use by the policy, the
// size parsed as config.toml's sizes are, and what the host knows of the
// volume (its disk's size now, whether the running VM can resize it, the
// space free) is the host's own (VolumeFacts). The broker decides it: never
// allowed without a person, and a person allows each one once.

import (
	"errors"
	"fmt"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
)

// VolumeFacts are what the host knows of the environment's volumes now,
// for a resize the sandbox asks for. Tests fake it.
type VolumeFacts interface {
	// VolumeFacts says, of volume name: its disk's size (0 when it has
	// none), whether the running VM has it on a disk whose size can
	// change while it runs, and the space free on this machine where the
	// disk is (-1 when unknown).
	VolumeFacts(name string) (size int64, live bool, free int64, err error)
}

// errNoVolumes is a resize asked of a link that runs none.
var errNoVolumes = errors.New("this host resizes no volumes for the sandbox: only a link the host's caboose daemon runs does")

// askVolumeResize is the volume resize a asks for, with the host's facts.
func (h *Host) askVolumeResize(a *agentproto.Ask) (policy.Request, error) {
	if h.cfg.Volumes == nil {
		return policy.Request{}, errNoVolumes
	}
	if len(a.Volume) > agentproto.MaxAskVolume || len(a.Size) > agentproto.MaxAskSize {
		return policy.Request{}, fmt.Errorf("a volume's name is at most %d bytes, and its size %d", agentproto.MaxAskVolume, agentproto.MaxAskSize)
	}
	if err := config.CheckVolumeName(a.Volume); err != nil {
		return policy.Request{}, errors.New(clip(proposal.Printable(err.Error()), 300))
	}
	size, err := config.ParseScratchSize(a.Size)
	if err != nil {
		return policy.Request{}, fmt.Errorf("size: %s", clip(proposal.Printable(err.Error()), 300))
	}
	from, live, free, err := h.cfg.Volumes.VolumeFacts(a.Volume)
	if err != nil {
		return policy.Request{}, fmt.Errorf("the host cannot tell how the VM has volume %s now: %s", a.Volume, clip(proposal.Printable(err.Error()), 300))
	}
	return policy.Request{Kind: policy.KindVolumeResize, Volume: a.Volume, VolumeSize: size,
		VolumeFrom: from, VolumeLive: live, VolumeFree: free}, nil
}
