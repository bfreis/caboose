package launcher

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
)

var testRoots = []config.Root{{Host: "/r", Container: "/work"}}

func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillOf(checkout string) string {
	return filepath.Join(checkout, assets.SandboxSkillsPath, "one", "SKILL.md")
}

func syncApp(t *testing.T, checkout string) (a *App, data string, errb *bytes.Buffer) {
	t.Helper()
	data = t.TempDir()
	errb = &bytes.Buffer{}
	a = &App{Cfg: &config.Config{Roots: testRoots, DataDir: data, Env: config.DefaultEnv,
		ImageProfile: config.ImageProfile{Kind: config.ImageKindApko, Name: "default"}},
		Docker: &docker.CLI{Path: "false"}, Checkout: checkout, Stderr: errb}
	return a, data, errb
}

func installed(t *testing.T, data, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(data, datadir.ManagedDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What a launch installs without a checkout is the embedded set, filled in:
// no placeholder or directive left, whatever the sandbox is.
func TestEmbeddedFilesExpandForEveryFacts(t *testing.T) {
	files, err := EmbeddedSandboxFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[".claude/skills/caboose-propose/SKILL.md"]; !ok {
		t.Fatal("no sandbox/skills/caboose-propose/SKILL.md embedded")
	}
	// A clone under a root, for source=clone.
	cloneRoot := t.TempDir()
	writeFile(t, filepath.Join(cloneRoot, "caboose", "go.mod"), "module "+datadir.Module+"\n")
	if err := os.Mkdir(filepath.Join(cloneRoot, "caboose", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []config.Root{{Host: cloneRoot, Container: "/work"}}
	sources := map[string]datadir.Facts{
		datadir.SourceCheckout: {Checkout: filepath.Join(cloneRoot, "caboose"), Roots: roots},
		datadir.SourceClone:    {Roots: roots},
		datadir.SourceOutside:  {Checkout: "/elsewhere/caboose", Roots: roots},
		datadir.SourceRelease:  {Roots: []config.Root{{Host: "/nowhere", Container: "/work"}}},
	}
	for src, base := range sources {
		for _, iso := range []string{"container", "gvisor", "vm"} {
			for _, img := range []string{"apko", "dockerfile", "ref"} {
				for _, hx := range []bool{false, true} {
					for _, env := range []string{config.DefaultEnv, "work"} {
						f := base
						f.Isolation, f.Profile, f.Image, f.ImageProfile = iso, iso+".p", img, img+".default"
						f.HostExec, f.Env, f.Version = hx, env, "v1"
						out, err := datadir.ExpandTree(files, f)
						if err != nil {
							t.Errorf("%s/%s/%s/%v/%s: %v", src, iso, img, hx, env, err)
							continue
						}
						for name, b := range out {
							if strings.Contains(string(b), "@@") {
								t.Errorf("%s/%s/%s/%v/%s: %s still has @@", src, iso, img, hx, env, name)
							}
							if err := checkInstructionSize(name, string(b)); err != nil {
								t.Errorf("%s/%s/%s/%v/%s: %v", src, iso, img, hx, env, err)
							}
						}
					}
				}
			}
		}
	}
}

// Every session loads CLAUDE.md whole, and every skill's description, so
// both have a budget.
const (
	maxClaudeLines      = 125
	maxSkillDescription = 1000
)

// checkInstructionSize says what of one expanded file is over its budget:
// CLAUDE.md's lines, a skill's frontmatter description.
func checkInstructionSize(name, text string) error {
	if name == "CLAUDE.md" {
		if n := strings.Count(strings.TrimRight(text, "\n"), "\n") + 1; n > maxClaudeLines {
			return fmt.Errorf("CLAUDE.md is %d lines, over %d", n, maxClaudeLines)
		}
		return nil
	}
	if path.Base(name) != "SKILL.md" {
		return nil
	}
	front, ok := strings.CutPrefix(text, "---\n")
	if ok {
		front, _, ok = strings.Cut(front, "\n---\n")
	}
	if !ok {
		return fmt.Errorf("%s has no frontmatter", name)
	}
	for _, line := range strings.Split(front, "\n") {
		if d, ok := strings.CutPrefix(line, "description:"); ok {
			if n := len(strings.TrimSpace(d)); n > maxSkillDescription {
				return fmt.Errorf("%s: description is %d characters, over %d", name, n, maxSkillDescription)
			}
			return nil
		}
	}
	return fmt.Errorf("%s has no description", name)
}

func TestInstructionSizeCheck(t *testing.T) {
	long := strings.Repeat("x\n", maxClaudeLines+1)
	if checkInstructionSize("CLAUDE.md", long) == nil {
		t.Error("a CLAUDE.md over budget passed")
	}
	if err := checkInstructionSize("CLAUDE.md", strings.Repeat("x\n", maxClaudeLines)); err != nil {
		t.Error(err)
	}
	skill := func(desc string) string { return "---\nname: one\ndescription: " + desc + "\n---\n\nbody\n" }
	if checkInstructionSize(".claude/skills/one/SKILL.md", skill(strings.Repeat("d", maxSkillDescription+1))) == nil {
		t.Error("a description over budget passed")
	}
	if err := checkInstructionSize(".claude/skills/one/SKILL.md", skill("short")); err != nil {
		t.Error(err)
	}
	if checkInstructionSize(".claude/skills/one/SKILL.md", "no frontmatter\n") == nil {
		t.Error("a skill without frontmatter passed")
	}
}

func TestSandboxFilesSource(t *testing.T) {
	emb, err := EmbeddedSandboxFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := SandboxFiles(""); err != nil || len(got) != len(emb) {
		t.Errorf("no checkout: %d files, %v", len(got), err)
	}

	// A checkout without skills: its CLAUDE.md, the embedded skills.
	co := makeCheckout(t, ModulePath)
	got, err := SandboxFiles(co)
	if err != nil || string(got["CLAUDE.md"]) != "live @@CABOOSE_SOURCE@@\n" || len(got) != len(emb) {
		t.Errorf("checkout without skills: %d files, %q, %v", len(got), got["CLAUDE.md"], err)
	}

	// With skills: theirs, dotfiles skipped, the embedded ones not mixed in.
	writeFile(t, skillOf(co), "mine\n")
	writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "one", "ref", "x.txt"), "x\n")
	writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, ".git", "y"), "y\n")
	writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "one", ".hidden"), "z\n")
	got, err = SandboxFiles(co)
	if err != nil || len(got) != 3 || string(got[".claude/skills/one/SKILL.md"]) != "mine\n" || string(got[".claude/skills/one/ref/x.txt"]) != "x\n" {
		t.Errorf("checkout skills: %v, %v", got, err)
	}
}

