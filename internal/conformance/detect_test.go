package conformance

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

// This sandbox's own environment must be refused: the real detectors run
// where /etc/claude-code exists (inside a caboose sandbox), and the test
// is skipped elsewhere.
func TestRefusesThisSandbox(t *testing.T) {
	if _, err := os.Lstat("/etc/claude-code"); err != nil {
		t.Skip("not inside a caboose sandbox")
	}
	err := Preflight(Facts{})
	var re *RefusalError
	if !errors.As(err, &re) || re.Item != "sandbox" || !strings.Contains(err.Error(), sandboxMessage) {
		t.Fatalf("Preflight in a sandbox: %v", err)
	}
	f, tmp, _ := testFacts(t)
	f.Detectors = nil // the real ones
	if _, err := Setup(f, config.KindContainer, config.TrustFallible); err == nil {
		t.Fatal("Setup ran in a sandbox")
	}
	if es, _ := os.ReadDir(tmp); len(es) != 0 {
		t.Fatalf("Setup made %v in a sandbox", es)
	}
}

func TestEachDetectorRefuses(t *testing.T) {
	no := func() (bool, error) { return false, nil }
	names := []string{}
	for _, d := range DefaultDetectors() {
		names = append(names, d.Name)
	}
	if len(names) < 7 {
		t.Fatalf("detectors: %v", names)
	}
	for i := range names {
		for _, verdict := range []func() (bool, error){
			func() (bool, error) { return true, nil },
			func() (bool, error) { return false, errors.New("unreadable") },
		} {
			ds := make([]Detector, len(names))
			for j, n := range names {
				ds[j] = Detector{n, no}
			}
			ds[i].Present = verdict
			if err := Preflight(Facts{Detectors: ds, GOOS: "darwin"}); err == nil {
				t.Fatalf("%s present did not refuse", names[i])
			}
		}
	}
	ds := make([]Detector, len(names))
	for j, n := range names {
		ds[j] = Detector{n, no}
	}
	if err := Preflight(Facts{Detectors: ds, GOOS: "linux"}); err != nil {
		t.Fatalf("no sign refused: %v", err)
	}
}

func TestDetectorFunctions(t *testing.T) {
	dir := t.TempDir()
	if ok, err := exists(dir + "/none")(); ok || err != nil {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if ok, err := exists(dir)(); !ok || err != nil {
		t.Fatalf("present: %v %v", ok, err)
	}
	for content, want := range map[string]bool{
		"0::/\n":                                      false,
		"0::/system.slice/docker-0123.scope\n":        true,
		"12:pids:/kubepods/besteffort/pod1/abc\n":     true,
		"1:name=systemd:/user.slice/user-501.slice\n": false,
	} {
		p := dir + "/cgroup"
		must(t, os.WriteFile(p, []byte(content), 0o644))
		if got, err := cgroupNamesRuntime(p)(); got != want || err != nil {
			t.Fatalf("%q: %v %v", content, got, err)
		}
	}
	if ok, err := cgroupNamesRuntime(dir + "/none")(); ok || err != nil {
		t.Fatalf("no cgroup file: %v %v", ok, err)
	}
}

func TestPlatform(t *testing.T) {
	for goos, ok := range map[string]bool{"darwin": true, "linux": true, "windows": false, "freebsd": false} {
		if err := checkPlatform(goos); (err == nil) != ok {
			t.Fatalf("%s: %v", goos, err)
		}
	}
}

// What the product writes is skipped by the walk, and nothing else: no
// entry reaches roots/, canary/, the markers or the environment's
// config.toml.
func TestProductWritten(t *testing.T) {
	const D = "home/envs/cf-0011aabb/data/"
	for _, p := range append(slices.Clone(productWritten), vmProductWritten...) {
		if !strings.HasPrefix(p, "home/") && !strings.HasPrefix(p, "<data>/") {
			t.Errorf("%s is not under the throwaway CABOOSE_HOME", p)
		}
		if strings.HasPrefix(p, "roots/") || strings.HasPrefix(p, "canary/") {
			t.Errorf("%s is walked in full and cannot be the product's", p)
		}
	}
	vmOnly := []string{"home/vm/v1.0.0/arm64/kernel", D + "vm/caboose-cf/state.json", D + "vm/images/x.img"}
	for _, iso := range config.Kinds {
		pats := fillProductPatterns(iso, "home/envs/cf-0011aabb/data", "cf-0011aabb")
		for _, rel := range []string{
			D + "claude-code/CLAUDE.md", D + "sync/.git/HEAD", D + "proposals/current/state.toml",
			D + "home/.config/caboose/sandbox.toml", D + "sandbox.last-good.toml",
			"home/envs/cf-0011aabb/broker.lock", "home/envs/cf-0011aabb/checkpoints/k/1",
			D + "link.json", "home/daemon/daemon.log",
			"home/envs/cf-0011aabb/.config.toml-123",
			"home/cache/apk/x/aarch64/binutils-2.47-r1/expand-apk1/stream-1.tar",
		} {
			if !productPath(pats, rel) {
				t.Errorf("%s: %s is not taken as the product's", iso, rel)
			}
		}
		for _, rel := range []string{
			"roots/conf-0011aabb-ro/x", "canary/conf-0011aabb-outside", "home/.conformance", D + ".conformance-data",
			"home/envs/cf-0011aabb", D + "home/x", "home/envs/cf-0011aabb/data",
			"home/envs/other/broker.lock", D + "sandbox.last-good.toml.x", "home/envs/other/config.toml", "home/envs/cf-0011aabb/config.toml",
			"home/cachex/y", "home/envs/cf-0011aabb/cache/z", "home/cache/x", "home/cache/apkx/y",
			"data/claude-code/CLAUDE.md", "data/link.json", "home/envs/other/data/link.json", StubLog, StubDir + "/open",
			// No run showed the product writing these, and no code of
			// it does during a run: the sandbox's ~/.ssh, the launches'
			// attach records (which no mount may reach) and the apko
			// locks beside config.toml stay walked.
			D + "home/.ssh", D + "home/.ssh/config", D + "home/.ssh/known_hosts",
			D + "attached", D + "attached/123",
			"home/envs/cf-0011aabb/apko-default.lock.json", "home/envs/cf-0011aabb/apko-x.lock.json",
			"home/envs/other/data/vm/x/state.json",
		} {
			if productPath(pats, rel) {
				t.Errorf("%s: %s is taken as the product's", iso, rel)
			}
		}
		for _, rel := range vmOnly {
			if got := productPath(pats, rel); got != (iso == config.KindVM) {
				t.Errorf("%s: %s taken as the product's: %v", iso, rel, got)
			}
		}
	}
}
