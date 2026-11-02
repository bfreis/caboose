package hostlink

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

// fakeVolumes is VolumeFacts with fixed answers, recording what it was
// asked about.
type fakeVolumes struct {
	mu    sync.Mutex
	asked []string
	size  int64
	live  bool
	free  int64
	err   error
}

func (f *fakeVolumes) VolumeFacts(name string) (int64, bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, name)
	return f.size, f.live, f.free, f.err
}

// A resize the sandbox asks for names a volume and a size, nothing more:
// the host checks both, adds what it knows of the volume, and files it.
func TestBrokerRequestVolumeResize(t *testing.T) {
	fd := newFakeDecider()
	fd.set(policy.KindVolumeResize, broker.Outcome{Verdict: broker.Pending, Request: 9, Reason: "waits"})
	fv := &fakeVolumes{size: 64 << 30, live: true, free: 123 << 30}
	a := startExecHost(t, Config{Decide: fd, Actions: &fakeActions{}, Volumes: fv})
	r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &agentproto.Ask{Kind: "volume_resize", Volume: "cache", Size: "128g", Context: "the build cache is full"}})
	if r.OK || r.Verdict != agentproto.VerdictPending || r.RequestID != 9 {
		t.Fatalf("%+v", r)
	}
	fd.mu.Lock()
	if len(fd.filed) != 1 {
		t.Fatalf("filed %+v", fd.filed)
	}
	got := fd.filed[0]
	fd.mu.Unlock()
	want := policy.Request{Kind: policy.KindVolumeResize, Volume: "cache", VolumeSize: 128 << 30, VolumeFrom: 64 << 30, VolumeLive: true, VolumeFree: 123 << 30}
	if got.Request.Kind != want.Kind || got.Request.Volume != want.Volume || got.Request.VolumeSize != want.VolumeSize || got.Request.VolumeFrom != want.VolumeFrom ||
		got.Request.VolumeLive != want.VolumeLive || got.Request.VolumeFree != want.VolumeFree || got.Context != "the build cache is full" {
		t.Fatalf("filed %+v", got)
	}

	for _, c := range []struct {
		name string
		cfg  Config
		ask  agentproto.Ask
		want string
	}{
		{"no volumes", Config{Decide: fd, Actions: &fakeActions{}}, agentproto.Ask{Kind: "volume_resize", Volume: "cache", Size: "128G"}, "only a link the host's caboose daemon runs"},
		{"a bad name", Config{Decide: fd, Actions: &fakeActions{}, Volumes: fv}, agentproto.Ask{Kind: "volume_resize", Volume: "../x", Size: "128G"}, "not a volume name"},
		{"docker", Config{Decide: fd, Actions: &fakeActions{}, Volumes: fv}, agentproto.Ask{Kind: "volume_resize", Volume: "docker", Size: "128G"}, "dockerd storage"},
		{"a long name", Config{Decide: fd, Actions: &fakeActions{}, Volumes: fv}, agentproto.Ask{Kind: "volume_resize", Volume: strings.Repeat("a", agentproto.MaxAskVolume+1), Size: "128G"}, "name is at most 64 bytes"},
		{"a bad size", Config{Decide: fd, Actions: &fakeActions{}, Volumes: fv}, agentproto.Ask{Kind: "volume_resize", Volume: "cache", Size: "lots"}, "size: \"lots\" is not an amount"},
		{"too big", Config{Decide: fd, Actions: &fakeActions{}, Volumes: fv}, agentproto.Ask{Kind: "volume_resize", Volume: "cache", Size: "2048G"}, "outside 4G to 1024G"},
		{"facts fail", Config{Decide: fd, Actions: &fakeActions{}, Volumes: &fakeVolumes{err: errors.New("state.json \x1b[2J")}}, agentproto.Ask{Kind: "volume_resize", Volume: "cache", Size: "128G"}, "cannot tell how the VM has volume cache"},
	} {
		ask := c.ask
		r := startExecHost(t, c.cfg).request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &ask})
		if r.OK || r.Verdict != "" || !strings.Contains(r.Error, c.want) || strings.ContainsRune(r.Error, 0x1b) {
			t.Errorf("%s: %+v, want %q", c.name, r, c.want)
		}
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.filed) != 1 {
		t.Errorf("a refused request was filed: %+v", fd.filed[1:])
	}
}
