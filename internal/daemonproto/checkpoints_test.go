package daemonproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCheckpointStatusValidate(t *testing.T) {
	ok := &StatusResult{Daemon: DaemonInfo{Version: "v1"}, Envs: []EnvStatus{{Name: "alpha", Sandbox: SandboxRunning, Link: LinkRunning,
		Checkpoints: &CheckpointStatus{Paths: []CheckpointPathStatus{{Path: "/work/p", Key: "p-00000000", Resolved: true,
			Newest: "p-00000000@20261018T120000Z", NewestAgeS: -5, Method: "clonefile"}}, LastError: "boom", Served: "held",
			InProgressSince: time.Now()}}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid status refused: %v", err)
	}
	b, err := json.Marshal(ok)
	if err != nil || !strings.Contains(string(b), `"checkpoints":{"paths":[{"path":"/work/p"`) || !strings.Contains(string(b), `"newest_age_s":-5`) {
		t.Fatalf("encoding %s (%v)", b, err)
	}
	for name, bad := range map[string]func(*CheckpointStatus){
		"control in a path":  func(c *CheckpointStatus) { c.Paths[0].Path = "/work/\x1b[2J" },
		"bidi in a reason":   func(c *CheckpointStatus) { c.Paths[0].Reason = "a\u202eb" },
		"long error":         func(c *CheckpointStatus) { c.LastError = strings.Repeat("e", maxCheckpointText+1) },
		"long key":           func(c *CheckpointStatus) { c.Paths[0].Key = strings.Repeat("k", maxCheckpointShort+1) },
		"too many paths":     func(c *CheckpointStatus) { c.Paths = make([]CheckpointPathStatus, MaxCheckpointPaths+1) },
		"control in exposed": func(c *CheckpointStatus) { c.Exposed = "\x00" },
	} {
		st := *ok.Envs[0].Checkpoints
		st.Paths = append([]CheckpointPathStatus(nil), st.Paths...)
		bad(&st)
		r := &StatusResult{Daemon: ok.Daemon, Envs: []EnvStatus{{Name: "alpha", Checkpoints: &st}}}
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckpointsResultValidate(t *testing.T) {
	good := func() *CheckpointsResult {
		return &CheckpointsResult{Env: "alpha", Paths: []CheckpointListed{{Path: "/work/p", Key: "p-00000000",
			Checkpoints: []CheckpointView{{ID: "p-00000000@20261018T120000Z", Reason: "interval", Entries: 3}}}},
			Orphans: []CheckpointListed{{Path: "/work/q", Key: "q-00000000", Checkpoints: []CheckpointView{}}}}
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("valid listing refused: %v", err)
	}
	for name, bad := range map[string]func(*CheckpointsResult){
		"bad env":          func(r *CheckpointsResult) { r.Env = "../x" },
		"control in ID":    func(r *CheckpointsResult) { r.Paths[0].Checkpoints[0].ID = "x\ny" },
		"negative entries": func(r *CheckpointsResult) { r.Paths[0].Checkpoints[0].Entries = -1 },
		"orphan path":      func(r *CheckpointsResult) { r.Orphans[0].Path = "\x07" },
		"too many":         func(r *CheckpointsResult) { r.Paths[0].Checkpoints = make([]CheckpointView, MaxCheckpointList+1) },
		"too many paths":   func(r *CheckpointsResult) { r.Paths = make([]CheckpointListed, MaxCheckpointPaths+1) },
	} {
		r := good()
		bad(r)
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFitCheckpoints(t *testing.T) {
	long := strings.Repeat("x", maxCheckpointText)
	r := &CheckpointsResult{Env: "alpha"}
	for range 6 {
		l := CheckpointListed{Path: "/p", Key: "k"}
		for range MaxCheckpointList {
			l.Checkpoints = append(l.Checkpoints, CheckpointView{ID: "k@1", Error: long})
		}
		r.Paths = append(r.Paths, l)
	}
	FitCheckpoints(r)
	b, _ := json.Marshal(r)
	if !r.Truncated || len(b) > resultBudget || r.Validate() != nil {
		t.Fatalf("truncated %v, %d bytes", r.Truncated, len(b))
	}
	small := &CheckpointsResult{Env: "alpha", Paths: []CheckpointListed{{Path: "/p", Key: "k", Checkpoints: []CheckpointView{{ID: "a"}}}}}
	FitCheckpoints(small)
	if small.Truncated || len(small.Paths[0].Checkpoints) != 1 {
		t.Fatalf("a small listing was cut: %+v", small)
	}
}

// Whatever a daemon sends that validates is printable and bounded: the
// command line prints it.
func FuzzCheckpointResults(f *testing.F) {
	for _, s := range []string{
		`{"env":"alpha","paths":[{"path":"/work/p","key":"k","checkpoints":[{"id":"k@x","entries":1}]}]}`,
		`{"env":"alpha","paths":[],"orphans":[{"path":"\u001b","key":"k","checkpoints":null}]}`,
		`{"daemon":{"version":"v1"},"envs":[{"name":"alpha","sandbox":"running","link":"running","checkpoints":{"paths":[{"path":"/p","key":"k","resolved":true,"newest_age_s":-1}],"last_error":"x"}}]}`,
		`{"daemon":{},"envs":[{"name":"a","checkpoints":{"paths":null,"exposed":"\u202e"}}]}`,
		`[]`, `null`, `{"env":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var r CheckpointsResult
		if json.Unmarshal(b, &r) == nil && r.Validate() == nil {
			if !ValidEnv(r.Env) || len(r.Paths) > MaxCheckpointPaths || len(r.Orphans) > MaxCheckpointPaths {
				t.Fatalf("accepted %+v", r)
			}
			for _, l := range append(append([]CheckpointListed(nil), r.Paths...), r.Orphans...) {
				if !printable(l.Path) || len(l.Path) > maxCheckpointText || len(l.Checkpoints) > MaxCheckpointList {
					t.Fatalf("accepted %+v", l)
				}
				for _, v := range l.Checkpoints {
					if !printable(v.ID) || !printable(v.Error) || v.Entries < 0 || v.Bytes < 0 {
						t.Fatalf("accepted %+v", v)
					}
				}
			}
		}
		var st StatusResult
		if json.Unmarshal(b, &st) == nil && st.Validate() == nil {
			for _, e := range st.Envs {
				c := e.Checkpoints
				if c == nil {
					continue
				}
				if len(c.Paths) > MaxCheckpointPaths || !printable(c.LastError) || !printable(c.Exposed) || len(c.LastError) > maxCheckpointText {
					t.Fatalf("accepted %+v", c)
				}
				for _, p := range c.Paths {
					if !printable(p.Path) || !printable(p.Reason) || !printable(p.Failing) || len(p.Key) > maxCheckpointShort {
						t.Fatalf("accepted %+v", p)
					}
				}
			}
		}
	})
}
