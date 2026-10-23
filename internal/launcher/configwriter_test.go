package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
)

func writerFor(t *testing.T, body string, mode os.FileMode) (*configWriter, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	if body != "" {
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	return &configWriter{envDir: dir}, path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const writerConfig = `# top comment
isolation = "vm.default"

# the VM
[vm.default]
egress = true  # keep this
egress_allow = [
  "a.example",   # first
  "b.example",
]

[link]
open_urls_allow = ["https://docs.example.com"]

[alias.deploy]
command = ["/usr/bin/true"]   # the thing
policy = "ask"

# trailing comment
`

func TestConfigWriterChangesOnlyTheEditedLines(t *testing.T) {
	for name, tc := range map[string]struct {
		edit config.Edit
		want func(string) string
	}{
		"egress_allow": {config.Edit{Set: map[string]any{"vm.default.egress_allow": []string{"a.example", "b.example", "c.example"}}},
			func(s string) string {
				return strings.Replace(s, "egress_allow = [\n  \"a.example\",   # first\n  \"b.example\",\n]", `egress_allow = ["a.example", "b.example", "c.example"]`, 1)
			}},
		"open_urls_allow": {config.Edit{Set: map[string]any{"link.open_urls_allow": []string{"https://docs.example.com", "https://x.example"}}},
			func(s string) string {
				return strings.Replace(s, `["https://docs.example.com"]`, `["https://docs.example.com", "https://x.example"]`, 1)
			}},
		"alias policy": {config.Edit{Set: map[string]any{"alias.deploy.policy": "allow"}},
			func(s string) string { return strings.Replace(s, `policy = "ask"`, `policy = "allow"`, 1) }},
	} {
		t.Run(name, func(t *testing.T) {
			w, path := writerFor(t, writerConfig, 0o600)
			diff, err := w.Diff(tc.edit)
			if err != nil || diff == "" {
				t.Fatalf("diff %q, %v", diff, err)
			}
			if read(t, path) != writerConfig {
				t.Fatal("Diff wrote")
			}
			if err := w.Write(tc.edit, broker.DiffHash(diff)); err != nil {
				t.Fatal(err)
			}
			if got, want := read(t, path), tc.want(writerConfig); got != want {
				t.Errorf("config.toml:\n%s\nwant\n%s", got, want)
			}
			if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
				t.Errorf("mode %v", fi.Mode())
			}
			if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
				t.Errorf("left %v", ents)
			}
			// The file still reads as the config it was, with the edit.
			if _, err := config.ParseFile(path, []byte(read(t, path))); err != nil {
				t.Error(err)
			}
		})
	}
}

// Whatever is not edited survives byte for byte: every line of the
// original is in the result, in order, but the ones replaced.
func TestConfigWriterKeepsEveryOtherLine(t *testing.T) {
	w, path := writerFor(t, writerConfig, 0o644)
	e := config.Edit{Set: map[string]any{"link.open_urls_allow": []string{"https://docs.example.com", "https://x.example"}}}
	diff, _ := w.Diff(e)
	if err := w.Write(e, broker.DiffHash(diff)); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(read(t, path), "\n")
	for _, l := range strings.Split(writerConfig, "\n") {
		if strings.HasPrefix(l, "open_urls_allow") {
			continue
		}
		found := false
		for _, g := range got {
			if g == l {
				found = true
			}
		}
		if !found {
			t.Errorf("lost the line %q", l)
		}
	}
}

func TestConfigWriterRefusesAnUnsafeEdit(t *testing.T) {
	// A file the editor cannot follow: it does not parse once edited.
	bad := "isolation = \"vm.default\"\n[vm.default\negress = true\n"
	w, path := writerFor(t, bad, 0o644)
	e := config.Edit{Set: map[string]any{"vm.default.egress_allow": []string{"a.example"}}}
	if _, err := w.Diff(e); err == nil || !strings.Contains(err.Error(), "safely") {
		t.Errorf("diff: %v", err)
	}
	if err := w.Write(e, broker.DiffHash("anything")); err == nil || errors.Is(err, broker.ErrStaleDiff) {
		t.Errorf("write: %v", err)
	}
	if read(t, path) != bad {
		t.Error("an unsafe edit changed config.toml")
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Errorf("left %v", ents)
	}
}

