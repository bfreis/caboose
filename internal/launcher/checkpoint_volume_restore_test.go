package launcher

import (
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// restore without --to is a volume's restore: a path's checkpoint is
// refused, saying how to restore it.
func TestCheckpointRestoreVolumeRefusesPath(t *testing.T) {
	e, _, id := restoreEnv(t, &ckDaemon{w: &daemonproto.WritableResult{}})
	err := e.run(t, checkpointOpts{now: clock()}, "restore", id)
	if err == nil || !strings.Contains(err.Error(), "is of a path, not a volume's disk") || !strings.Contains(err.Error(), "--to DIR") {
		t.Errorf("err %v", err)
	}
}
