package launcher

import (
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
)

// The host's terminal reaches an exec under its own name, and only when the
// host names one.
func TestExecEnvTermProgram(t *testing.T) {
	for _, tc := range []struct {
		name string
		host map[string]string
		want []string
	}{
		{"named", map[string]string{"TERM_PROGRAM": "ghostty", "TERM_PROGRAM_VERSION": "1.2.3"},
			[]string{"TERM_PROGRAM=ghostty", "TERM_PROGRAM_VERSION=1.2.3"}},
		{"name only", map[string]string{"TERM_PROGRAM": "WezTerm"}, []string{"TERM_PROGRAM=WezTerm"}},
		{"unnamed", map[string]string{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := map[string]string{"TERM": "xterm-256color"}
			for k, v := range tc.host {
				host[k] = v
			}
			a := &App{Cfg: &config.Config{Env: "default", Container: "box", TZ: "UTC",
				Getenv: func(k string) string { return host[k] }}, Backend: &backendtest.Fake{Status: "running"}}
			var got []string
			for _, e := range a.execEnv() {
				if k, _, _ := strings.Cut(e, "="); k == "TERM_PROGRAM" || k == "TERM_PROGRAM_VERSION" {
					got = append(got, e)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("execEnv's terminal = %q, want %q", got, tc.want)
			}
		})
	}
}

// tmux sets its own TERM_PROGRAM in every pane, over new-session -e, so the
// host's goes in the command; TZ, which tmux leaves alone, stays an -e.
func TestTmuxSessionArgv(t *testing.T) {
	base := []string{"tmux", "-u", "new-session", "-A", "-s", "proj", "-c", "/work/proj"}
	for _, tc := range []struct {
		name      string
		env, args []string
		want      []string
	}{
		{"bare", []string{"TERM=xterm-256color"}, nil, append(slices.Clone(base), Entrypoint)},
		{"timezone", []string{"TERM=xterm", "TZ=Asia/Kolkata"}, []string{"--resume"},
			append(slices.Clone(base), "-e", "TZ=Asia/Kolkata", Entrypoint, "--resume")},
		{"terminal", []string{"TERM=xterm-ghostty", "COLORTERM=truecolor", "TERM_PROGRAM=ghostty", "TERM_PROGRAM_VERSION=1.2.3", "TZ=UTC"},
			[]string{"-c"},
			append(slices.Clone(base), "-e", "TZ=UTC", "/usr/bin/env", "TERM_PROGRAM=ghostty", "TERM_PROGRAM_VERSION=1.2.3", Entrypoint, "-c")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tmuxSessionArgv("proj", "/work/proj", tc.env, tc.args); !slices.Equal(got, tc.want) {
				t.Errorf("tmuxSessionArgv =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
