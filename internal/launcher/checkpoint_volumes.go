package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
)

// A [checkpoints] path that is a volume's path, under vm, is checkpointed
// as the volume's disk: a clone of its image file, vm/volumes/NAME.img in
// the data dir, made while the VM holds the volume's filesystem still
// (backend.VolumeHolder). A path inside a volume is not resolved: what is
// in it lives on the disk, which is cloned whole or not at all. dockerd's
// disk is never checkpointed: it holds images, containers and build cache
// docker makes again, which would churn the checkpoints' blocks for
// nothing (and 'caboose prune --docker' empties it anyway).

// checkpointVolumes are the volumes a checkpoint path can be the path
// of, by name: dockerd's disk, which every VM has (at dockerTarget), the
// profile's, then those the VM was created with (its label), which differ
// until a restart. None but under vm.
func (a *App) checkpointVolumes() map[string][]string {
	if !a.isVM() {
		return nil
	}
	out := map[string][]string{}
	add := func(name, target string) {
		if !slices.Contains(out[name], target) {
			out[name] = append(out[name], target)
		}
	}
	add(dockerVolume, dockerTarget)
	for _, v := range a.Cfg.Volumes {
		add(v.Name, v.Path)
	}
	if labels, err := a.box().Labels(); err == nil {
		var have map[string]string
		if json.Unmarshal([]byte(labels[assets.LabelVolumes]), &have) == nil {
			for _, n := range slices.Sorted(maps.Keys(have)) {
				add(n, have[n])
			}
		}
	}
	return out
}

// volumeSource is checkpoint path p's source when p is at or inside a
// volume of vols: ok is false when it is neither, and reason says why one
// inside a volume, or dockerd's, is not copied.
func (a *App) volumeSource(p string, vols map[string][]string) (s checkpoint.Source, reason string, ok bool) {
	ctr := p
	if rel, home := strings.CutPrefix(p, "~/"); home {
		ctr = config.ContainerHome + "/" + rel
	}
	for _, name := range slices.Sorted(maps.Keys(vols)) {
		for _, target := range vols[name] {
			switch {
			case target == "" || !config.Within(ctr, target):
				continue
			case name == dockerVolume:
				return s, fmt.Sprintf("%s is on dockerd's own disk, which a checkpoint never copies: it holds what docker makes again (images, containers, build cache)", p), true
			case ctr != target:
				return s, fmt.Sprintf("%s is inside volume %s (%s), whose disk a checkpoint clones whole: list %s in [checkpoints] paths instead", p, name, target, displayPath(target)), true
			}
			top, err := config.Physical(a.volumesDir())
			if err != nil {
				return s, fmt.Sprintf("volume %s's disk cannot be reached: %v", name, err), true
			}
			return checkpoint.Source{Path: p, Kind: checkpoint.SourceVolume, Top: top, Rel: name + ".img", File: true,
				Volume: name, Hold: &volumeHolder{a, name}}, "", true
		}
	}
	return s, "", false
}

// displayPath is a container path as [checkpoints] paths writes it: ~/REL
// under the home.
func displayPath(p string) string {
	if rel, ok := strings.CutPrefix(p, config.ContainerHome+"/"); ok {
		return "~/" + rel
	}
	return p
}

// volumeHolder is the checkpoint.Holder of a volume: the sandbox's own
// hold, asked when the checkpoint is taken.
type volumeHolder struct {
	a    *App
	name string
}

func (h *volumeHolder) Hold(ctx context.Context, during func() error) (checkpoint.Held, error) {
	b, ok := h.a.box().(backend.VolumeHolder)
	if !ok {
		return checkpoint.Held{}, errors.New("the sandbox is no VM, which alone has volumes on disks of their own")
	}
	held, err := b.HoldVolume(ctx, h.name, during)
	return checkpoint.Held{Frozen: held.Frozen, FrozenFor: held.FrozenFor}, err
}
