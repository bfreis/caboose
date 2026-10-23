package launcher

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
)

// caboose stop and caboose restart say on the terminal exactly what the
// golden files hold: what they will kill, the question, its answers, and
// the refusals without a terminal or a yes.
func TestStopRestartGolden(t *testing.T) {
	for _, iso := range []string{isolationContainer, isolationVM} {
		t.Run(iso, func(t *testing.T) {
			var b strings.Builder
			var last *boxApp
			for _, sc := range []struct {
				name     string
				cmd      string
				running  bool
				sessions bool
				answer   string // "" is no terminal
				force    bool
			}{
				{"stop, not running", "stop", false, true, "y\n", false},
				{"stop, no sessions", "stop", true, false, "", false},
				{"stop, no terminal", "stop", true, true, "", false},
				{"stop, no", "stop", true, true, "n\n", false},
				{"stop, yes", "stop", true, true, "y\n", false},
				{"stop, forced", "stop", true, true, "", true},
				{"restart, no sessions", "restart", true, false, "", false},
				{"restart, no terminal", "restart", true, true, "", false},
				{"restart, no", "restart", true, true, "\n", false},
				{"restart, yes", "restart", true, true, "yes\n", false},
				{"restart, forced", "restart", true, true, "", true},
				{"restart, absent", "restart", false, false, "", false},
			} {
				box := runningBox(iso)
				switch {
				case !sc.running && sc.cmd == "stop":
					box.Status = "exited"
				case !sc.running:
					box.Status = "absent"
				}
				box.Exec = func(s backend.ExecSpec) *exec.Cmd {
					if sc.sessions && len(s.Argv) > 1 && s.Argv[0] == "tmux" && s.Argv[1] == "list-sessions" {
						return backendtest.Reply("proj-abc123\nproj-abc123-2\n", "", 0)
					}
					return nil
				}
				a := newBoxApp(t, iso, box)
				a.Terminal = func() (io.ReadCloser, error) {
					if sc.answer == "" {
						return nil, errors.New("no terminal")
					}
					return io.NopCloser(strings.NewReader(sc.answer)), nil
				}
				if sc.force {
					a.Cfg.Getenv = func(k string) string { return map[string]string{"CABOOSE_FORCE": "1"}[k] }
				}
				var err error
				if sc.cmd == "stop" {
					err = a.Stop()
				} else {
					err = a.Restart()
				}
				out, errs := a.said()
				said := fmt.Sprintf("== %s\n-- stdout\n%s-- stderr\n%s-- error\n%v\n-- calls\n%s\n", sc.name, out, errs, err, strings.Join(box.Calls, " "))
				b.WriteString(strings.ReplaceAll(said, a.tmp, "TMP"))
				last = a
			}
			golden(t, last, "stop-restart-"+iso+".txt", b.String())
		})
	}
}
