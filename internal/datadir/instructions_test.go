package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
)

func factsIn(checkout string, roots ...config.Root) Facts {
	return Facts{Env: config.DefaultEnv, Isolation: "container", Image: "apko", Checkout: checkout, Roots: roots}
}

func TestSourceKinds(t *testing.T) {
	one := []config.Root{{Host: "/h/dev", Container: "/work"}}
	several := []config.Root{{Name: "a", Host: "/a", Container: "/work/a"}, {Name: "dev", Host: "/h/dev", Container: "/work/dev"}}
	for _, tc := range []struct {
		name, checkout string
		roots          []config.Root
		kind, source   string
	}{
		{"inside", "/h/dev/caboose", one, SourceCheckout, "/work/caboose"},
		{"at root", "/h/dev", one, SourceCheckout, "/work"},
		{"under a root of /", "/h/c", []config.Root{{Host: "/", Container: "/work"}}, SourceCheckout, "/work/h/c"},
		{"named root", "/h/dev/caboose", several, SourceCheckout, "/work/dev/caboose"},
		{"prefix is not inside", "/h/devx/caboose", one, SourceOutside, "/h/devx/caboose"},
		{"release", "", one, SourceRelease, UpstreamURL},
	} {
		f := factsIn(tc.checkout, tc.roots...).Resolve()
		if f.sourceKind != tc.kind || f.source != tc.source {
			t.Errorf("%s: %q %q, want %q %q", tc.name, f.sourceKind, f.source, tc.kind, tc.source)
		}
	}
	// A clone under a root, with no checkout of the launcher's own.
	root := t.TempDir()
	clone := filepath.Join(root, "src", "caboose")
	for _, d := range []string{".git", ""} {
		if err := os.MkdirAll(filepath.Join(clone, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(clone, "go.mod"), []byte("module "+Module+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := factsIn("", config.Root{Host: root, Container: "/work"}).Resolve()
	if f.sourceKind != SourceClone || f.source != "/work/src/caboose" {
		t.Errorf("clone: %q %q", f.sourceKind, f.source)
	}
}

func TestExpandPlaceholders(t *testing.T) {
	f := Facts{Env: "work", Isolation: "gvisor", Profile: "gvisor.main", ImageProfile: "apko.default", Version: "v1.2",
		Roots: []config.Root{{Name: "a", Host: "/a", Container: "/work/a"}, {Name: "dev", Host: "/h/dev", Container: "/work/dev"}}}
	got, err := Expand([]byte("@@CABOOSE_ROOTS@@|@@CABOOSE_ENV@@|`caboose@@CABOOSE_ENV_FLAG@@ apply`|@@CABOOSE_ISOLATION@@|@@CABOOSE_PROFILE@@|@@CABOOSE_IMAGE@@|@@CABOOSE_VERSION@@|@@CABOOSE_SOURCE@@|@@CABOOSE_UPSTREAM@@ & \\1\n"), f)
	if err != nil {
		t.Fatal(err)
	}
	want := "`/a` at `/work/a`, `/h/dev` at `/work/dev`|work|`caboose -e work apply`|gvisor|gvisor.main|apko.default|v1.2|" +
		UpstreamURL + "|" + UpstreamURL + " & \\1\n"
	if string(got) != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	f.Env = ""
	got, _ = Expand([]byte("@@CABOOSE_ENV@@ `caboose@@CABOOSE_ENV_FLAG@@ apply`\n"), f)
	if string(got) != "default `caboose apply`\n" {
		t.Errorf("default env: %q", got)
	}
}

func TestExpandConditionals(t *testing.T) {
	src := "a\n" +
		"@@IF isolation=gvisor,vm@@\n" +
		"  not container\n" +
		"  @@IF image=ref@@\n" +
		"ref\n" +
		"@@ELSE@@\n" +
		"not ref\n" +
		"@@END@@\n" +
		"@@ELSE@@\n" +
		"container\n" +
		"@@END@@\n" +
		"@@IF env!=default@@\n" +
		"named @@CABOOSE_ENV@@\n" +
		"@@END@@\n" +
		"  @@IF hostexec=on@@  \n" +
		"hx\n" +
		"@@END@@\n" +
		"z\n"
	for _, tc := range []struct {
		f    Facts
		want string
	}{
		{Facts{Isolation: "container", Image: "apko"}, "a\ncontainer\nz\n"},
		{Facts{Isolation: "vm", Image: "ref", Env: "x", HostExec: true}, "a\n  not container\nref\nnamed x\nhx\nz\n"},
		{Facts{Isolation: "gvisor", Image: "apko", Env: "default"}, "a\n  not container\nnot ref\nz\n"},
	} {
		got, err := Expand([]byte(src), tc.f)
		if err != nil || string(got) != tc.want {
			t.Errorf("%+v: %q %v, want %q", tc.f, got, err, tc.want)
		}
	}
	// A removed block does not leave a gap.
	got, _ := Expand([]byte("a\n\n@@IF source=release@@\nx\n@@END@@\n\nb\n"), Facts{Checkout: "/c", Roots: []config.Root{{Host: "/c", Container: "/work"}}})
	if string(got) != "a\n\nb\n" {
		t.Errorf("gap: %q", got)
	}
	// Blank runs the author wrote stay, wherever no block was removed.
	got, _ = Expand([]byte("a\n\n\n\nb\n```\n\n\nx\n```\n"), Facts{})
	if string(got) != "a\n\n\n\nb\n```\n\n\nx\n```\n" {
		t.Errorf("author's blanks: %q", got)
	}
	got, _ = Expand([]byte("a\n\n@@IF isolation=vm@@\nx\n@@END@@\n\n\nb\n\n\nc\n"), Facts{})
	if string(got) != "a\n\nb\n\n\nc\n" {
		t.Errorf("collapse next to a removed block only: %q", got)
	}
	// Source conditions.
	got, _ = Expand([]byte("@@IF source=checkout,clone@@\nedit @@CABOOSE_SOURCE@@\n@@END@@\n"), Facts{Checkout: "/c", Roots: []config.Root{{Host: "/c", Container: "/work/c"}}})
	if string(got) != "edit /work/c\n" {
		t.Errorf("source: %q", got)
	}
}

func TestExpandErrors(t *testing.T) {
	for name, src := range map[string]string{
		"unknown key":    "@@IF color=red@@\nx\n@@END@@\n",
		"unknown value":  "@@IF isolation=docker@@\nx\n@@END@@\n",
		"empty value":    "@@IF isolation=vm,@@\nx\n@@END@@\n",
		"unbalanced IF":  "@@IF isolation=vm@@\nx\n",
		"stray END":      "x\n@@END@@\n",
		"stray ELSE":     "@@ELSE@@\nx\n",
		"two ELSE":       "@@IF isolation=vm@@\n@@ELSE@@\n@@ELSE@@\n@@END@@\n",
		"malformed":      "@@IF isolation@@\n@@END@@\n",
		"malformed 2":    "@@IFX isolation=vm@@\n@@END@@\n",
		"spaces":         "@@IF isolation = vm@@\n@@END@@\n",
		"hostexec value": "@@IF hostexec=maybe@@\n@@END@@\n",
		"unknown":        "@@CABOOSE_FROM_THE_FUTURE@@\n",
		"mid-line IF":    "x @@IF vm@@ y\n",
	} {
		if got, err := Expand([]byte(src), Facts{}); err == nil {
			t.Errorf("%s: expanded to %q", name, got)
		}
	}
}

func TestUnknownPlaceholders(t *testing.T) {
	got, err := Expand([]byte("@@CABOOSE_ROOTS@@ @@CABOOSE_ENV@@ a@@b @@x@@\n"), factsIn("/p", config.Root{Host: "/p", Container: "/work"}))
	if err != nil || UnknownPlaceholders(got) != nil {
		t.Errorf("known placeholders reported: %q %v", UnknownPlaceholders(got), err)
	}
	got = []byte("@@CABOOSE_NEW@@ and @@CABOOSE_NEW@@, @@OTHER_2@@ x @@IF mid-line@@ @@END@@")
	if want := []string{"@@CABOOSE_NEW@@", "@@OTHER_2@@", "@@IF mid-line@@", "@@END@@"}; !reflect.DeepEqual(UnknownPlaceholders(got), want) {
		t.Errorf("got %q, want %q", UnknownPlaceholders(got), want)
	}
	// The retired placeholders are unknown now.
	if u := UnknownPlaceholders([]byte("@@CABOOSE_DIR@@ @@CABOOSE_HOST_EXEC@@")); len(u) != 2 {
		t.Errorf("retired: %q", u)
	}
}

func TestExpandTree(t *testing.T) {
	f := factsIn("", config.Root{Host: "/r", Container: "/work"})
	out, err := ExpandTree(map[string][]byte{"CLAUDE.md": []byte("@@CABOOSE_ENV@@\n"), "a/data.txt": []byte("@@CABOOSE_ENV@@\n")}, f)
	if err != nil || string(out["CLAUDE.md"]) != "default\n" || string(out["a/data.txt"]) != "@@CABOOSE_ENV@@\n" {
		t.Errorf("%q %v", out, err)
	}
	_, err = ExpandTree(map[string][]byte{"x/SKILL.md": []byte("@@CABOOSE_FROM_THE_FUTURE@@\n")}, f)
	if err == nil || !strings.Contains(err.Error(), "x/SKILL.md") || !strings.Contains(err.Error(), "@@CABOOSE_FROM_THE_FUTURE@@") {
		t.Errorf("unknown placeholder: %v", err)
	}
	if _, err = ExpandTree(map[string][]byte{"x/SKILL.md": []byte("@@END@@\n")}, f); err == nil {
		t.Error("a bad directive expanded")
	}
}

const skill = ".claude/skills/one/SKILL.md"

// caboose's files go into the managed dir, never into ~/.claude: the
// sandbox's ~/.claude/CLAUDE.md is the user's.
func TestInstallManaged(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"CLAUDE.md": []byte("claude\n"), skill: []byte("one\n"), ".claude/skills/two/SKILL.md": []byte("two\n"),
		".claude/skills/two/ref/notes.md": []byte("notes\n")}
	changed, err := InstallManaged(files, dir)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for name, want := range files {
		if got := read(t, filepath.Join(dir, ManagedDir, filepath.FromSlash(name))); got != string(want) {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, ClaudeDir)); err == nil {
		t.Error("installing made ~/.claude, which is the user's")
	}

	// Unchanged: not written at all.
	old := time.Unix(1_000_000, 0)
	dst := filepath.Join(dir, ManagedDir, skill)
	if err := os.Chtimes(dst, old, old); err != nil {
		t.Fatal(err)
	}
	if changed, err = InstallManaged(files, dir); err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	if fi, _ := os.Stat(dst); !fi.ModTime().Equal(old) {
		t.Error("an unchanged file was rewritten")
	}

	// Changed: rewritten in place, and only that one.
	ino := inode(t, dst)
	files[skill] = []byte("one, edited\n")
	if changed, err = InstallManaged(files, dir); err != nil || !changed {
		t.Fatalf("edit: changed=%v err=%v", changed, err)
	}
	if read(t, dst) != "one, edited\n" || inode(t, dst) != ino {
		t.Error("edit was not written in place")
	}

	// A skill that is gone, with its directories, goes; one that stays
	// keeps its own.
	delete(files, ".claude/skills/two/ref/notes.md")
	delete(files, skill)
	if changed, err = InstallManaged(files, dir); err != nil || !changed {
		t.Fatalf("prune: changed=%v err=%v", changed, err)
	}
	for _, gone := range []string{".claude/skills/one", ".claude/skills/two/ref"} {
		if _, err := os.Lstat(filepath.Join(dir, ManagedDir, gone)); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	if read(t, filepath.Join(dir, ManagedDir, ".claude/skills/two/SKILL.md")) != "two\n" {
		t.Error("a kept skill was lost")
	}
	if changed, err = InstallManaged(files, dir); err != nil || changed {
		t.Fatalf("after prune: changed=%v err=%v", changed, err)
	}
	// Nothing outside the skills is pruned.
	other := filepath.Join(dir, ManagedDir, "other.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManaged(files, dir); err != nil || read(t, other) != "x" {
		t.Errorf("other.txt: %v", err)
	}
}

// A user's own ~/.claude/CLAUDE.md is left as it is.
func TestInstallManagedLeavesTheUsers(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureLayout(dir, defKeep()); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, ClaudeDir, "CLAUDE.md")
	if err := os.WriteFile(mine, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManaged(map[string][]byte{"CLAUDE.md": []byte("caboose's\n")}, dir); err != nil {
		t.Fatal(err)
	}
	if read(t, mine) != "mine\n" || read(t, filepath.Join(dir, ManagedInstructions)) != "caboose's\n" {
		t.Errorf("user's %q, caboose's %q", read(t, mine), read(t, filepath.Join(dir, ManagedInstructions)))
	}
}

// What the container can plant in the managed dir is never followed, nor
// removed through: a link in the skills tree is removed itself, and one
// where a file or directory goes is an error.
func TestInstallManagedSkillsNeverFollowLinks(t *testing.T) {
	host := hostFile(t, "host file\n")
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "SKILL.md"), []byte("host skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"CLAUDE.md": []byte("c\n"), skill: []byte("one\n")}

	// A link where a skill file goes.
	dir := t.TempDir()
	skills := filepath.Join(dir, ManagedDir, ".claude", "skills", "one")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(skills, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManaged(files, dir); err != nil {
		t.Errorf("link as file: %v", err)
	}
	if read(t, host) != "host file\n" || read(t, filepath.Join(skills, "SKILL.md")) != "one\n" {
		t.Error("the link was written through, or not replaced")
	}

	// A link where a skill's directory goes.
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ManagedDir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostDir, filepath.Join(dir, ManagedDir, ".claude", "skills", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManaged(files, dir); err != nil {
		t.Errorf("link as dir: %v", err)
	}
	if read(t, filepath.Join(hostDir, "SKILL.md")) != "host skill\n" {
		t.Error("a file was written through the linked directory")
	}
	if read(t, filepath.Join(dir, ManagedDir, skill)) != "one\n" {
		t.Error("the link was not replaced")
	}

	// A hard link where a skill file goes is not replaced, nor written.
	dir = t.TempDir()
	skills = filepath.Join(dir, ManagedDir, ".claude", "skills", "one")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(host, filepath.Join(skills, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManaged(files, dir); !errors.Is(err, nofollow.ErrNotPlain) {
		t.Errorf("hard link: %v, want ErrNotPlain", err)
	}
	if read(t, host) != "host file\n" {
		t.Error("the hard-linked file was written through")
	}

	// A link where .claude goes.
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ManagedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostDir, filepath.Join(dir, ManagedDir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallManaged(files, dir); !errors.Is(err, nofollow.ErrNotPlain) {
		t.Errorf("link as .claude: %v, want ErrNotPlain", err)
	}

	// Stale links and a stale linked directory are removed, not followed.
	dir = t.TempDir()
	stale := filepath.Join(dir, ManagedDir, ".claude", "skills", "old")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostDir, filepath.Join(stale, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(stale, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if changed, err := InstallManaged(files, dir); err != nil || !changed {
		t.Fatalf("stale links: %v %v", changed, err)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Error("the stale skill is still there")
	}
	if read(t, host) != "host file\n" || read(t, filepath.Join(hostDir, "SKILL.md")) != "host skill\n" {
		t.Error("pruning reached through a link")
	}
}

// What the values hold is never read as a placeholder: a clone the sandbox
// named @@X@@, or a root path with @@ in it, expands fine.
func TestExpandValuesAreNotPlaceholders(t *testing.T) {
	f := Facts{Roots: []config.Root{{Host: "/h/@@ROOT@@", Container: "/work"}}, Version: "@@V@@", Env: "e@@x@@"}
	got, err := Expand([]byte("@@CABOOSE_ROOTS@@ @@CABOOSE_VERSION@@ @@CABOOSE_ENV@@\n"), f)
	if err != nil || string(got) != "`/h/@@ROOT@@` at `/work` @@V@@ e@@x@@\n" {
		t.Errorf("%q %v", got, err)
	}
}

// A clone whose name the sandbox chose is not one that goes into the
// instructions: @@, a backtick or a control character skip it, and a clean
// one is still found.
func TestFindCloneSkipsUnfitNames(t *testing.T) {
	for _, bad := range []string{"@@X@@", "a`b", "a\nb", "a\x1bb", "a\u202eb"} {
		root := t.TempDir()
		mk := func(name string) string {
			dir := filepath.Join(root, name, "caboose")
			if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
				t.Skip("name not usable here:", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+Module+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return dir
		}
		mk(bad)
		roots := []config.Root{{Host: root, Container: "/work"}}
		if got := FindClone(roots); got != "" {
			t.Errorf("%q: found %q", bad, got)
		}
		if f := (Facts{Roots: roots}).Resolve(); f.sourceKind != SourceRelease {
			t.Errorf("%q: source %q", bad, f.sourceKind)
		}
		clean := mk("zz")
		if got := FindClone(roots); got != clean {
			t.Errorf("%q: found %q, want %q", bad, got, clean)
		}
	}
}

// A file where a directory goes, and a directory where a file does, in
// either direction: the managed dir is caboose's, so one is replaced.
func TestInstallManagedSwaps(t *testing.T) {
	dir := t.TempDir()
	asFile := map[string][]byte{".claude/skills/x": []byte("file\n")}
	asDir := map[string][]byte{".claude/skills/x/SKILL.md": []byte("dir\n"), ".claude/skills/x/sub/y.md": []byte("y\n")}
	for i, files := range []map[string][]byte{asFile, asDir, asFile, asDir} {
		if changed, err := InstallManaged(files, dir); err != nil || !changed {
			t.Fatalf("step %d: %v %v", i, changed, err)
		}
		for name, want := range files {
			if got := read(t, filepath.Join(dir, ManagedDir, filepath.FromSlash(name))); got != string(want) {
				t.Errorf("step %d, %s: %q", i, name, got)
			}
		}
	}
	if _, err := InstallManaged(asFile, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ManagedDir, ".claude/skills/x/SKILL.md")); err == nil {
		t.Error("the old directory's files are still there")
	}
}
