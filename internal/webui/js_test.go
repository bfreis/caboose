package webui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The page's script tests: node --test over static/*.test.mjs, with fakes
// for the browser (a document, fetch, storage, timers), so no browser and
// no network. Skipped, saying why, where no node 18 or newer is on PATH.
func TestJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the page's script tests (static/*.test.mjs) did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "NO_COLOR=1", "LANG=C.UTF-8"}

	v, err := nodeCmd(ctx, node, env, "--version").Output()
	if err != nil {
		t.Skipf("node --version failed (%v): the page's script tests did not run", err)
	}
	major, ok := nodeMajor(strings.TrimSpace(string(v)))
	if !ok || major < 18 {
		t.Skipf("node %q is older than 18: the page's script tests did not run", strings.TrimSpace(string(v)))
	}

	files, err := filepath.Glob(filepath.Join("static", "*.test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app.test.mjs", "dom.test.mjs", "model.test.mjs", "pages.test.mjs"} {
		if !slices.Contains(files, filepath.Join("static", want)) {
			t.Fatalf("static/%s is missing", want)
		}
	}
	args := []string{"--test"}
	for _, f := range files {
		args = append(args, filepath.Base(f))
	}
	cmd := nodeCmd(ctx, node, env, args...)
	cmd.Dir = "static"
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("node --test ran past 60 s:\n%s", tail(out))
	}
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, tail(out))
	}
	t.Logf("node %s: %s", strings.TrimSpace(string(v)), summary(out))
}

// nodeCmd is node with only env, and nothing of this process's: no
// NODE_OPTIONS, proxies or the user's own settings.
func nodeCmd(ctx context.Context, node string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = env
	return cmd
}

var nodeVersion = regexp.MustCompile(`^v([0-9]+)\.`)

func nodeMajor(v string) (int, bool) {
	m := nodeVersion.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// tail is the end of node's output, enough to show a failure.
func tail(out []byte) string {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > 80 {
		lines = append([]string{"..."}, lines[len(lines)-80:]...)
	}
	return strings.Join(lines, "\n")
}

// summary is node's count of tests and passes, in either reporter's form.
func summary(out []byte) string {
	var s []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimLeft(l, "#ℹ ")
		if strings.HasPrefix(l, "tests ") || strings.HasPrefix(l, "pass ") || strings.HasPrefix(l, "fail ") {
			s = append(s, l)
		}
	}
	return strings.Join(s, ", ")
}

func TestNodeMajor(t *testing.T) {
	for v, want := range map[string]int{"v18.0.0": 18, "v24.22.0": 24, "v9.1.0": 9} {
		if n, ok := nodeMajor(v); !ok || n != want {
			t.Errorf("nodeMajor(%q) = %d, %v", v, n, ok)
		}
	}
	for _, v := range []string{"", "18.0.0", "vx.1"} {
		if _, ok := nodeMajor(v); ok {
			t.Errorf("nodeMajor(%q) ok", v)
		}
	}
}
