package proposal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const full = `title = "Install foo"
reason = """
foo builds the docs.
It keeps its config in ~/.config/foo."""
dockerfile_sha256 = "` + sha + `"

[section]
name = "foo"
title = "foo 2.3"
body = '''
ARG FOO_VERSION=2.3.0
RUN curl -fsSL https://example.invalid/foo.tgz | tar -xz -C /usr/local/bin foo
'''

[roots]
other = "~/src/other"
`

func TestParse(t *testing.T) {
	p, err := Parse("foo.toml", []byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "foo" || p.Title != "Install foo" || !strings.HasPrefix(p.Reason, "foo builds") || p.DockerfileSHA256 != sha {
		t.Errorf("%+v", p)
	}
	if p.Section == nil || p.Section.Name != "foo" || p.Section.Title != "foo 2.3" || !strings.Contains(p.Section.Body, "FOO_VERSION") {
		t.Errorf("section %+v", p.Section)
	}
	if p.Root == nil || *p.Root != (Root{"other", "~/src/other"}) {
		t.Errorf("root %+v", p.Root)
	}
	// Each part alone is a proposal too.
	for _, s := range []string{
		"title = \"t\"\n[roots]\nx = \"/x\"\n",
		"title = \"t\"\ndockerfile_sha256 = \"" + sha + "\"\n[section]\nname = \"a\"\ntitle = \"A\"\nbody = \"RUN x\"\n",
	} {
		if _, err := Parse("p.toml", []byte(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	sec := "dockerfile_sha256 = \"" + sha + "\"\n[section]\nname = \"a\"\ntitle = \"A\"\n"
	for _, tc := range []struct{ name, data, want string }{
		{"Foo.toml", "title = \"t\"\n[roots]\nx = \"/x\"\n", "not a proposal's name"},
		{"foo.txt", "", "not a proposal's name"},
		{"p.toml", "title = \"t\"\n", "proposes nothing"},
		{"p.toml", "[roots]\nx = \"/x\"\n", "no title"},
		{"p.toml", "title = \"t\"\ndocker_sock = true\n[roots]\nx = \"/x\"\n", "unknown key docker_sock"},
		{"p.toml", "title = \"t\"\nbase_image = \"evil\"\n", "unknown key base_image"},
		{"p.toml", "title = \"t\"\n[roots]\nx = \"/x\"\n[sync]\nremote = \"u\"\n", "unknown key sync"},
		{"p.toml", "title = \"t\"\n[roots]\nx = \"/x\"\ny = \"/y\"\n", "more than one root"},
		{"p.toml", "title = \"t\"\n[roots]\nX = \"/x\"\n", "not a root name"},
		{"p.toml", "title = \"t\"\n[roots]\nx = \"\"\n", "names no directory"},
		// What the sandbox keeps is the sandbox config's, not a proposal's.
		{"p.toml", "title = \"t\"\n[persist]\nfoo = \"~/.foo\"\n", "sandbox.toml, with no proposal"},
		{"p.toml", "title = \"t\"\n[section]\nname = \"a\"\ntitle = \"A\"\nbody = \"RUN x\"\n", "needs dockerfile_sha256"},
		{"p.toml", "title = \"t\"\ndockerfile_sha256 = \"x\"\n[roots]\nx = \"/x\"\n", "without a [section]"},
		{"p.toml", "title = \"t\"\n" + sec + "body = \"FROM evil\"\n", "FROM"},
		{"p.toml", "title = \"t\"\n" + sec + "body = \"RUN x\\n# caboose:end\"\n", "marker"},
		{"p.toml", "title = \"t\"\n" + sec + "body = \"RUN x\"\nextra = 1\n", "unknown key section.extra"},
		// What could make the terminal lie about the proposal.
		{"p.toml", "title = \"t\\u001b[2K\"\n[roots]\nx = \"/x\"\n", "title has the character U+001B"},
		{"p.toml", "title = \"t\"\nreason = \"a\\rb\"\n[roots]\nx = \"/x\"\n", "U+000D"},
		{"p.toml", "title = \"t\"\n" + sec + "body = \"RUN x \\u202e# y\"\n", "section.body has the character U+202E"},
		{"p.toml", "title = \"t\"\n[roots]\nx = \"/x\\u200b\"\n", "U+200B"},
		{"p.toml", "title = \"a\\nb\"\n[roots]\nx = \"/x\"\n", "U+000A"},
		{"p.toml", "title = \"t\"\n[roots]\nx = \"/x\\u0085\"\n", "U+0085"},
		{"p.toml", "title = \"" + strings.Repeat("x", 101) + "\"\n[roots]\nx = \"/x\"\n", "longer than"},
		{"p.toml", "title = \"t\"\n[roots]\nx = \"/x\"\n" + strings.Repeat("#", MaxSize), "larger than"},
		{"p.toml", "title = \"\xff\"\n", "not UTF-8"},
		{"p.toml", "title = [1]\n", "not a proposal"},
	} {
		_, err := Parse(tc.name, []byte(tc.data))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %q: %v, want %q", tc.name, tc.data, err, tc.want)
		}
	}
}

func TestPrintable(t *testing.T) {
	if got := Printable("a\x1b[2Kb\u202ec d\te"); got != `a\u001B[2Kb\u202Ec d\u0009e` {
		t.Errorf("got %q", got)
	}
}

func TestList(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, Dir)
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if es, err := List(data); err != nil || es != nil {
		t.Fatalf("no dir: %v %v", es, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, CurrentDir), 0o755); err != nil {
		t.Fatal(err)
	}
	write("b.toml", "title = \"t\"\n[roots]\nx = \"/x\"\n")
	write("a.toml", "title = \"t\"\n")
	write(".tmp.toml", "junk")
	write("notes.md", "junk")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "c.toml")); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("#", MaxSize+1)
	write("d.toml", big)

	es, err := List(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(Names(es), " "); got != "a b c d" {
		t.Fatalf("names %q", got)
	}
	if es[0].Err == nil || !strings.Contains(es[0].Err.Error(), "proposes nothing") {
		t.Errorf("a: %v", es[0].Err)
	}
	if es[1].Err != nil || es[1].Proposal.Root.Name != "x" {
		t.Errorf("b: %+v", es[1])
	}
	if es[2].Err == nil || es[2].Proposal != nil {
		t.Errorf("c, a symlink, was read: %+v", es[2])
	}
	if es[3].Err == nil || !strings.Contains(es[3].Err.Error(), "larger than") {
		t.Errorf("d: %v", es[3].Err)
	}
	if err := Remove(data, "b.toml"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.toml")); !os.IsNotExist(err) {
		t.Errorf("b.toml still there: %v", err)
	}
	// Removing the symlink removes it, not what it names.
	if err := Remove(data, "c.toml"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Errorf("/etc/passwd: %v", err)
	}
}

func TestWriteCurrent(t *testing.T) {
	data := t.TempDir()
	df := []byte("FROM x\n")
	s := State{Image: "dockerfile.default", Source: SourceDockerfile, Dockerfile: df, Roots: map[string]StateRoot{"dev": {Host: "~/dev", Path: "/work/dev"}}}
	if err := WriteCurrent(data, s); err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(data, Dir, CurrentDir)
	b, err := os.ReadFile(filepath.Join(cur, "state.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`image = "dockerfile.default"`, `dockerfile = "dockerfile"`, `dockerfile_sha256 = "` + Hash(df) + `"`, "[roots.dev]", `host = "~/dev"`, `path = "/work/dev"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("state.toml lacks %q:\n%s", want, b)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(cur, "Dockerfile")); string(got) != string(df) {
		t.Errorf("Dockerfile %q", got)
	}
	// Under apko or ref, no Dockerfile, and no dockerfile key.
	if err := WriteCurrent(data, State{Image: "apko.default"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(cur, "state.toml")); !strings.Contains(string(b), `image = "apko.default"`) ||
		strings.Contains(string(b), "\ndockerfile") {
		t.Errorf("state.toml:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(cur, "Dockerfile")); !os.IsNotExist(err) {
		t.Errorf("Dockerfile still there: %v", err)
	}
	// A symlink the container put in place is replaced, not written through.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cur, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	if err := WriteCurrent(data, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Errorf("wrote through the symlink: %q", got)
	}
	// A current/ that is a symlink is refused.
	os.RemoveAll(cur)
	if err := os.Symlink(t.TempDir(), cur); err != nil {
		t.Fatal(err)
	}
	if err := WriteCurrent(data, s); err == nil {
		t.Error("wrote through a symlinked current/")
	}
}

func TestParsePackages(t *testing.T) {
	p, err := Parse("p.toml", []byte("title = \"Add graphviz\"\nreason = \"diagrams\"\n[packages]\nadd = [\"graphviz\", \"py3-pip\"]\nremove = [\"jq\"]\n[roots]\nx = \"/x\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Packages == nil || strings.Join(p.Packages.Add, " ") != "graphviz py3-pip" || strings.Join(p.Packages.Remove, " ") != "jq" || p.Root == nil {
		t.Errorf("%+v %+v", p, p.Packages)
	}
	for _, s := range []string{
		"title = \"t\"\n[packages]\nadd = [\"a\"]\n",
		"title = \"t\"\n[packages]\nremove = [\"a\"]\n",
	} {
		if _, err := Parse("p.toml", []byte(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	many := make([]string, MaxPackages)
	for i := range many {
		many[i] = fmt.Sprintf("\"p%d\"", i)
	}
	if _, err := Parse("p.toml", []byte("title = \"t\"\n[packages]\nadd = ["+strings.Join(many, ", ")+"]\n")); err != nil {
		t.Errorf("%d packages: %v", MaxPackages, err)
	}
	sec := "dockerfile_sha256 = \"" + sha + "\"\n[section]\nname = \"a\"\ntitle = \"A\"\nbody = \"RUN x\"\n"
	for _, tc := range []struct{ data, want string }{
		{"title = \"t\"\n[packages]\n", "[packages] proposes nothing"},
		{"title = \"t\"\n[packages]\nadd = []\nremove = []\n", "[packages] proposes nothing"},
		{"title = \"t\"\n" + sec + "[packages]\nadd = [\"a\"]\n", "both [section] and [packages]"},
		{"title = \"t\"\n[packages]\nadd = [\"Graphviz\"]\n", "packages.add: package name \"Graphviz\""},
		{"title = \"t\"\n[packages]\nremove = [\"-x\"]\n", "packages.remove: package name \"-x\" must start"},
		{"title = \"t\"\n[packages]\nadd = [\"a b\"]\n", "packages.add"},
		{"title = \"t\"\n[packages]\nadd = [\"a\\u001b\"]\n", "packages.add"},
		{"title = \"t\"\n[packages]\nadd = [\"\"]\n", "empty"},
		{"title = \"t\"\n[packages]\nadd = [\"a\", \"a\"]\n", "packages.add names a twice"},
		{"title = \"t\"\n[packages]\nadd = [\"a\"]\nremove = [\"a\"]\n", "both adds and removes a"},
		{"title = \"t\"\n[packages]\nadd = [" + strings.Join(many, ", ") + "]\nremove = [\"zz\"]\n", "names 65 packages"},
		{"title = \"t\"\n[packages]\nadd = [\"a\"]\npin = [\"b\"]\n", "unknown key packages.pin"},
		{"title = \"t\"\n[packages]\nadd = \"a\"\n", "not a proposal"},
	} {
		_, err := Parse("p.toml", []byte(tc.data))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.data, err, tc.want)
		}
	}
}

// A check file is written beside its proposal, never through what the
// sandbox put there, is not listed as a proposal, and goes with it.
func TestCheckFile(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.toml"), []byte("title = \"t\"\n[packages]\nadd = [\"a\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "p.check")); err != nil {
		t.Fatal(err)
	}
	if _, ok := CheckModified(data, "p.toml"); ok {
		t.Error("a symlink counts as a check file")
	}
	if err := WriteCheck(data, "p.toml", CheckError, []string{"no package named \"x\x1b[2J\"", "line one\nline two", strings.Repeat("y", 1000)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Errorf("wrote through the symlink: %q", got)
	}
	got, err := os.ReadFile(filepath.Join(dir, "p.check"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 5 || lines[0] != "error" || lines[1] != `no package named "x\u001B[2J"` || lines[2] != "line one" || lines[3] != "line two" ||
		len(lines[4]) != checkLineMax || !strings.HasSuffix(lines[4], "...") {
		t.Errorf("check file:\n%s", got)
	}
	if _, ok := CheckModified(data, "p.toml"); !ok {
		t.Error("no check file")
	}
	var facts []string
	for i := 0; i < 30; i++ {
		facts = append(facts, "fact")
	}
	if n := strings.Count(string(FormatCheck(CheckOK, facts)), "\n"); n != checkLines+2 {
		t.Errorf("%d lines", n)
	}
	es, err := List(data)
	if err != nil || len(es) != 1 || es[0].File != "p.toml" {
		t.Errorf("listed %+v %v", es, err)
	}
	if err := Remove(data, "p.toml"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "p.check")); !os.IsNotExist(err) {
		t.Errorf("the check file stayed: %v", err)
	}
	if err := RemoveCheck(data, "p.toml"); err != nil {
		t.Errorf("removing a missing check: %v", err)
	}
}

func TestWriteCurrentApko(t *testing.T) {
	data := t.TempDir()
	s := State{Image: "apko.default", Apko: &ApkoState{Packages: nil, Defaults: true, Installed: []string{"bash", "jq"}}}
	if err := WriteCurrent(data, s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(data, Dir, CurrentDir, "state.toml"))
	for _, want := range []string{`image = "apko.default"`, "packages = []", "defaults = true", `installed = ["bash", "jq"]`, "a [packages] proposal"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("state.toml lacks %q:\n%s", want, b)
		}
	}
	if err := WriteCurrent(data, State{Image: "ref.mine"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(data, Dir, CurrentDir, "state.toml"))
	for _, not := range []string{"\npackages", "\ndefaults", "\ninstalled"} {
		if strings.Contains(string(b), not) {
			t.Errorf("state.toml for a ref has %q:\n%s", not, b)
		}
	}
}

// Whatever the sandbox put at a proposal's check file's name, a directory
// with something in it say, the proposal itself still goes.
func TestRemoveDespiteAPlantedCheck(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, Dir)
	if err := os.MkdirAll(filepath.Join(dir, "p.check", "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.toml"), []byte("title = \"t\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(data, "p.toml"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "p.toml")); !os.IsNotExist(err) {
		t.Errorf("the proposal stayed: %v", err)
	}
}
