package repocheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The forbidden strings are built from parts so that this file does not trip
// the check it tests.
var (
	sockPath = strings.Join([]string{"/tmp/caboose", "agent.sock"}, "-")
	qualRef  = "agent." + "SocketPath"
	bare     = "Socket" + "Path"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanTests(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/literal_test.go", "package a\n\nvar s = \""+sockPath+"\"\n")
	write(t, root, "b/qual_test.go", "package b_test\n\nvar s = "+qualRef+"\n")
	write(t, root, "agent/bare_test.go", "package agent\n\nvar s = "+bare+"\n")
	write(t, root, "agent/bare2_test.go", "package agent // x\n\nvar s = "+bare+"\n")
	write(t, root, "testdata/x/t_test.go", "package x\n\nvar s = \""+sockPath+"\"\n")
	write(t, root, "e/alias_test.go", "package e\n\nimport ag \"x/internal/agent\"\n\nvar s = ag."+bare+"\n")
	write(t, root, "f/concat_test.go", "package f\n\nvar s = \"/tmp/caboose-agent\" + \".sock\"\n")
	write(t, root, "g/dot_test.go", "package g\n\nimport . \"x/internal/agent\"\n\nvar s = "+bare+"\n")
	// Allowed: a bare name outside package agent, other files, hidden dirs.
	write(t, root, "c/ok_test.go", "package c\n\nvar "+bare+" = 1\n")
	write(t, root, "agent/prod.go", "package agent\n\nconst "+bare+" = \""+sockPath+"\"\n")
	write(t, root, ".claude/w/hidden_test.go", "package w\n\nvar s = \""+sockPath+"\"\n")
	write(t, root, "d/ok_test.go", "package d\n\nvar s = \"/tmp/other.sock\"\n")

	got, err := ScanTests(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"a/literal_test.go":    true,
		"b/qual_test.go":       true,
		"agent/bare_test.go":   true,
		"agent/bare2_test.go":  true,
		"testdata/x/t_test.go": true,
		"e/alias_test.go":      true,
		"f/concat_test.go":     true,
		"g/dot_test.go":        true,
	}
	seen := map[string]bool{}
	for _, f := range got {
		if !want[f.File] {
			t.Errorf("unexpected finding %+v", f)
		}
		if f.Line != 3 && f.File != "e/alias_test.go" && f.File != "g/dot_test.go" {
			t.Errorf("%s: line %d, want 3", f.File, f.Line)
		}
		seen[f.File] = true
	}
	for f := range want {
		if !seen[f] {
			t.Errorf("missed %s", f)
		}
	}
}

// TestRepoTestsDoNotNameTheLink is the check itself, over this repository.
func TestRepoTestsDoNotNameTheLink(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not the repository root: %v", err)
	}
	got, err := ScanTests(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Errorf("%s:%d %s: tests must use a socket path of their own, never the real link", f.File, f.Line, f.What)
	}
}

func TestCheckDockerGuard(t *testing.T) {
	root := t.TempDir()
	const imp = "import (\n\t\"os\"\n\t\"testing\"\n\n\tnd \"github.com/bfreis/caboose/internal/nodocker\"\n)\n\n"
	write(t, root, "good/main_test.go", "package good\n\n"+imp+"func TestMain(m *testing.M) {\n\tif len(os.Args) > 3 {\n\t\tos.Exit(2)\n\t}\n\tos.Exit(nd.Run(m))\n}\n")
	write(t, root, "plain/main_test.go", "package plain\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n")
	write(t, root, "none/x_test.go", "package none\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")
	// Calls Run, but outside TestMain, or of another package named nodocker.
	write(t, root, "elsewhere/x_test.go", "package elsewhere\n\n"+imp+"func TestMain(m *testing.M) { os.Exit(m.Run()) }\n\nfunc helper(m *testing.M) int { return nd.Run(m) }\n")
	write(t, root, "other/x_test.go", "package other\n\nimport (\n\t\"os\"\n\t\"testing\"\n\n\t\"example.com/nodocker\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(nodocker.Run(m)) }\n")
	// Only in a subdirectory, or in a file that is not a test.
	write(t, root, "deep/sub/main_test.go", "package sub\n\n"+imp+"func TestMain(m *testing.M) { os.Exit(nd.Run(m)) }\n")
	write(t, root, "prod/main.go", "package prod\n\n"+imp+"func TestMain(m *testing.M) { os.Exit(nd.Run(m)) }\n")

	got, err := CheckDockerGuard(root, []string{"good", "plain", "none", "elsewhere", "other", "deep", "prod"})
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, f := range got {
		bad = append(bad, f.File)
	}
	if want := []string{"plain", "none", "elsewhere", "other", "deep", "prod"}; strings.Join(bad, " ") != strings.Join(want, " ") {
		t.Errorf("flagged %v, want %v", bad, want)
	}
	if _, err := CheckDockerGuard(root, []string{"gone"}); err == nil {
		t.Error("a missing package passed")
	}
}

// TestRepoDockerPackagesAreGuarded is the check itself, over this repository.
func TestRepoDockerPackagesAreGuarded(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	got, err := CheckDockerGuard(root, DockerPackages)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Errorf("%s %s: its tests must fail on a call to the real docker", f.File, f.What)
	}
}