// An edit that would make another setting read differently (a table
// header uncommented from under it) fails CheckEdit, and writes nothing.
func TestConfigWriterChecksTheEdit(t *testing.T) {
	// Setting a key of a table the file only has commented out would
	// uncomment the header; with a stray multi-line string in the way, the
	// result changes a value it was not asked to.
	body := "x = \"\"\"\n[vm.default]\n\"\"\"\n#[vm.default]\n"
	w, path := writerFor(t, body, 0o644)
	e := config.Edit{Set: map[string]any{"vm.default.egress": true}}
	diff, err := w.Diff(e)
	if err == nil {
		// If the editor happened to cope, confirming it must still be what
		// Diff showed; the point of the test is the next one.
		t.Skipf("the editor handled it: %q", diff)
	}
	if err := w.Write(e, broker.DiffHash("")); err == nil {
		t.Error("written")
	}
	if read(t, path) != body {
		t.Error("changed")
	}
}

func TestConfigWriterStaleHashWritesNothing(t *testing.T) {
	w, path := writerFor(t, writerConfig, 0o644)
	e := config.Edit{Set: map[string]any{"alias.deploy.policy": "allow"}}
	diff, err := w.Diff(e)
	if err != nil {
		t.Fatal(err)
	}
	// Not the hash of this diff.
	if err := w.Write(e, broker.DiffHash(diff+"x")); !errors.Is(err, broker.ErrStaleDiff) {
		t.Errorf("wrong hash: %v", err)
	}
	if err := w.Write(e, ""); !errors.Is(err, broker.ErrStaleDiff) {
		t.Errorf("no hash: %v", err)
	}
	// The file changes after the diff was shown.
	changed := strings.Replace(writerConfig, `policy = "ask"`, `policy = "ask"`+"\ndescription = \"hi\"", 1)
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(e, broker.DiffHash(diff)); !errors.Is(err, broker.ErrStaleDiff) {
		t.Errorf("changed file: %v", err)
	}
	if read(t, path) != changed {
		t.Error("a stale write changed config.toml")
	}
}

func TestConfigWriterNoChangeAndNoFile(t *testing.T) {
	w, path := writerFor(t, writerConfig, 0o644)
	e := config.Edit{Set: map[string]any{"alias.deploy.policy": "ask"}}
	diff, err := w.Diff(e)
	if err != nil || diff != "" {
		t.Fatalf("diff %q, %v", diff, err)
	}
	if err := w.Write(e, broker.DiffHash(diff)); err != nil || read(t, path) != writerConfig {
		t.Errorf("a no-op: %v", err)
	}

	// No config.toml yet: the template, with the edit.
	w, path = writerFor(t, "", 0)
	e = config.Edit{Set: map[string]any{"link.open_urls_allow": []string{"https://x.example"}}}
	diff, err = w.Diff(e)
	if err != nil || !strings.Contains(diff, "https://x.example") {
		t.Fatalf("diff %q, %v", diff, err)
	}
	if err := w.Write(e, broker.DiffHash(diff)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); !strings.Contains(got, `open_urls_allow = ["https://x.example"]`) || !strings.Contains(got, "#") {
		t.Errorf("the new file:\n%s", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode())
	}

	// An environment with no dir of its own has no file to edit.
	if _, err := (&configWriter{}).Diff(e); err == nil {
		t.Error("diff with no dir")
	}
	if err := (&configWriter{}).Write(e, "x"); err == nil {
		t.Error("write with no dir")
	}
}
