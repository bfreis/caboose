package datadir

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
)

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Ino)
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteInPlaceKeepsInode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("a much longer original content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := inode(t, p)
	if err := WriteInPlace(p, []byte("short\n")); err != nil {
		t.Fatal(err)
	}
	if inode(t, p) != before {
		t.Error("inode changed")
	}
	if got := read(t, p); got != "short\n" {
		t.Errorf("content = %q (not truncated?)", got)
	}
}

func TestEnsureLayout(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(keep, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ino := inode(t, keep)
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	for _, d := range Dirs {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}
	for _, f := range append(append([]string(nil), Files...), Created...) {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s is not a regular file", f)
		}
	}
	if read(t, keep) != "{}\n" || inode(t, keep) != ino {
		t.Error("an existing file was replaced or truncated")
	}
	// The configs tools rewrite must be reachable through directory mounts;
	// a single-file mount of them is what broke `gh auth login`.
	for _, f := range Files {
		if strings.Contains(f, "gitconfig") || strings.Contains(f, "jjconfig") || strings.HasPrefix(f, "dot_config/") {
			t.Errorf("%s is a single-file mount; tools save it by rename, which a file mount refuses", f)
		}
	}
	for _, f := range []string{".gitconfig", ".jjconfig.toml"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was created; git and jj config live under dot_config", f)
		}
	}
	// No git config means git would write ~/.gitconfig instead, outside
	// every mount. jj creates its own, so an empty one is not planted.
	if _, err := os.Stat(filepath.Join(dir, JJConfig)); err == nil {
		t.Error("an empty jj config.toml was created")
	}
}

// .claude.json starts as valid JSON: an empty one fails the first `claude
// install`. An empty one is seeded in place, keeping the inode a running
// container's file mount is pinned to; one with content is never touched,
// even if it does not parse.
func TestEnsureLayoutSeedsClaudeJSON(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".claude.json")
	if got := read(t, p); got != "{}\n" {
		t.Errorf("new .claude.json = %q, want %q", got, "{}\n")
	}

	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	ino := inode(t, p)
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "{}\n" {
		t.Errorf("empty .claude.json = %q after EnsureLayout, want %q", got, "{}\n")
	}
	if inode(t, p) != ino {
		t.Error("seeding replaced the file rather than writing it in place")
	}

	if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "{" {
		t.Errorf("a .claude.json with content was rewritten to %q", got)
	}
}

func TestGhDirIsPrivate(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	dir := t.TempDir()
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(dir, PrivateDir)
	if m := perm(t, gh); m != 0o700 {
		t.Errorf("%s: mode %v, want 0700", PrivateDir, m)
	}
	// Its parent is an ordinary dir, not collateral 0700.
	if m := perm(t, filepath.Dir(gh)); m != 0o775 {
		t.Errorf("dot_config: mode %v, want 0775", m)
	}
	// An existing one is left alone, like everything else here.
	if err := os.Chmod(gh, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, gh); m != 0o750 {
		t.Errorf("an existing gh dir was re-moded to %v", m)
	}
}

func TestInstructionsLocation(t *testing.T) {
	one := []config.Root{{Host: "/h/dev", Container: "/work"}}
	if got := InstructionsLocation("/h/dev/caboose", one); got != "/work/caboose" {
		t.Errorf("inside: %q", got)
	}
	if got := InstructionsLocation("/h/dev", one); got != "/work" {
		t.Errorf("at root: %q", got)
	}
	if got := InstructionsLocation("/h/c", []config.Root{{Host: "/", Container: "/work"}}); got != "/work/h/c" {
		t.Errorf("under a root of /: %q", got)
	}
	several := []config.Root{{Name: "a", Host: "/a", Container: "/work/a"}, {Name: "dev", Host: "/h/dev", Container: "/work/dev"}}
	if got := InstructionsLocation("/h/dev/caboose", several); got != "/work/dev/caboose" {
		t.Errorf("under a named root: %q", got)
	}
	if got, want := InstructionsLocation("/h/devx/caboose", one),
		"NOT MOUNTED — its checkout, /h/devx/caboose, is outside the repo root (/h/dev), "+
			"so it is edited from the host, not from in here"; got != want {
		t.Errorf("outside: %q", got)
	}
	if got, want := InstructionsLocation("", one),
		"NOT MOUNTED — this launcher is an installed binary, not a checkout. The source is "+
			"https://github.com/bfreis/caboose: changes are made there (or in a clone of it on the host), "+
			"not from in here"; got != want {
		t.Errorf("no checkout: %q", got)
	}
}

