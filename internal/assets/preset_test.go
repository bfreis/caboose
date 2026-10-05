package assets

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSections(t *testing.T) {
	var names []string
	for _, s := range Sections() {
		names = append(names, s.Name)
		if s.Title == "" {
			t.Errorf("section %s has no title", s.Name)
		}
	}
	if want := []string{"node", "bun", "go", "gh", "jj", "docker", "dockerd", "sudo", "rust"}; !slices.Equal(names, want) {
		t.Errorf("sections %v, want %v", names, want)
	}
	if got := DefaultSections(); !slices.Equal(got, []string{"node", "bun", "go", "gh", "jj", "docker", "dockerd", "sudo"}) {
		t.Errorf("DefaultSections = %v", got)
	}
}

// What the image check requires is outside every section: no choice in
// setup can leave it out.
func TestCoreIsNotOptional(t *testing.T) {
	data, err := Preset(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"FROM ubuntu:", "ca-certificates curl git openssh-client", "ripgrep", "tmux", "ncurses-term"} {
		if !strings.Contains(s, want) {
			t.Errorf("the core lacks %q", want)
		}
	}
	for _, gone := range []string{"NODE_VERSION", "BUN_VERSION", "GO_VERSION", "GH_VERSION", "JJ_VERSION", "DOCKER_CLI_VERSION", "rustup", "\n# caboose:section"} {
		if strings.Contains(s, gone) {
			t.Errorf("no sections, yet %q is in it", gone)
		}
	}
	if h, ok := ReadHeader(data); !ok || h.ID != PresetID() || h.Sections != nil {
		t.Errorf("header %+v %v", h, ok)
	}
}

// The default preset is the embedded Dockerfile, less its off sections,
// under the header: what an environment without image/ builds.
func TestDefaultPresetIsTheDockerfile(t *testing.T) {
	data, err := Preset(DefaultSections())
	if err != nil {
		t.Fatal(err)
	}
	h, ok := ReadHeader(data)
	if !ok || !slices.Equal(h.Sections, DefaultSections()) {
		t.Fatalf("header %+v %v", h, ok)
	}
	_, body, _ := strings.Cut(string(data), "\n#\n# Written by caboose setup")
	_, body, _ = strings.Cut(body, "\n#\n")
	want := string(embeddedDockerfile())
	i := strings.Index(want, "# caboose:section rust off")
	j := strings.Index(want[i:], "# caboose:end\n") + i + len("# caboose:end\n")
	want = want[:i] + want[j:]
	if body != want {
		t.Errorf("the default preset differs from the Dockerfile:\n%s", body)
	}
}

// An off section, chosen, is uncommented: its instructions run.
func TestPresetUncommentsAnOffSection(t *testing.T) {
	data, err := Preset([]string{"rust"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"\n# caboose:section rust Rust (rustup, with the stable toolchain)\n# Rust through rustup",
		"\nENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH\nRUN set -eux; \\\n    case ",
		"\n    fetch 1800 \"https://static.rust-lang.org/rustup/dist/${target}/rustup-init\" -o /tmp/rustup-init; \\\n",
		"\n    cargo --version\n# caboose:end\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "NODE_VERSION") || !strings.HasPrefix(s, "# caboose:preset "+PresetID()+" rust\n") {
		t.Errorf("preset:\n%s", s)
	}
	if _, err := Preset([]string{"cobol"}); err == nil {
		t.Error("an unknown section was taken")
	}
}

func TestParseSectionsRefuses(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"# caboose:section a A\n# caboose:section b B\n# caboose:end\n", "inside section a"},
		{"# caboose:section a A\n# caboose:end\n# caboose:section a A\n# caboose:end\n", "section a again"},
		{"# caboose:end\n", "outside a section"},
		{"# caboose:section a A\nRUN x\n", "never ended"},
		{"# caboose:section a off A\nRUN x\n# caboose:end\n", "must be commented out"},
		{"# caboose:sectoin a A\n", "unknown marker"},
	} {
		if _, err := parseSections([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.in, err, tc.want)
		}
	}
}

func TestReadHeader(t *testing.T) {
	for _, tc := range []struct {
		in   string
		ok   bool
		want Header
	}{
		{"# caboose:preset abc123 node,go\nFROM x\n", true, Header{"abc123", []string{"node", "go"}}},
		{"# caboose:preset abc123 \n", true, Header{ID: "abc123"}},
		{"FROM x\n", false, Header{}},
		{"# caboose:preset \n", false, Header{}},
	} {
		h, ok := ReadHeader([]byte(tc.in))
		if ok != tc.ok || h.ID != tc.want.ID || !slices.Equal(h.Sections, tc.want.Sections) {
			t.Errorf("%q: %+v %v", tc.in, h, ok)
		}
	}
}

func TestDirHash(t *testing.T) {
	dir := t.TempDir()
	write := func(p, data string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, p), mode); err != nil {
			t.Fatal(err)
		}
	}
	hash := func() string {
		t.Helper()
		h, err := DirHash(dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	write("Dockerfile", "FROM x\n", 0o644)
	write("files/a.sh", "echo a\n", 0o755)
	seen := map[string]string{hash(): "start"}
	if a, b := hash(), hash(); a != b {
		t.Fatal("not stable")
	}
	for _, step := range []struct {
		name string
		do   func()
	}{
		{"an edit", func() { write("Dockerfile", "FROM y\n", 0o644) }},
		{"a chmod", func() { write("files/a.sh", "echo a\n", 0o644) }},
		{"a new file", func() { write("README", "", 0o644) }},
		{"a rename", func() {
			if err := os.Rename(filepath.Join(dir, "README"), filepath.Join(dir, "README.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlink", func() {
			if err := os.Symlink("files/a.sh", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"its target", func() {
			if err := os.Remove(filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("files/b.sh", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"bytes moved between files", func() {
			write("Dockerfile", "FROM ", 0o644)
			write("files/a.sh", "yecho a\n", 0o644)
		}},
	} {
		step.do()
		h := hash()
		if prev, ok := seen[h]; ok {
			t.Errorf("%s: same hash as after %s", step.name, prev)
		}
		seen[h] = step.name
	}
	if _, err := DirHash(filepath.Join(dir, "nope")); err == nil {
		t.Error("no error for a missing dir")
	}
}