func TestSandboxFilesRefusesWhatItCannotTrust(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	writeFile(t, secret, "secret\n")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "SKILL.md"), "outside\n")

	for name, plant := range map[string]func(co string){
		"symlinked file": func(co string) {
			writeFile(t, skillOf(co), "ok\n")
			os.Symlink(secret, filepath.Join(co, assets.SandboxSkillsPath, "one", "leak.md"))
		},
		"symlinked skill dir": func(co string) {
			os.MkdirAll(filepath.Join(co, assets.SandboxSkillsPath), 0o755)
			os.Symlink(outside, filepath.Join(co, assets.SandboxSkillsPath, "one"))
		},
		"symlinked skills dir": func(co string) {
			os.Symlink(outside, filepath.Join(co, assets.SandboxSkillsPath))
		},
		"too many files": func(co string) {
			for i := 0; i <= maxSkillFiles; i++ {
				writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "one", strings.Repeat("a", i+1)), "x")
			}
		},
		"too large": func(co string) {
			writeFile(t, skillOf(co), strings.Repeat("x", maxSkillBytes+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			co := makeCheckout(t, ModulePath)
			plant(co)
			if got, err := SandboxFiles(co); err == nil {
				t.Errorf("used it: %d files", len(got))
			}
		})
	}

	// A symlinked CLAUDE.md is not followed either: the embedded copy.
	co := makeCheckout(t, ModulePath)
	p := filepath.Join(co, assets.SandboxInstructionsPath)
	os.Remove(p)
	os.Symlink(secret, p)
	emb, _ := assets.SandboxInstructions()
	if b, _ := SandboxInstructions(co); !bytes.Equal(b, emb) {
		t.Errorf("followed a link: %q", b)
	}
}

