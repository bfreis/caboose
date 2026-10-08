package apkobuild

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pkglock "chainguard.dev/apko/pkg/lock"
)

func TestGroups(t *testing.T) {
	gs := Groups()
	if len(gs) == 0 {
		t.Fatal("no groups")
	}
	names := map[string]bool{}
	pkgs := map[string]string{}
	required := 0
	for _, g := range gs {
		if g.Name == "" || g.Title == "" || len(g.Packages) == 0 {
			t.Errorf("group %+v is incomplete", g)
		}
		if names[g.Name] {
			t.Errorf("group %q appears twice", g.Name)
		}
		names[g.Name] = true
		if g.Required {
			required++
			if g.Default {
				t.Errorf("group %q is both required and default", g.Name)
			}
		}
		for _, p := range g.Packages {
			if err := CheckName(p); err != nil {
				t.Errorf("group %q: %v", g.Name, err)
			}
			if prev, ok := pkgs[p]; ok {
				t.Errorf("package %q is in groups %q and %q", p, prev, g.Name)
			}
			pkgs[p] = g.Name
		}
	}
	if required != 1 {
		t.Errorf("%d required groups, want 1", required)
	}
	if gs[0].Name != "required" {
		t.Errorf("first group is %q, want required", gs[0].Name)
	}
	if !slices.Contains(Required(), "bash") {
		t.Error("required lacks bash")
	}
	def := DefaultPackages()
	for _, p := range Required() {
		if !slices.Contains(def, p) {
			t.Errorf("defaults lack required package %q", p)
		}
	}
	if !slices.Contains(def, "go-1.26") || slices.Contains(def, "rustup") {
		t.Error("defaults should hold go and not rustup")
	}
}

func TestGroupsAreCopies(t *testing.T) {
	Groups()[0].Packages[0] = "changed"
	if Required()[0] == "changed" {
		t.Error("Groups exposes its own storage")
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"a", "bash", "python-3.13", "libstdc++", "a_b", "9p", strings.Repeat("a", 128)} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", ".a", "_a", "+a", "A", "a b", "a/b", "a=1", "a\x00", "é", strings.Repeat("a", 129)} {
		if err := CheckName(bad); err == nil {
			t.Errorf("CheckName(%q) passed", bad)
		}
	}
	if err := CheckName("Bad"); err == nil || !strings.Contains(err.Error(), `"Bad"`) {
		t.Errorf("error does not quote the name: %v", err)
	}
}

func TestSpecList(t *testing.T) {
	got, err := Spec{}.List()
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(Required())
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("empty spec = %v, want %v", got, want)
	}

	got, err = Spec{Packages: []string{"zzz", "bash", "aaa", "aaa"}}.List()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(got) || slices.Contains(got[1:], "aaa") || got[0] != "aaa" || !slices.Contains(got, "zzz") {
		t.Errorf("list = %v", got)
	}
	if n := len(got); n != len(want)+2 {
		t.Errorf("%d packages, want %d", n, len(want)+2)
	}

	all, err := Spec{Defaults: true, Packages: []string{"go-1.26"}}.List()
	if err != nil {
		t.Fatal(err)
	}
	d := slices.Clone(DefaultPackages())
	slices.Sort(d)
	if !slices.Equal(all, slices.Compact(d)) {
		t.Errorf("defaults list differs: %v", all)
	}

	if _, err := (Spec{Packages: []string{"Bad Name"}}).List(); err == nil {
		t.Error("a bad name passed")
	}
}

func TestArchFor(t *testing.T) {
	for in, want := range map[string]string{"arm64": "aarch64", "amd64": "x86_64"} {
		if got, err := ArchFor(in); err != nil || got != want {
			t.Errorf("ArchFor(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ArchFor("riscv64"); err == nil {
		t.Error("riscv64 passed")
	}
}

func TestOptionsArch(t *testing.T) {
	for _, a := range []string{"", "arm64", "ppc64le"} {
		if err := (Options{Arch: a}).check(); err == nil {
			t.Errorf("arch %q passed", a)
		}
	}
	for _, a := range []string{"aarch64", "x86_64"} {
		if err := (Options{Arch: a}).check(); err != nil {
			t.Errorf("arch %q: %v", a, err)
		}
	}
}

func sampleLock() *Lock {
	return mustLock(pkglock.Lock{
		Version: "v1",
		Config:  &pkglock.Config{Name: ConfigName(Spec{Packages: []string{"busybox", "bash"}}, []string{"bash", "busybox"})},
		Contents: pkglock.LockContents{
			Repositories: []pkglock.LockRepo{{Name: "r", URL: "https://example.com/os/aarch64/APKINDEX.tar.gz", Architecture: "aarch64"}},
			Packages: []pkglock.LockPkg{
				{Name: "busybox", Version: "1.0-r0", Architecture: "aarch64", URL: "https://example.com/os/aarch64/busybox-1.0-r0.apk", Checksum: "Q1aaa", Data: pkglock.LockPkgRangeAndChecksum{Checksum: "sha256-x"}},
				{Name: "bash", Version: "5.2-r1", Architecture: "aarch64", URL: "https://example.com/os/aarch64/bash-5.2-r1.apk", Checksum: "Q1bbb"},
			},
		},
	})
}

func mustLock(l pkglock.Lock) *Lock {
	lock, err := newLock(l)
	if err != nil {
		panic(err)
	}
	return lock
}

func TestLockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.json")
	l := sampleLock()
	if err := l.Write(path); err != nil {
		t.Fatal(err)
	}
	// Writing again over an existing file works and leaves no temp files.
	if err := l.Write(path); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("directory holds %d files, want only the lock", len(ents))
	}
	got, err := ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash() != l.Hash() {
		t.Error("hash changed over a round trip")
	}
	if !slices.Equal(got.Input(), []string{"bash", "busybox"}) {
		t.Errorf("input = %v", got.Input())
	}
	if s := got.Spec(); s.Defaults || !slices.Equal(s.Packages, []string{"bash", "busybox"}) {
		t.Errorf("spec = %+v", s)
	}
	if want := []PackageVersion{{"busybox", "1.0-r0"}, {"bash", "5.2-r1"}}; !slices.Equal(got.Packages(), want) {
		t.Errorf("packages = %v", got.Packages())
	}
	if got.Arch() != "aarch64" {
		t.Errorf("arch = %q", got.Arch())
	}
}

func TestWriteFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := sampleLock().Write(filepath.Join(dir, "missing", "lock.json")); err == nil {
		t.Error("write into a missing directory succeeded")
	}
	// The destination is a directory: the rename fails and the temp file goes.
	dest := filepath.Join(dir, "d")
	os.Mkdir(dest, 0o755)
	os.WriteFile(filepath.Join(dest, "x"), nil, 0o644)
	if err := sampleLock().Write(dest); err == nil {
		t.Error("write over a directory succeeded")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("directory holds %d entries, want only d", len(ents))
	}
}

func TestReadLockErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"junk":     "not json",
		"foreign":  `{"version":"v1","contents":{"packages":[{"name":"a"}]}}`,
		"nopkgs":   `{"version":"v1","config":{"name":"caboose: {\\"spec\\":{\\"packages\\":[],\\"defaults\\":true},\\"input\\":[\\"a\\"]}"},"contents":{"packages":[]}}`,
		"no spec":  `{"version":"v1","config":{"name":"caboose: a"},"contents":{"packages":[{"name":"a"}]}}`,
		"nil spec": `{"version":"v1","config":{"name":"caboose: {\\"input\\":[\\"a\\"]}"},"contents":{"packages":[{"name":"a"}]}}`,
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := ReadLock(p); err == nil {
			t.Errorf("%s: read succeeded", name)
		}
	}
	if _, err := ReadLock(filepath.Join(dir, "absent")); err == nil {
		t.Error("absent file read")
	}
}

// A lock records the spec it was resolved for, apart from its input, and
// WithSpec changes it only for a spec that stands for the same packages;
// the hash, which names what the lock builds, stays.
func TestLockSpec(t *testing.T) {
	l := sampleLock()
	same := Spec{Packages: []string{"bash", "busybox", "bash"}}
	if !l.Spec().Equal(same) || l.Spec().Equal(Spec{Packages: []string{"bash"}}) {
		t.Errorf("spec %+v", l.Spec())
	}
	input, err := Spec{Packages: []string{"busybox"}}.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.WithSpec(Spec{Packages: []string{"busybox"}}); err == nil {
		t.Error("WithSpec took a spec of other packages")
	}
	l2 := mustLock(pkglock.Lock{Version: "v1", Config: &pkglock.Config{Name: ConfigName(Spec{Packages: []string{"jq"}}, input)},
		Contents: sampleLock().l.Contents})
	moved, err := l2.WithSpec(Spec{Packages: []string{"busybox"}})
	if err != nil {
		t.Fatal(err)
	}
	if !moved.Spec().Equal(Spec{Packages: []string{"busybox"}}) || !l2.Spec().Equal(Spec{Packages: []string{"jq"}}) || moved.Hash() != l2.Hash() {
		t.Errorf("moved %+v, original %+v", moved.Spec(), l2.Spec())
	}
}

func TestHash(t *testing.T) {
	base := sampleLock().Hash()
	if len(base) != 64 || base != sampleLock().Hash() {
		t.Fatalf("hash %q is not stable", base)
	}
	mutations := map[string]func(*Lock){
		"version":  func(l *Lock) { l.l.Contents.Packages[0].Version = "1.1-r0" },
		"checksum": func(l *Lock) { l.l.Contents.Packages[1].Checksum = "Q1ccc" },
		"data":     func(l *Lock) { l.l.Contents.Packages[0].Data.Checksum = "sha256-y" },
		"input": func(l *Lock) {
			l.l.Config.Name = ConfigName(Spec{}, []string{"bash", "busybox", "curl"})
			*l = *mustLock(l.l)
		},
		"order": func(l *Lock) { p := l.l.Contents.Packages; p[0], p[1] = p[1], p[0] },
		"url":   func(l *Lock) { l.l.Contents.Packages[0].URL += "x" },
		"repo":  func(l *Lock) { l.l.Contents.Repositories[0].URL += "x" },
		"extra": func(l *Lock) {
			l.l.Contents.Packages = append(l.l.Contents.Packages, pkglock.LockPkg{Name: "z", Architecture: "aarch64"})
		},
	}
	for name, mut := range mutations {
		l := sampleLock()
		mut(l)
		if l.Hash() == base {
			t.Errorf("hash ignores %s", name)
		}
	}
}
