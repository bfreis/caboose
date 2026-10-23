package daemonproto

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritableResultValidate(t *testing.T) {
	good := func() *WritableResult {
		return &WritableResult{Writable: []WritablePath{{Path: "/Users/u/dev/p", Envs: []string{"alpha", "beta"}}},
			ReadOnly: []WritablePath{{Path: "/Users/u/dev", Envs: []string{"alpha"}}}}
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("valid answer refused: %v", err)
	}
	if err := (&WritableResult{}).Validate(); err != nil {
		t.Fatalf("an empty answer refused: %v", err)
	}
	for name, bad := range map[string]func(*WritableResult){
		"relative":      func(r *WritableResult) { r.Writable[0].Path = "dev/p" },
		"unclean":       func(r *WritableResult) { r.Writable[0].Path = "/a/../b" },
		"empty":         func(r *WritableResult) { r.ReadOnly[0].Path = "" },
		"control":       func(r *WritableResult) { r.Writable[0].Path = "/a\nb" },
		"bidi":          func(r *WritableResult) { r.ReadOnly[0].Path = "/a\u202eb" },
		"long":          func(r *WritableResult) { r.Writable[0].Path = "/" + strings.Repeat("x", maxWritablePath) },
		"no env":        func(r *WritableResult) { r.Writable[0].Envs = nil },
		"bad env":       func(r *WritableResult) { r.Writable[0].Envs = []string{"../x"} },
		"too many envs": func(r *WritableResult) { r.Writable[0].Envs = make([]string, MaxWritableEnvs+1) },
		"too many":      func(r *WritableResult) { r.ReadOnly = make([]WritablePath, MaxWritablePaths+1) },
	} {
		r := good()
		bad(r)
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Whatever a daemon sends that validates is bounded, printable and
// absolute: restore judges a target by it.
func FuzzWritableResult(f *testing.F) {
	for _, s := range []string{
		`{"writable":[{"path":"/a","envs":["alpha"]}],"read_only":[{"path":"/b","envs":["beta","gamma"]}]}`,
		`{"writable":[{"path":"a","envs":["alpha"]}]}`,
		`{"writable":[{"path":"/a/","envs":[]}]}`,
		`{"read_only":[{"path":"/\u001b","envs":["x"]}]}`,
		`[]`, `null`, `{"writable":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var r WritableResult
		if json.Unmarshal(b, &r) != nil || r.Validate() != nil {
			return
		}
		for _, l := range [][]WritablePath{r.Writable, r.ReadOnly} {
			if len(l) > MaxWritablePaths {
				t.Fatalf("accepted %d paths", len(l))
			}
			for _, p := range l {
				if !printable(p.Path) || len(p.Path) > maxWritablePath || !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) != p.Path {
					t.Fatalf("accepted path %q", p.Path)
				}
				if len(p.Envs) == 0 || len(p.Envs) > MaxWritableEnvs {
					t.Fatalf("accepted %d envs", len(p.Envs))
				}
				for _, e := range p.Envs {
					if !ValidEnv(e) {
						t.Fatalf("accepted env %q", e)
					}
				}
			}
		}
	})
}
