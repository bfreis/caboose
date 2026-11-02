package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm"
)

// TestVolumeFreezeStall measures how long a volume's checkpoint holds up a
// writer while the volume's filesystem has a lot of dirty data: the guest
// keeps up to 90% of its memory dirty, a probe (stallprobe, from DIR's
// stallprobe-linux-arm64: 4 KiB buffered writes every 2 ms) runs
// throughout, CABOOSE_VM_E2E_STALL_MIB (default 1536) MiB are written
// without a sync, and a checkpoint is taken at once; the probe's slowest
// write is the stall. It runs once without a checkpoint, for the baseline,
// then with one. It does so with DIR's agent, and again with DIR's
// caboose-agent-nosync-linux-arm64 when there is one (an agent built
// without the sync before the freeze), for comparison. Skipped unless
// CABOOSE_VM_E2E_STALL is set: it writes a few GiB.
func TestVolumeFreezeStall(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" || os.Getenv("CABOOSE_VM_E2E_STALL") == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files, or CABOOSE_VM_E2E_STALL is unset")
	}
	mib := 1536
	if s := os.Getenv("CABOOSE_VM_E2E_STALL_MIB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		mib = n
	}
	agents := []string{"caboose-agent-linux-arm64"}
	if _, err := os.Stat(filepath.Join(dir, "caboose-agent-nosync-linux-arm64")); err == nil {
		agents = append(agents, "caboose-agent-nosync-linux-arm64")
	}
	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) { stallRun(t, dir, agent, mib) })
	}
}

func stallRun(t *testing.T, dir, agent string, mib int) {
	work, err := os.MkdirTemp(dir, "cvs")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(work, "home")
	env := config.EnvDirFor(home, "default")
	if err := os.MkdirAll(env, 0o755); err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(work, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	probe, err := os.ReadFile(filepath.Join(dir, "stallprobe-linux-arm64"))
	if err != nil {
		t.Skipf("no probe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(share, "stallprobe"), probe, 0o755); err != nil {
		t.Fatal(err)
	}
	h := host{dir: dir, work: work, volSize: 6 << 30, agent: agent, memMiB: 4096}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-stall", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "stall-"+filepath.Base(f)), b, 0o644)
			}
		}
		_ = v.Remove()
		os.RemoveAll(work)
	}()
	start := time.Now()
	out := func(script string) string {
		t.Helper()
		s, err := backend.Output(v, "sh", "-c", script+" 2>&1")
		if err != nil {
			t.Errorf("%s: %v\n%s", script, err, s)
		}
		return strings.TrimSpace(s)
	}
	if err := v.Create(backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Mounts:  []backend.Mount{{Source: share, Target: "/ck"}},
		Volumes: []backend.Volume{{Name: "docker", Target: "/var/lib/docker"}, {Name: "live", Target: "/vol/live", Live: liveVolumes()}},
		Labels:  []string{"caboose.isolation=vm"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	// Writeback only when asked: what is written stays dirty until the
	// freeze, or its sync, writes it out.
	out(`echo 90 > /proc/sys/vm/dirty_ratio; echo 80 > /proc/sys/vm/dirty_background_ratio; echo 360000 > /proc/sys/vm/dirty_expire_centisecs; echo 0 > /proc/sys/vm/dirty_writeback_centisecs`)
	s, err := checkpoint.Open(home, env)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Exposed = func() error { return nil }

	for _, take := range []bool{false, true} {
		what := "baseline"
		if take {
			what = "checkpoint"
		}
		out(`rm -f /vol/live/dirty /tmp/probe.json; sync; echo 3 > /proc/sys/vm/drop_caches`)
		out(`(/ck/stallprobe -file /vol/live/probe -for 30s > /tmp/probe.json) > /dev/null 2>&1 < /dev/null & true`)
		time.Sleep(3 * time.Second)
		t0 := time.Now()
		out(`dd if=/dev/zero of=/vol/live/dirty bs=1M count=` + strconv.Itoa(mib) + ` 2>/dev/null`)
		wrote := time.Since(t0)
		dirty := out(`grep -E '^Dirty:' /proc/meminfo | tr -s ' '`)
		var took time.Duration
		var frozen int64
		if take {
			l, err := s.Lock(context.Background(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			t1 := time.Now()
			r, err := checkpoint.TakeLocked(context.Background(), l, s, checkpoint.Source{
				Path: "/vol/live", Kind: checkpoint.SourceVolume, Top: filepath.Join(work, "volumes"), Rel: "live.img", File: true,
				Volume: "live", Hold: vmHolder{v, "live"},
			}, checkpoint.ReasonManual, checkpoint.TakeOptions{NoPrune: true})
			took = time.Since(t1)
			l.Unlock()
			if err != nil {
				t.Fatalf("checkpoint: %v", err)
			}
			frozen = r.Meta.Volume.FrozenMS
		} else {
			time.Sleep(2 * time.Second)
		}
		for range 120 {
			if out(`pgrep -x stallprobe > /dev/null && echo running || echo done`) == "done" {
				break
			}
			time.Sleep(time.Second)
		}
		var w struct {
			Writes  int     `json:"writes"`
			MaxMS   float64 `json:"max_ms"`
			MeanMS  float64 `json:"mean_ms"`
			Over10  int     `json:"over_10ms"`
			Over100 int     `json:"over_100ms"`
		}
		js := out(`cat /tmp/probe.json`)
		if err := json.Unmarshal([]byte(js), &w); err != nil || w.Writes == 0 {
			t.Fatalf("the probe's output: %v\n%s", err, js)
		}
		step(t, start, "%s, %s: %d MiB written in %v (%s); checkpoint took %v, frozen %d ms (thaw answered after freeze answered); the probe's slowest write %.1f ms, mean %.3f ms, %d writes, %d over 10 ms, %d over 100 ms",
			agent, what, mib, wrote.Round(time.Millisecond), dirty, took.Round(time.Millisecond), frozen, w.MaxMS, w.MeanMS, w.Writes, w.Over10, w.Over100)
	}
	step(t, start, "dmesg: %s", strings.ReplaceAll(out(`dmesg | grep -i -E 'hung|blocked for|EXT4-fs (warning|error)' | tail -5; true`), "\n", " / "))
}