// A checkout newer than the launcher -- pulled, not rebuilt -- can carry a
// placeholder or directive this binary cannot fill, in CLAUDE.md or in any
// skill. The whole embedded set goes in instead, with a note, rather than
// @@SOMETHING@@ reaching every session literally or the two disagreeing; a
// set it can fill is still installed from the checkout.
func TestSyncFallsBackOnUnknownPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name, claude, skill string
		embedded            bool
		note                string
	}{
		{"unknown placeholder", "live @@CABOOSE_SOURCE@@ @@CABOOSE_FROM_THE_FUTURE@@\n", "s\n", true, "(@@CABOOSE_FROM_THE_FUTURE@@)"},
		{"unknown directive key", "@@IF colour=red@@\nx\n@@END@@\n", "s\n", true, "unknown condition key"},
		{"unknown in a skill", "live\n", "s @@CABOOSE_FROM_THE_FUTURE@@\n", true, "SKILL.md: uses placeholders"},
		{"unbalanced in a skill", "live\n", "@@IF vm@@\n", true, "SKILL.md"},
		{"known", "live @@CABOOSE_SOURCE@@ in @@CABOOSE_ROOTS@@\n@@IF env=default@@\ndefault\n@@END@@\n", "s @@CABOOSE_ENV@@\n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkout := makeCheckout(t, ModulePath)
			writeFile(t, filepath.Join(checkout, assets.SandboxInstructionsPath), tc.claude)
			writeFile(t, skillOf(checkout), tc.skill)
			a, data, errb := syncApp(t, checkout)
			if err := a.syncSandboxInstructions(); err != nil {
				t.Fatal(err)
			}
			src := map[string][]byte{"CLAUDE.md": []byte(tc.claude), ".claude/skills/one/SKILL.md": []byte(tc.skill)}
			if tc.embedded {
				src, _ = EmbeddedSandboxFiles()
			}
			want, err := datadir.ExpandTree(src, a.instructionFacts())
			if err != nil {
				t.Fatal(err)
			}
			for name, w := range want {
				if got := installed(t, data, name); got != string(w) {
					t.Errorf("%s installed:\n%s\nwant:\n%s", name, got, w)
				}
			}
			if tc.embedded {
				if _, err := os.Stat(filepath.Join(data, datadir.ManagedSkills, "one")); err == nil {
					t.Error("the checkout's skill was installed beside the embedded set")
				}
				if !strings.Contains(errb.String(), tc.note) || !strings.Contains(errb.String(), "rebuild the launcher (make launcher") ||
					strings.Contains(errb.String(), "remove or fix") {
					t.Errorf("stderr:\n%s", errb.String())
				}
			} else if strings.Contains(errb.String(), "make launcher") || strings.Contains(errb.String(), "remove or fix") {
				t.Errorf("stderr:\n%s", errb.String())
			}
		})
	}
}

// Without a checkout, an installed launcher writes its embedded set: the
// skills too, byte for byte what the data dir then holds.
func TestSyncWithoutCheckout(t *testing.T) {
	a, data, _ := syncApp(t, "")
	if err := a.syncSandboxInstructions(); err != nil {
		t.Fatal(err)
	}
	emb, _ := EmbeddedSandboxFiles()
	for name := range emb {
		if installed(t, data, name) == "" {
			t.Errorf("%s is empty", name)
		}
	}
	var n int
	filepath.WalkDir(filepath.Join(data, datadir.ManagedSkills), func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			n++
		}
		return nil
	})
	if n != len(emb)-1 {
		t.Errorf("%d skill files installed, want %d", n, len(emb)-1)
	}
	// A second launch changes nothing.
	var errb bytes.Buffer
	a.Stderr = &errb
	if err := a.syncSandboxInstructions(); err != nil || errb.Len() != 0 {
		t.Errorf("second sync: %v, %q", err, errb.String())
	}
}

