package caboose

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func needBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
}

// versionOrder sorts versions newest first through entrypoint.sh's own
// version_newer, lifted out of the script: sourcing the whole thing would
// run its dispatch.
func versionOrder(t *testing.T, versions []string) []string {
	t.Helper()
	src, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "\nversion_newer() {\n")
	if start < 0 {
		t.Fatal("no version_newer function in entrypoint.sh")
	}
	end := strings.Index(s[start+1:], "\n}\n")
	if end < 0 {
		t.Fatal("version_newer has no closing brace")
	}
	fn := s[start+1 : start+1+end+2]
	script := fn + `
set -euo pipefail
sorted=()
for v in "$@"; do
    i=${#sorted[@]}
    while [ "$i" -gt 0 ] && version_newer "$v" "${sorted[i-1]}"; do
        sorted[i]="${sorted[i-1]}"; i=$((i - 1))
    done
    sorted[i]="$v"
done
printf '%s\n' "${sorted[@]}"
`
	out, err := exec.Command("bash", append([]string{"-c", script, "order"}, versions...)...).Output()
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	return strings.Fields(string(out))
}

// The version order replaced `sort -Vr`, which BusyBox may not have. Where
// a sort with -V is around, the two must agree; the fixed expectations
// cover the cases that matter without one.
func TestVersionOrder(t *testing.T) {
	needBash(t)
	_, sortErr := exec.Command("sort", "-V", "/dev/null").Output()
	for _, tc := range []struct{ name, in, want string }{
		{"releases", "2.1.9 2.1.10 2.0.99 10.0.0 2.1.8 2.10.0 2.1.100 1.0.0 2.2.0 3.0.0",
			"10.0.0 3.0.0 2.10.0 2.2.0 2.1.100 2.1.10 2.1.9 2.1.8 2.0.99 1.0.0"},
		// Fewer fields rank first, leading zeros are decimal (not octal),
		// and a suffix ranks after the bare number.
		{"edges", "2.1.0 2.1 2.1.08 2.1.7 2.1.7b", "2.1.08 2.1.7b 2.1.7 2.1.0 2.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, want := strings.Fields(tc.in), strings.Fields(tc.want)
			if got := versionOrder(t, in); !slices.Equal(got, want) {
				t.Errorf("order = %v, want %v", got, want)
			}
			if sortErr != nil {
				return
			}
			cmd := exec.Command("sort", "-Vr")
			cmd.Stdin = strings.NewReader(strings.Join(in, "\n") + "\n")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if gnu := strings.Fields(string(out)); !slices.Equal(gnu, want) {
				t.Errorf("sort -Vr = %v; the expectation is wrong", gnu)
			}
		})
	}
}

