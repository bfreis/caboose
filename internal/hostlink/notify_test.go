package hostlink

import (
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/agent"
)

// sandboxActions tell a sandbox's notification from a host component's.
type sandboxActions struct {
	fakeActions
	sandbox []string
}

func (f *sandboxActions) SandboxNotify(title, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sandbox = append(f.sandbox, title+"|"+text)
	return nil
}

// A sandbox's notify reaches SandboxNotify, made printable and cut
// short, with its own title as it gave it (none stays none); Notify, the
// host's, is not called for it.
func TestSandboxNotifyIsTold(t *testing.T) {
	fa := &sandboxActions{}
	socket := linked(t, Config{Ports: PortSet{}, Actions: fa})
	if err := agent.Notify(socket, "Approve\x1b request?", "done\u202e "+strings.Repeat("y", 2*maxText)); err != nil {
		t.Fatal(err)
	}
	if err := agent.Notify(socket, "", "plain"); err != nil {
		t.Fatal(err)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.notes) != 0 || len(fa.sandbox) != 2 {
		t.Fatalf("host %q, sandbox %q", fa.notes, fa.sandbox)
	}
	title, text, _ := strings.Cut(fa.sandbox[0], "|")
	if title != `Approve\u001B request?` || strings.ContainsRune(text, '\u202e') || len(text) > maxText+len("\u2026") {
		t.Errorf("sandbox notification %q", fa.sandbox[0])
	}
	if fa.sandbox[1] != "|plain" {
		t.Errorf("no title became %q", fa.sandbox[1])
	}
}

// Actions without SandboxNotify get a sandbox's notification through
// Notify, as they always did: none for a title is "caboose". The host's
// own Notify never goes to SandboxNotify.
func TestSandboxNotifyFallsBack(t *testing.T) {
	fa := &fakeActions{}
	if err := SandboxNotify(fa, "", "x"); err != nil || len(fa.notes) != 1 || fa.notes[0] != "caboose: x" {
		t.Fatalf("%q %v", fa.notes, err)
	}
	if err := SandboxNotify(fa, "t", ""); err == nil {
		t.Error("an empty text was shown")
	}
	sa := &sandboxActions{}
	if err := Notify(sa, "caboose: a session proposes a change", "y"); err != nil {
		t.Fatal(err)
	}
	if len(sa.sandbox) != 0 || len(sa.notes) != 1 || sa.notes[0] != "caboose: a session proposes a change: y" {
		t.Errorf("host %q, sandbox %q", sa.notes, sa.sandbox)
	}
}