func TestInstallInstructions(t *testing.T) {
	dir := t.TempDir()
	src := []byte("see @@CABOOSE_DIR@@ and @@CABOOSE_DIR@@/x | & \\1 in @@CABOOSE_ROOTS@@\n")
	r := []config.Root{{Host: "/r", Container: "/work"}}
	changed, err := InstallInstructions(src, "/r/a|b&c", r, dir)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	dst := filepath.Join(dir, ".claude", "CLAUDE.md")
	if got, want := read(t, dst), "see /work/a|b&c and /work/a|b&c/x | & \\1 in `/r` at `/work`\n"; got != want {
		t.Errorf("installed %q, want %q", got, want)
	}

	// Unchanged: not written at all.
	old := time.Unix(1_000_000, 0)
	if err := os.Chtimes(dst, old, old); err != nil {
		t.Fatal(err)
	}
	ino := inode(t, dst)
	changed, err = InstallInstructions(src, "/r/a|b&c", r, dir)
	if err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	if fi, _ := os.Stat(dst); !fi.ModTime().Equal(old) {
		t.Error("an unchanged file was rewritten")
	}

	// Changed: rewritten in place.
	changed, err = InstallInstructions([]byte("new @@CABOOSE_DIR@@\n"), "/p/q", []config.Root{{Host: "/p", Container: "/work"}}, dir)
	if err != nil || !changed {
		t.Fatalf("edit: changed=%v err=%v", changed, err)
	}
	if read(t, dst) != "new /work/q\n" || inode(t, dst) != ino {
		t.Error("edit was not written in place")
	}
}

// fakeGit keeps config files as key=value lines, which is all the logic
// here needs to observe.
type fakeGit struct {
	global map[string]string
	sets   int
}

func (g *fakeGit) Get(path, key string) string {
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && k == key {
			return v
		}
	}
	return ""
}
func (g *fakeGit) GetGlobal(key string) string { return g.global[key] }
func (g *fakeGit) Set(path, key, value string) error {
	return g.rewrite(path, func(lines []string) []string {
		return append(g.without(lines, key), key+"="+value)
	})
}
func (g *fakeGit) Unset(path, key string) error {
	return g.rewrite(path, func(lines []string) []string { return g.without(lines, key) })
}

func (g *fakeGit) without(lines []string, key string) []string {
	var out []string
	for _, l := range lines {
		if k, _, _ := strings.Cut(l, "="); k != key {
			out = append(out, l)
		}
	}
	return out
}

