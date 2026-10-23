package daemonproto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDetailsParamsAndResults(t *testing.T) {
	for _, tc := range []struct {
		p  LinkLogParams
		ok bool
	}{
		{LinkLogParams{Env: "alpha"}, true},
		{LinkLogParams{Env: "alpha", Lines: 1}, true},
		{LinkLogParams{Env: "alpha", Lines: MaxLinkLogLines}, true},
		{LinkLogParams{Env: "alpha", Lines: MaxLinkLogLines + 1}, false},
		{LinkLogParams{Env: "alpha", Lines: -1}, false},
		{LinkLogParams{Env: "a/b", Lines: 5}, false},
	} {
		if err := tc.p.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.p, err)
		}
	}
	long := strings.Repeat("x", MaxLinkLogLine+1)
	for name, r := range map[string]Validator{
		"line over the cut":   &LinkLogResult{Lines: []string{long}},
		"line with an escape": &LinkLogResult{Lines: []string{"\x1b[0m"}},
		"too many lines":      &LinkLogResult{Lines: make([]string, MaxLinkLogLines+1)},
		"bad env":             &EnvDetailsResult{Env: "../x"},
		"session newline":     &EnvDetailsResult{Env: "a", Sessions: []string{"a\nb"}},
		"too many sessions":   &EnvDetailsResult{Env: "a", Sessions: make([]string, MaxDetailSessions+1)},
		"bad port":            &EnvDetailsResult{Env: "a", Ports: []PortDetail{{Port: 0}}},
		"keep control":        &EnvDetailsResult{Env: "a", Keep: []KeepDetail{{Path: "~/\x00"}}},
		"negative resources":  &EnvDetailsResult{Env: "a", Resources: &ResourcesDetail{Vnodes: -1}},
		"error control":       &EnvDetailsResult{Env: "a", Errors: []DetailError{{Part: "keep", Message: "\u202e"}}},
		"bad image state":     &EnvDetailsResult{Env: "a", Image: &ImageDetail{State: "fresh"}},
		"bad level":           &DoctorResult{Findings: []DoctorFinding{{Level: "fine"}}},
		"finding control":     &DoctorResult{Findings: []DoctorFinding{{Level: LevelOK, Message: "a\x1b"}}},
		"too many findings":   &DoctorResult{Findings: make([]DoctorFinding, MaxDoctorFindings+1)},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := &DoctorResult{As: DoctorAsDaemon, Findings: []DoctorFinding{{Level: LevelOK, Label: "git", Message: "fine"}}}
	if err := good.Validate(); err != nil {
		t.Error(err)
	}
	st := EnvStatus{Name: "a", Sandbox: "running", Link: "running", Trust: "hostile\x1b"}
	if (&StatusResult{Envs: []EnvStatus{st}}).Validate() == nil {
		t.Error("a trust with an escape was accepted")
	}
	st = EnvStatus{Name: "a", Sandbox: "running", Link: "running", ModsMissing: -1}
	if (&StatusResult{Envs: []EnvStatus{st}}).Validate() == nil {
		t.Error("negative mods_missing was accepted")
	}
}

// Validator is anything with a Validate.
type Validator interface{ Validate() error }

// Whatever validates is bounded and printable: the page and the command
// line show it.
func FuzzDetailsResults(f *testing.F) {
	for _, s := range []string{
		`{"lines":["a","\u001b"],"truncated":true}`,
		`{"env":"alpha","started":"2026-10-18T12:00:00Z","sessions":[],"ports":[],"keep":[]}`,
		`{"env":"alpha","isolation":"vm","image":{"state":"current"},"sessions":["a"],"ports":[{"port":3000,"url":"http://localhost:3000"}],"keep":[{"path":"~/.ssh","sync":true}],"errors":[{"part":"x","message":"\u202e"}]}`,
		`{"findings":[{"level":"problem","label":"git","message":"m","fix":"f"}],"ran_at":"2026-10-18T12:00:00Z","as":"daemon"}`,
		`{"daemon":{},"envs":[{"name":"a","trust":"\u0000","checkpoints_line":"x","owner":"dev","mods_missing":1}]}`,
		`{"daemon":{},"envs":[],"attention":[{"env":"a","kind":"update","text":"x\u0007","route":"requests/a"}]}`,
		`[]`, `null`, `{"env":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var l LinkLogResult
		if json.Unmarshal(b, &l) == nil && l.Validate() == nil {
			for _, s := range l.Lines {
				if !printable(s) || len(s) > MaxLinkLogLine || len(l.Lines) > MaxLinkLogLines {
					t.Fatalf("accepted %+v", l)
				}
			}
		}
		var d EnvDetailsResult
		if json.Unmarshal(b, &d) == nil && d.Validate() == nil {
			if !ValidEnv(d.Env) || len(d.Sessions) > MaxDetailSessions || len(d.Ports) > MaxDetailPorts || len(d.Keep) > MaxDetailKeep || !printable(d.Isolation) {
				t.Fatalf("accepted %+v", d)
			}
			for _, k := range d.Keep {
				if !printable(k.Path) {
					t.Fatalf("accepted %+v", k)
				}
			}
			for _, e := range d.Errors {
				if !printable(e.Message) || !printable(e.Part) {
					t.Fatalf("accepted %+v", e)
				}
			}
		}
		var r DoctorResult
		if json.Unmarshal(b, &r) == nil && r.Validate() == nil {
			if len(r.Findings) > MaxDoctorFindings {
				t.Fatalf("accepted %d findings", len(r.Findings))
			}
			for _, x := range r.Findings {
				if !printable(x.Message) || !printable(x.Fix) || !printable(x.Label) {
					t.Fatalf("accepted %+v", x)
				}
			}
		}
		var st StatusResult
		if json.Unmarshal(b, &st) == nil && st.Validate() == nil {
			for _, e := range st.Envs {
				if !printable(e.Trust) || !printable(e.CheckpointsLine) || !printable(e.Owner) || e.ModsMissing < 0 {
					t.Fatalf("accepted %+v", e)
				}
			}
		}
	})
}