// The facts are the running sandbox's isolation and profile where it has
// them, else the configuration's; the image is always the configured one.
func TestInstructionFacts(t *testing.T) {
	a, _, _ := syncApp(t, "")
	a.Cfg.Isolation, a.Cfg.Profile = "gvisor", "gvisor.main"
	f := a.instructionFacts()
	if f.Env != config.DefaultEnv || f.Isolation != "gvisor" || f.Profile != "gvisor.main" || f.Image != "apko" || f.ImageProfile != "apko.default" || f.Version == "" {
		t.Errorf("configured: %+v", f)
	}

	b := newBoxApp(t, isolationVM, runningBox(isolationVM))
	b.Cfg.Env = "work"
	b.Cfg.Isolation = "gvisor"
	b.box.SandboxLabels[assets.LabelBaseKind] = assets.BaseKindDockerfile
	f = b.instructionFacts()
	if f.Env != "work" || f.Isolation != isolationVM || f.Profile != isolationVM || f.Image != "ref" || f.ImageProfile != "ref.default" {
		t.Errorf("running: %+v", f)
	}
}

// A sandbox file that is a sparse terabyte is refused without being read,
// and the embedded set goes in.
func TestSyncRefusesHugeFiles(t *testing.T) {
	for name, path := range map[string]func(co string) string{
		"CLAUDE.md": func(co string) string { return filepath.Join(co, assets.SandboxInstructionsPath) },
		"a skill":   skillOf,
	} {
		t.Run(name, func(t *testing.T) {
			co := makeCheckout(t, ModulePath)
			writeFile(t, skillOf(co), "s\n")
			p := path(co)
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(1 << 40); err != nil {
				t.Skip("no sparse files here:", err)
			}
			f.Close()
			a, data, errb := syncApp(t, co)
			if err := a.syncSandboxInstructions(); err != nil {
				t.Fatal(err)
			}
			emb, _ := EmbeddedSandboxFiles()
			if got := installed(t, data, "CLAUDE.md"); got == "" || len(emb) == 0 {
				t.Error("nothing installed")
			}
			if !strings.Contains(errb.String(), "remove or fix") || strings.Contains(errb.String(), "make launcher") {
				t.Errorf("stderr:\n%s", errb.String())
			}
		})
	}
}