// rewrite saves like real git: replace by rename, carrying the file's mode
// across.
func (g *fakeGit) rewrite(path string, edit func([]string) []string) error {
	g.sets++
	b, _ := os.ReadFile(path)
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	out := ""
	for _, l := range edit(lines) {
		out += l + "\n"
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	nw := path + ".lock"
	if err := os.WriteFile(nw, []byte(out), mode); err != nil {
		return err
	}
	if err := os.Chmod(nw, mode); err != nil {
		return err
	}
	return os.Rename(nw, path)
}

func TestWriteSandboxGit(t *testing.T) {
	t.Run("no git reads nothing", func(t *testing.T) {
		if s, err := ReadSandboxGit(t.TempDir(), nil); s != (SandboxGit{}) || err != nil {
			t.Errorf("ReadSandboxGit = %+v, %v", s, err)
		}
	})
	t.Run("sets and unsets", func(t *testing.T) {
		dir := t.TempDir()
		dst := mkGitConfig(t, dir)
		if err := os.WriteFile(dst, []byte("user.email=old@sandbox\nuser.signingkey=ssh-ed25519 AAAA\ncore.editor=vi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		g := &fakeGit{}
		err := WriteSandboxGit(dir, g, []Change{
			{Key: "user.email", Value: "new@sandbox"},
			{Key: "user.name", Value: "Me"},
			{Key: "user.signingkey", Unset: true},
			{Key: "commit.gpgsign", Unset: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, dst); got != "core.editor=vi\nuser.email=new@sandbox\nuser.name=Me\n" {
			t.Errorf("git config = %q", got)
		}
	})
	t.Run("nothing to change writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		g := &fakeGit{}
		if err := WriteSandboxGit(dir, g, nil); err != nil || g.sets != 0 {
			t.Errorf("err=%v sets=%d", err, g.sets)
		}
		if _, err := os.Lstat(filepath.Join(dir, GitConfig)); err == nil {
			t.Error("a config was created")
		}
	})
	t.Run("creates the XDG file, never ~/.gitconfig", func(t *testing.T) {
		dir := t.TempDir()
		g := &fakeGit{}
		if err := WriteSandboxGit(dir, g, []Change{{Key: "user.name", Value: "Me"}}); err != nil {
			t.Fatal(err)
		}
		if got := read(t, filepath.Join(dir, GitConfig)); got != "user.name=Me\n" {
			t.Errorf("git config = %q", got)
		}
		if _, err := os.Lstat(filepath.Join(dir, ".gitconfig")); err == nil {
			t.Error("created .gitconfig")
		}
	})
}

func TestHostIdentity(t *testing.T) {
	g := &fakeGit{global: map[string]string{"user.name": "Me", "user.email": "me@example.invalid"}}
	if id := HostIdentity(g); id != (Identity{"Me", "me@example.invalid"}) || !id.Complete() {
		t.Errorf("HostIdentity = %+v", id)
	}
	if id := HostIdentity(nil); id != (Identity{}) {
		t.Errorf("HostIdentity(nil) = %+v", id)
	}
	if (Identity{Name: "Me"}).Complete() || (Identity{Email: "x"}).Complete() {
		t.Error("half an identity is complete")
	}
}

// mkGitConfig creates the dir the sandbox's git config lives in and returns
// the config's path.
func mkGitConfig(t *testing.T, dir string) string {
	t.Helper()
	dst := filepath.Join(dir, GitConfig)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

// The same, end to end against the real git, whose own saves rename; an
// unset of a key that is not there (git's exit 5) is no error.
func TestWriteSandboxGitRealGit(t *testing.T) {
	g := FindGit()
	if g == nil {
		t.Skip("no git on PATH")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	err := WriteSandboxGit(dir, g, []Change{
		{Key: "user.email", Value: "me@example.invalid"},
		{Key: "commit.gpgsign", Unset: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSandboxGit(dir, g, []Change{{Key: "user.name", Value: "Me"}}); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSandboxGit(dir, g)
	if err != nil || s.Identity != (Identity{"Me", "me@example.invalid"}) {
		t.Errorf("ReadSandboxGit = %+v, %v", s, err)
	}
	if err := WriteSandboxGit(dir, g, []Change{{Key: "user.name", Unset: true}}); err != nil {
		t.Fatal(err)
	}
	if got := g.Get(filepath.Join(dir, GitConfig), "user.name"); got != "" {
		t.Errorf("user.name still %q", got)
	}
}

// What the container does with the result: with ~/.config/git/config present
// and no ~/.gitconfig, `git config --global` reads and writes the XDG file.
// Were it missing, git would create ~/.gitconfig, outside every mount.
func TestRealGitUsesXDGConfig(t *testing.T) {
	if FindGit() == nil {
		t.Skip("no git on PATH")
	}
	data := t.TempDir()
	if err := EnsureLayout(data); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stands in for the directory bind mount.
	if err := os.Symlink(filepath.Join(data, "dot_config/git"), filepath.Join(home, ".config/git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Unsetenv("XDG_CONFIG_HOME")
	os.Unsetenv("GIT_CONFIG_GLOBAL")
	if out, err := exec.Command("git", "config", "--global", "caboose.probe", "yes").CombinedOutput(); err != nil {
		t.Fatalf("git config --global: %v: %s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".gitconfig")); err == nil {
		t.Error("git created ~/.gitconfig instead of using ~/.config/git/config")
	}
	if got := (ExecGit{Path: "git"}).Get(filepath.Join(data, GitConfig), "caboose.probe"); got != "yes" {
		t.Errorf("the write did not land in the data dir: %q", got)
	}
}

// A read-only file already in the data dir is fine: touch(1) never opens an
// existing file for writing, and neither may EnsureLayout.
func TestEnsureLayoutReadOnlyFiles(t *testing.T) {
	dir := t.TempDir()
	files := append(append([]string(nil), Files...), Created...)
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnsureLayout(dir); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	for _, f := range files {
		if read(t, filepath.Join(dir, f)) != "x\n" {
			t.Errorf("%s changed", f)
		}
	}
}

// A read-only git config is only ever read.
func TestReadSandboxGitReadOnly(t *testing.T) {
	dir := t.TempDir()
	dst := mkGitConfig(t, dir)
	if err := os.WriteFile(dst, []byte("user.email=a\nuser.name=b\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSandboxGit(dir, &fakeGit{})
	if err != nil || s.Identity != (Identity{"b", "a"}) {
		t.Errorf("ReadSandboxGit = %+v, %v", s, err)
	}
	if read(t, dst) != "user.email=a\nuser.name=b\n" {
		t.Error("git config changed")
	}
}

// Modes come from the umask, as with mkdir -p, touch and `cat >`.
func TestCreatedModesFollowUmask(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), "data")
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteSandboxGit(dir, &fakeGit{}, []Change{{Key: "user.name", Value: "Me"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallInstructions([]byte("x"), "", []config.Root{{Host: "/r", Container: "/work"}}, dir); err != nil {
		t.Fatal(err)
	}
	for _, d := range append([]string{".", "dot_config"}, Dirs...) {
		if isPrivate(d) {
			continue // 0700 regardless; see TestGhDirIsPrivate
		}
		if m := perm(t, filepath.Join(dir, d)); m != 0o775 {
			t.Errorf("%s: mode %v, want 0775", d, m)
		}
	}
	for _, f := range append(append([]string{".claude/CLAUDE.md"}, Files...), Created...) {
		if m := perm(t, filepath.Join(dir, f)); m != 0o664 {
			t.Errorf("%s: mode %v, want 0664", f, m)
		}
	}
	// A file created by WriteInPlace alone gets the same.
	p := filepath.Join(dir, "new")
	if err := WriteInPlace(p, nil); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, p); m != 0o664 {
		t.Errorf("WriteInPlace mode %v, want 0664", m)
	}
}

func TestUnknownPlaceholders(t *testing.T) {
	if got := UnknownPlaceholders(ExpandInstructions([]byte("@@CABOOSE_DIR@@ @@CABOOSE_ROOTS@@ a@@b @@x@@\n"), "/p", []config.Root{{Host: "/p", Container: "/work"}})); got != nil {
		t.Errorf("known placeholders reported: %q", got)
	}
	got := UnknownPlaceholders([]byte("@@CABOOSE_NEW@@ and @@CABOOSE_NEW@@, @@OTHER_2@@"))
	if want := []string{"@@CABOOSE_NEW@@", "@@OTHER_2@@"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Every root is listed with where the container has it.
func TestExpandInstructionsRoots(t *testing.T) {
	src := []byte("mounted: @@CABOOSE_ROOTS@@.\n")
	for want, roots := range map[string][]config.Root{
		"mounted: `/` at `/work`.\n":      {{Host: "/", Container: "/work"}},
		"mounted: `/h/dev` at `/work`.\n": {{Host: "/h/dev", Container: "/work"}},
		"mounted: `/a` at `/work/a`, `/h/dev` at `/work/dev`.\n": {
			{Name: "a", Host: "/a", Container: "/work/a"}, {Name: "dev", Host: "/h/dev", Container: "/work/dev"}},
	} {
		if got := string(ExpandInstructions(src, "", roots)); got != want {
			t.Errorf("%+v: %q, want %q", roots, got, want)
		}
	}
}

func TestLoggedIn(t *testing.T) {
	dir := t.TempDir()
	if in, err := LoggedIn(dir); in || err != nil {
		t.Errorf("no .claude: %v %v", in, err)
	}
	p := filepath.Join(dir, Credentials)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if in, err := LoggedIn(dir); in || err != nil {
		t.Errorf("empty: %v %v", in, err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if in, err := LoggedIn(dir); !in || err != nil {
		t.Errorf("there: %v %v", in, err)
	}
	// A link in its place names something else.
	target := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p, p+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if in, err := LoggedIn(dir); in || err == nil {
		t.Errorf("a symlink: %v %v", in, err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".real", p); err != nil {
		t.Fatal(err)
	}
	// Reached through a link, or a link itself: not a login.
	other := t.TempDir()
	if err := os.Rename(filepath.Join(dir, ".claude"), filepath.Join(other, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, ".claude"), filepath.Join(dir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if in, err := LoggedIn(dir); in || err == nil {
		t.Errorf("through a linked .claude: %v %v", in, err)
	}
}

// The sandbox's ~/.ssh: ssh refuses one others can write, so 0700 whatever
// the umask, and left alone once it exists.
func TestSSHDirIsPrivate(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	dir := t.TempDir()
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, filepath.Join(dir, SSHDir)); m != 0o700 {
		t.Errorf("%s: mode %v, want 0700", SSHDir, m)
	}
}

func TestEnsurePersist(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	dir := t.TempDir()
	aws, _ := config.ParsePersist("aws", "~/.aws")
	foo, _ := config.ParsePersist("foo", "~/.config/foo")
	if err := os.MkdirAll(filepath.Join(dir, PersistDir("foo")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, PersistDir("foo"), "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePersist(dir, []config.Persist{aws, foo}); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, filepath.Join(dir, PersistDir("aws"))); m != 0o700 {
		t.Errorf("new: mode %v, want 0700", m)
	}
	// One that exists keeps its mode and what it holds.
	if m := perm(t, filepath.Join(dir, PersistDir("foo"))); m != 0o755 {
		t.Errorf("existing: mode %v, want it left at 0755", m)
	}
	if read(t, filepath.Join(dir, PersistDir("foo"), "keep")) != "x" {
		t.Error("an existing entry's contents changed")
	}
	if err := EnsurePersist(t.TempDir(), nil); err != nil {
		t.Errorf("no entries: %v", err)
	}
}

func TestPrivateKeysIn(t *testing.T) {
	dir := t.TempDir()
	if keys, err := PrivateKeysIn(dir); err != nil || keys != nil {
		t.Errorf("no ~/.ssh: %v, %v", keys, err)
	}
	ssh := filepath.Join(dir, SSHDir)
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"id_ed25519":     "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbg==\n-----END OPENSSH PRIVATE KEY-----\n",
		"id_rsa":         "-----BEGIN RSA PRIVATE KEY-----\nMII=\n",
		"work.pem":       "\n-----BEGIN ENCRYPTED PRIVATE KEY-----\nMII=\n",
		"id_ed25519.pub": "ssh-ed25519 AAAAC3Nza me@example.invalid\n",
		"known_hosts":    "github.com ssh-ed25519 AAAAC3Nza\n",
		"config":         "Host box\n  User me\n",
	} {
		if err := os.WriteFile(filepath.Join(ssh, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A link is never followed, even to a key: the container writes here.
	outside := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(outside, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ssh, "linked")); err != nil {
		t.Fatal(err)
	}
	keys, err := PrivateKeysIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	if want := []string{"id_ed25519", "id_rsa", "work.pem"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
}
