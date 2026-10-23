package launcher

import (
	"slices"
	"testing"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/hostlink"
)

// recordActions record what reaches them.
type recordActions struct{ got []string }

func (r *recordActions) OpenURL(u string) error { r.got = append(r.got, "open "+u); return nil }
func (r *recordActions) Notify(title, text string) error {
	r.got = append(r.got, "notify "+title+"|"+text)
	return nil
}
func (r *recordActions) Confirm(p string) (bool, error) {
	r.got = append(r.got, "confirm "+p)
	return true, nil
}

// With no app attached, the daemon's notifications reach the host's own
// with the title and text they always had, a sandbox's included; the
// link's browser and dialogs are the actions it wraps.
func TestNotifierActionsFallBackUnchanged(t *testing.T) {
	var shown []string
	n := daemon.NewNotifier(func(title, text string) error { shown = append(shown, title+"|"+text); return nil })
	inner := &recordActions{}
	a := notifierActions{Actions: inner, n: n.For("alpha", daemon.SourceLink)}
	if _, ok := hostlink.Actions(a).(hostlink.SandboxNotifier); !ok {
		t.Fatal("the link's actions do not tell a sandbox's notification")
	}
	_ = hostlink.Notify(a, "caboose: a session proposes a change", "x.toml")
	_ = hostlink.SandboxNotify(a, "", "done")
	_ = hostlink.SandboxNotify(a, "build", "done")
	daemonNotify(n, "alpha", daemon.SourceCheckpoints)("caboose (alpha)", "low space")
	_ = a.OpenURL("https://example.com/")
	_, _ = a.Confirm("open?")
	want := []string{"caboose: a session proposes a change|x.toml", "caboose|done", "build|done", "caboose (alpha)|low space"}
	if !slices.Equal(shown, want) {
		t.Errorf("shown %q, want %q", shown, want)
	}
	if !slices.Equal(inner.got, []string{"open https://example.com/", "confirm open?"}) {
		t.Errorf("the wrapped actions got %q", inner.got)
	}
}