// Never a Die over the instructions: a set that cannot be filled in
// leaves what is installed, with a note.
func TestSyncNeverDiesOverInstructions(t *testing.T) {
	a, data, errb := syncApp(t, "")
	if err := a.syncSandboxInstructions(); err != nil {
		t.Fatal(err)
	}
	before := installed(t, data, "CLAUDE.md")
	// A clone the sandbox named @@X@@ is no source; the embedded set still
	// fills in.
	root := t.TempDir()
	clone := filepath.Join(root, "@@X@@", "caboose")
	writeFile(t, filepath.Join(clone, "go.mod"), "module "+datadir.Module+"\n")
	if err := os.Mkdir(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Cfg.Roots = []config.Root{{Host: root, Container: "/work"}}
	errb.Reset()
	if err := a.syncSandboxInstructions(); err != nil {
		t.Fatal(err)
	}
	if got := installed(t, data, "CLAUDE.md"); strings.Contains(got, "@@") || got == "" {
		t.Errorf("installed %q (before: %q)", got, before)
	}
}

func TestSandboxFilesLimits(t *testing.T) {
	skills := func(co string, n int) {
		for i := 0; i < n; i++ {
			writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "one", strings.Repeat("a", i+1)+".txt"), "x")
		}
	}
	co := makeCheckout(t, ModulePath)
	skills(co, maxSkillFiles)
	if got, err := SandboxFiles(co); err != nil || len(got) != maxSkillFiles+1 {
		t.Errorf("exactly %d files: %d, %v", maxSkillFiles, len(got), err)
	}
	co = makeCheckout(t, ModulePath)
	skills(co, maxSkillFiles+1)
	if _, err := SandboxFiles(co); err == nil {
		t.Errorf("%d files used", maxSkillFiles+1)
	}

	// Entries that are not kept count too: directories and skipped files.
	co = makeCheckout(t, ModulePath)
	writeFile(t, skillOf(co), "s\n")
	for i := 0; i <= maxSkillEntries; i++ {
		writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, ".skipped", fmt.Sprint(i)), "x")
	}
	if _, err := SandboxFiles(co); err != nil {
		// The skipped dot directory is not entered: one entry.
		t.Errorf("a skipped directory was walked: %v", err)
	}
	co = makeCheckout(t, ModulePath)
	for i := 0; i <= maxSkillEntries; i++ {
		if err := os.MkdirAll(filepath.Join(co, assets.SandboxSkillsPath, "d"+fmt.Sprint(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SandboxFiles(co); err == nil {
		t.Error("an empty-directory flood was used")
	}
	co = makeCheckout(t, ModulePath)
	for i := 0; i <= maxSkillEntries; i++ {
		writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "one", ".h"+fmt.Sprint(i)), "x")
	}
	if _, err := SandboxFiles(co); err == nil {
		t.Error("a flood of skipped files was used")
	}

	// Depth.
	co = makeCheckout(t, ModulePath)
	writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "a", "b", "c", "e.md"), "x")
	if _, err := SandboxFiles(co); err != nil {
		t.Errorf("depth %d: %v", maxSkillDepth, err)
	}
	co = makeCheckout(t, ModulePath)
	writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "a", "b", "c", "d", "e.md"), "x")
	if _, err := SandboxFiles(co); err == nil {
		t.Errorf("depth %d used", maxSkillDepth+1)
	}

	// Total bytes: the CLAUDE.md counts.
	co = makeCheckout(t, ModulePath)
	writeFile(t, filepath.Join(co, assets.SandboxInstructionsPath), strings.Repeat("x", maxSkillBytes/2+1))
	writeFile(t, skillOf(co), strings.Repeat("x", maxSkillBytes/2))
	if _, err := SandboxFiles(co); err == nil {
		t.Error("over the byte budget in all")
	}
	writeFile(t, skillOf(co), strings.Repeat("x", maxSkillBytes/2-1))
	if _, err := SandboxFiles(co); err != nil {
		t.Errorf("within the budget: %v", err)
	}

	// Dot and underscore names are what go:embed leaves out.
	co = makeCheckout(t, ModulePath)
	writeFile(t, skillOf(co), "s\n")
	writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "one", "_draft.md"), "d\n")
	writeFile(t, filepath.Join(co, assets.SandboxSkillsPath, "_wip", "SKILL.md"), "d\n")
	if got, err := SandboxFiles(co); err != nil || len(got) != 2 {
		t.Errorf("underscore names: %v, %v", got, err)
	}

	// A hard link is not plain.
	co = makeCheckout(t, ModulePath)
	writeFile(t, skillOf(co), "s\n")
	if err := os.Link(skillOf(co), filepath.Join(co, assets.SandboxSkillsPath, "one", "again.md")); err != nil {
		t.Skip(err)
	}
	if _, err := SandboxFiles(co); err == nil {
		t.Error("a hard link was used")
	}
}

// A checkout's CLAUDE.md that is there but unusable is an error, never a
// mix of the checkout's skills and the embedded CLAUDE.md.
func TestSandboxFilesBadClaudeMD(t *testing.T) {
	co := makeCheckout(t, ModulePath)
	writeFile(t, skillOf(co), "s\n")
	p := filepath.Join(co, assets.SandboxInstructionsPath)
	os.Remove(p)
	if err := os.Symlink(skillOf(co), p); err != nil {
		t.Fatal(err)
	}
	if _, err := SandboxFiles(co); err == nil {
		t.Error("a symlinked CLAUDE.md was used")
	}
	os.Remove(p)
	if got, err := SandboxFiles(co); err != nil || len(got[".claude/skills/one/SKILL.md"]) == 0 || len(got["CLAUDE.md"]) == 0 {
		t.Errorf("an absent CLAUDE.md: %v", err)
	}
}