// --cc-prune against a fake HOME: keep the newest CABOOSE_KEEP_VERSIONS by
// version order, plus whatever ~/.local/bin/claude points at, and remove the
// rest, a directory-shaped version included.
func TestEntrypointPrune(t *testing.T) {
	needBash(t)
	home := t.TempDir()
	versions := filepath.Join(home, ".local/share/claude/versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"2.1.9", "2.1.10", "2.1.8", "2.0.99", "10.0.0"} {
		if err := os.WriteFile(filepath.Join(versions, v), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A directory layout, and a dotfile that must be left alone as ls -1
	// left it.
	if err := os.MkdirAll(filepath.Join(versions, "2.1.7/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versions, "2.1.7/sub/f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versions, ".hidden"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local/bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The active version is an old one: it survives anyway.
	if err := os.Symlink(filepath.Join(versions, "2.0.99"), filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "entrypoint.sh", "--cc-prune")
	cmd.Env = append(os.Environ(), "HOME="+home, "CABOOSE_KEEP_VERSIONS=2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--cc-prune: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pruned 3 old version(s); kept 3 (active: 2.0.99)") {
		t.Errorf("output:\n%s", out)
	}
	entries, err := os.ReadDir(versions)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{".hidden", "10.0.0", "2.0.99", "2.1.10"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

// With an empty versions directory, and no claude symlink, pruning is a
// quiet no-op rather than an error under set -u.
func TestEntrypointPruneEmpty(t *testing.T) {
	needBash(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local/share/claude/versions"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "entrypoint.sh", "--cc-prune")
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("--cc-prune: %v\n%s", err, out)
	}
}

// --cc-start runs ~/.config/caboose/start.d the way the supervisor does:
// every executable file, one at a time in byte order, in the home, carrying
// on past a failure, skipping dotfiles, backups, directories and files that
// are not executable, and not waiting for what a script leaves running.
func TestEntrypointStart(t *testing.T) {
	needBash(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".config/caboose/start.d")
	if err := os.MkdirAll(filepath.Join(dir, "25-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(home, "ran")
	script := func(name string, mode os.FileMode, body string) {
		t.Helper()
		sh := "#!/bin/sh\necho \"" + name + " $PWD\" >> '" + ran + "'\n" + body
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sh), mode); err != nil {
			t.Fatal(err)
		}
	}
	script("20-b", 0o755, "sleep 5 >/dev/null 2>&1 &\n")
	script("10-a", 0o755, "exit 4\n")
	script("B", 0o755, "")
	script("a", 0o755, "")
	script("15-noexec", 0o644, "")
	script("30-c~", 0o755, "")
	script(".hidden", 0o755, "")

	ep, err := filepath.Abs("entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", ep, "--cc-start")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--cc-start: %v\n%s", err, out)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("--cc-start took %v: it waited for what a script left running", d)
	}
	got, _ := os.ReadFile(ran)
	want := "10-a " + home + "\n20-b " + home + "\nB " + home + "\na " + home + "\n"
	if string(got) != want {
		t.Errorf("ran:\n%s\nwant:\n%s", got, want)
	}
	for _, line := range []string{"caboose: start.d/10-a: running", "caboose: start.d/10-a: exited 4", "caboose: start.d/20-b: running"} {
		if !strings.Contains(string(out), line+"\n") {
			t.Errorf("no %q in:\n%s", line, out)
		}
	}
	if strings.Contains(string(out), "20-b: exited") {
		t.Errorf("20-b exited 0, but:\n%s", out)
	}
}

// With no start.d at all, --cc-start is a quiet no-op.
func TestEntrypointStartNone(t *testing.T) {
	needBash(t)
	cmd := exec.Command("bash", "entrypoint.sh", "--cc-start")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("--cc-start: %v\n%s", err, out)
	}
}

// entrypointFunc is the function name from entrypoint.sh, lifted out of
// it as versionOrder does: sourcing the whole script would run its
// dispatch.
func entrypointFunc(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "\n"+name+"() {")
	if start < 0 {
		t.Fatalf("no %s function in entrypoint.sh", name)
	}
	end := strings.Index(s[start+1:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s has no closing brace", name)
	}
	return s[start+1:start+1+end+2] + "\n"
}

// Run as root with HOME elsewhere (vm, and gVisor where the agent cannot
// write its mounts), the entrypoint points root's passwd entry at HOME, so
// that ssh, which reads its home from passwd, finds the kept ~/.ssh.
func TestEntrypointRootHome(t *testing.T) {
	needBash(t)
	script := "set -euo pipefail\nlog() { printf 'caboose: %s\\n' \"$*\" >&2; }\n" +
		entrypointFunc(t, "point_root_home") + `point_root_home "$1" "$2"` + "\n"
	for _, tc := range []struct{ name, in, want, said string }{
		{"moved",
			"root:x:0:0:root:/root:/bin/bash\nagent:x:501:20::/home/agent:/bin/bash\n",
			"root:x:0:0:root:/home/agent:/bin/bash\nagent:x:501:20::/home/agent:/bin/bash\n",
			"caboose: root's home in PASSWD is now /home/agent\n"},
		{"already", "root:x:0:0:root:/home/agent:/bin/bash\n", "", ""},
		// getpwuid(0) answers with the first entry; a second is not root's
		// home, and comments and NIS lines are no entry at all.
		{"first only",
			"# x:x:0:0::/c:/bin/sh\n+::0:0::/n:\ntoor:x:0:0::/root:\nroot:x:0:0:root:/root:/bin/sh\n",
			"# x:x:0:0::/c:/bin/sh\n+::0:0::/n:\ntoor:x:0:0::/home/agent:\nroot:x:0:0:root:/root:/bin/sh\n",
			"caboose: root's home in PASSWD is now /home/agent\n"},
		{"no newline at the end", "root:x:0:0:root:/root:/bin/sh", "root:x:0:0:root:/home/agent:/bin/sh\n",
			"caboose: root's home in PASSWD is now /home/agent\n"},
		{"no root", "agent:x:501:20::/home/agent:/bin/bash\n", "", ""},
		{"uid 0 only", "r:x:00:0::/root:/bin/sh\nn:x:10:0::/root:/bin/sh\n", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			passwd := filepath.Join(t.TempDir(), "passwd")
			if err := os.WriteFile(passwd, []byte(tc.in), 0o640); err != nil {
				t.Fatal(err)
			}
			old, _ := os.Stat(passwd)
			var stderr strings.Builder
			cmd := exec.Command("bash", "-c", script, "root-home", passwd, "/home/agent")
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("point_root_home: %v\n%s", err, stderr.String())
			}
			want := tc.want
			if want == "" {
				want = tc.in
			}
			got, _ := os.ReadFile(passwd)
			if string(got) != want {
				t.Errorf("passwd:\n%s\nwant:\n%s", got, want)
			}
			if said := strings.ReplaceAll(tc.said, "PASSWD", passwd); stderr.String() != said {
				t.Errorf("said %q, want %q", stderr.String(), said)
			}
			st, _ := os.Stat(passwd)
			if !os.SameFile(old, st) || st.Mode().Perm() != 0o640 {
				t.Errorf("passwd was replaced or its mode changed: %v", st.Mode())
			}
			if tc.want == "" && !st.ModTime().Equal(old.ModTime()) {
				t.Errorf("passwd was written though nothing changed")
			}
		})
	}
}
