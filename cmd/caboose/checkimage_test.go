package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Probe output for the fake docker's `run` to print: a complete glibc image
// and alpine as it comes. The probe itself is tested in internal/imagecheck;
// this is about what the command makes of it.
const (
	probeTools = `ok tool:env /usr/bin/env
ok tool:readlink /usr/bin/readlink
ok tool:mktemp /usr/bin/mktemp
ok tool:find /usr/bin/find
ok tool:rm /usr/bin/rm
ok tool:rmdir /usr/bin/rmdir
ok tool:sleep /usr/bin/sleep
ok tool:test /usr/bin/test
ok tool:uname /usr/bin/uname
ok tool:mkdir /usr/bin/mkdir
ok tool:chmod /usr/bin/chmod
ok tool:cut /usr/bin/cut
ok tool:sed /usr/bin/sed
ok tool:tr /usr/bin/tr
ok tool:grep /usr/bin/grep
ok tool:head /usr/bin/head
ok tool:sha256sum /usr/bin/sha256sum
ok tool:chown /usr/bin/chown
ok etc:passwd /etc/passwd
ok etc:group /etc/group
ok home /home/agent (created by the layer)
`
	probeComplete = `probe 1
ok sh /bin/sh
ok bash /usr/bin/bash
ok curl /usr/bin/curl
ok tmux /usr/bin/tmux
ok git /usr/bin/git 2.53.0
ok tic /usr/bin/tic
` + probeTools + `info libc glibc
ok libc glibc /lib/aarch64-linux-gnu/libc.so.6
info arch aarch64
ok arch aarch64
info platform linux-arm64
info uid 1000 ubuntu
info gid 1000 ubuntu
ok net https://claude.ai
ok net https://downloads.claude.ai
ok cacerts /etc/ssl/certs/ca-certificates.crt
end
`
	probeAlpine = `probe 1
ok sh /bin/sh
missing bash
missing curl
missing tmux
missing git
missing tic
` + probeTools + `info libc musl
ok libc musl /lib/libc.musl-aarch64.so.1
info arch aarch64
ok arch aarch64
info platform linux-arm64-musl
missing libgcc no libgcc_s.so.1
missing libstdc++ no libstdc++.so.6
missing ripgrep
info uid 1000
info gid 1000
warn net https://claude.ai not checked: no curl
warn net https://downloads.claude.ai not checked: no curl
ok cacerts /etc/ssl/certs/ca-certificates.crt
end
`
)

// probeRuns makes `docker run` print out, and the image be present.
func probeRuns(t *testing.T, out string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "probe.out")
	if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	return `case "$1" in
  version) exit 0 ;;
  run) cat "` + f + `"; exit 0 ;;
  image) [ "$2" = inspect ] && { echo '{}'; exit 0; } ;;
esac`
}

// imageMissingButPullable: the image is absent until pulled; pull prints to
// stdout, as docker's does, which must end up on stderr.
const imageMissingButPullable = `case "$1" in
  image) [ "$2" = inspect ] && { echo "Error: No such image: $5" >&2; exit 1; } ;;
  pull) echo "pull progress on stdout"; echo "pull progress on stderr" >&2; exit 0 ;;
esac`

func checkEnv(t *testing.T, kv ...string) string {
	t.Helper()
	// No repo root: caboose check-image needs none.
	return sandboxEnv(t, append([]string{"CABOOSE_IMAGE", "img", "CABOOSE_REPO_ROOT", "/does/not/exist"}, kv...)...)
}

func TestCheckImageComplete(t *testing.T) {
	log := scriptedDocker(t, probeRuns(t, probeComplete))
	home := checkEnv(t)
	code, out, errs := runIt("check-image", "debian:stable")
	if code != 0 || errs != "" {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	want := `
  caboose check-image  debian:stable

  ✓ /bin/sh   ok
  ✓ bash      ok (/usr/bin/bash)
  ✓ curl      ok (/usr/bin/curl)
  ✓ ca-certs  ok (/etc/ssl/certs/ca-certificates.crt)
  ✓ tmux      ok (/usr/bin/tmux)
  ✓ git       ok (/usr/bin/git 2.53.0)
  ✓ libc      glibc
  ✓ platform  linux-arm64
  ✓ tools     ok
  ✓ user      ok
  ✓ tic       ok (/usr/bin/tic)
  ✓ uid       1000 (held by user 'ubuntu'; the layer takes it over as agent)
  ✓ gid       1000 (held by group 'ubuntu'; the layer makes it agent's group, as it is)
  ✓ network   ok (https://claude.ai reachable)

  ✓ Usable: every requirement is met.
`
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	// The log holds "$*", and the script spans lines.
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "\nrun --rm --init --user 0:0 -e CABOOSE_PROBE_ROOT= --entrypoint /bin/sh debian:stable -c #!/bin/sh\n") ||
		!strings.Contains(string(b), "\nexit 0\n caboose-probe "+strconv.Itoa(os.Getuid())+" "+strconv.Itoa(os.Getgid())+"\n") {
		t.Errorf("the probe did not run as expected:\n%s", b)
	}
	for _, l := range dockerLog(t, log) {
		if strings.HasPrefix(l, "pull ") || strings.HasPrefix(l, "build ") {
			t.Errorf("docker %s", l)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".caboose")); !os.IsNotExist(err) {
		t.Error("caboose check-image created the data dir")
	}
}

func TestCheckImageBareAlpine(t *testing.T) {
	scriptedDocker(t, probeRuns(t, probeAlpine))
	checkEnv(t)
	code, out, errs := runIt("check-image", "alpine")
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	want := `
  caboose check-image  alpine

  ✓ /bin/sh    ok
  ✗ bash       missing
  ✗ curl       missing
  ✓ ca-certs   ok (/etc/ssl/certs/ca-certificates.crt)
  ✗ tmux       missing
  ✗ git        missing
  ✓ libc       musl
  ✓ platform   linux-arm64-musl
  ✗ libgcc     missing (no libgcc_s.so.1)
  ✗ libstdc++  missing (no libstdc++.so.6)
  ✗ ripgrep    missing
  ✓ tools      ok
  ✓ user       ok
  ! tic        missing (optional)
  ✓ uid        1000 (free)
  ✓ gid        1000 (free)
  ! network    warning: https://claude.ai not checked: no curl; https://downloads.claude.ai not checked: no curl

  ✗ Not usable: 7 requirements unmet.
`
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	for _, w := range []string{
		"caboose: bash: missing -- entrypoint.sh and Claude Code's installer are bash scripts",
		"caboose: libgcc: missing (no libgcc_s.so.1) -- the musl build of Claude Code links libgcc_s.so.1\n",
		"caboose: ripgrep: missing -- ",
	} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	// With no curl, reachability is unknown, and curl is already named.
	if strings.Contains(errs, "did not answer") {
		t.Errorf("stderr:\n%s", errs)
	}
}

// An image that is fine but cannot reach claude.ai passes, with a warning.
func TestCheckImageUnreachable(t *testing.T) {
	scriptedDocker(t, probeRuns(t, strings.Replace(probeComplete,
		"ok net https://claude.ai\n", "warn net https://claude.ai unreachable (curl exit 6)\n", 1)))
	checkEnv(t)
	code, out, errs := runIt("check-image", "x")
	if code != 0 || !strings.Contains(out, "  ! network   warning: https://claude.ai unreachable (curl exit 6)\n") ||
		!strings.Contains(out, "  ✓ Usable: every requirement is met.") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
	if !strings.Contains(errs, "caboose: claude.ai did not answer from a container of this image.") {
		t.Errorf("stderr:\n%s", errs)
	}
}

func TestCheckImageNoShell(t *testing.T) {
	scriptedDocker(t, `case "$1" in
  version) exit 0 ;;
  image) echo '{}'; exit 0 ;;
  run) echo 'docker: Error response from daemon: ... exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown.' >&2; exit 127 ;;
esac`)
	checkEnv(t)
	code, out, errs := runIt("check-image", "gcr.io/distroless/static")
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	if want := "\n  caboose check-image  gcr.io/distroless/static\n\n  ✗ /bin/sh  missing\n\n  ✗ Not usable: 1 requirement unmet.\n"; out != want {
		t.Errorf("stdout:\n%s", out)
	}
	if !strings.Contains(errs, "caboose: /bin/sh: missing -- ") || strings.Contains(errs, "OCI") {
		t.Errorf("stderr:\n%s", errs)
	}
}

// A named image that is not local is pulled first, docker's output all on
// stderr, and then checked.
func TestCheckImagePulls(t *testing.T) {
	log := scriptedDocker(t, imageMissingButPullable+"\n"+probeRuns(t, probeComplete))
	checkEnv(t)
	code, out, errs := runIt("check-image", "ubuntu:26.04")
	if code != 0 {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	if strings.Contains(out, "pull progress") {
		t.Errorf("docker pull wrote to stdout:\n%s", out)
	}
	for _, w := range []string{
		"caboose: image 'ubuntu:26.04' is not in the local store; pulling it (docker's output follows)\n",
		"pull progress on stdout\npull progress on stderr\n",
	} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	var pulled bool
	for _, l := range dockerLog(t, log) {
		pulled = pulled || l == "pull ubuntu:26.04"
		if strings.HasPrefix(l, "run ") && !pulled {
			t.Error("docker run before the pull")
		}
	}
	if !pulled {
		t.Error("not pulled")
	}
}

// With no IMAGE, CABOOSE_BASE_IMAGE is checked, and pulled if need be...
func TestCheckImageDefaultsToBaseImage(t *testing.T) {
	log := scriptedDocker(t, imageMissingButPullable+"\n"+probeRuns(t, probeComplete))
	checkEnv(t, "CABOOSE_BASE_IMAGE", "mybase:1")
	if code, out, errs := runIt("check-image"); code != 0 || !strings.HasPrefix(out, "\n  caboose check-image  mybase:1\n") {
		t.Errorf("exit %d, stdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "\npull mybase:1\n") {
		t.Errorf("docker log:\n%s", b)
	}
}

// ...and without it, the default base caboose builds, which is never
// pulled: a registry's image of that name is not the one caboose would
// build. Not the image caboose runs, either: that one has the layer on it.
func TestCheckImageDefaultNotBuilt(t *testing.T) {
	log := scriptedDocker(t, imageAbsent)
	checkEnv(t)
	code, out, errs := runIt("check-image")
	if code != 2 || out != "\n  caboose check-image  img-base\n\n" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
	if !strings.Contains(errs, "caboose: base image 'img-base' is not built yet\n") || !strings.Contains(errs, "'caboose build'") {
		t.Errorf("stderr:\n%s", errs)
	}
	for _, l := range dockerLog(t, log) {
		if strings.HasPrefix(l, "pull ") || strings.HasPrefix(l, "run ") || strings.HasPrefix(l, "build ") {
			t.Errorf("docker %s", l)
		}
	}
}

func TestCheckImageDefaultBuilt(t *testing.T) {
	log := scriptedDocker(t, probeRuns(t, probeComplete))
	checkEnv(t)
	if code, out, _ := runIt("check-image"); code != 0 || !strings.HasPrefix(out, "\n  caboose check-image  img-base\n\n  ✓ /bin/sh   ok\n") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), " --entrypoint /bin/sh img-base -c ") {
		t.Errorf("docker log:\n%s", b)
	}
}

// Could not check is 2, never 1: that is the answer "not usable".
func TestCheckImageCannotCheck(t *testing.T) {
	for _, tc := range []struct{ name, docker, want string }{
		{"engine down", daemonDown, "caboose: cannot inspect image 'x': is the docker engine running?"},
		{"invalid name", invalidRef, "caboose: cannot inspect image 'x': docker image inspect x: invalid reference format"},
		{"pull fails", `case "$1" in
  version) exit 0 ;;
  image) exit 1 ;;
  pull) echo "pull access denied for x" >&2; exit 1 ;;
esac`, "caboose: cannot pull image 'x'"},
		{"run fails", `case "$1" in
  version) exit 0 ;;
  image) echo '{}'; exit 0 ;;
  run) echo "docker: Error response from daemon: no space left on device" >&2; exit 125 ;;
esac`, "caboose: cannot check image 'x': docker run x: docker: Error response from daemon: no space left on device"},
		{"engine gone mid-way", `case "$1" in
  version) exit 1 ;;
  image) echo '{}'; exit 0 ;;
  run) echo "Cannot connect to the Docker daemon" >&2; exit 1 ;;
esac`, "caboose: cannot check image 'x': is the docker engine running?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scriptedDocker(t, tc.docker)
			checkEnv(t)
			code, _, errs := runIt("check-image", "x")
			if code != 2 || !strings.Contains(errs, tc.want) {
				t.Errorf("exit %d, stderr:\n%s", code, errs)
			}
		})
	}
}

func TestCheckImageRejectsArguments(t *testing.T) {
	for _, argv := range [][]string{{"a", "b"}, {"--json"}} {
		log := fakeDocker(t)
		checkEnv(t)
		code, out, errs := runIt(append([]string{"check-image"}, argv...)...)
		if code != 1 || out != "" || !strings.HasPrefix(errs, "caboose: caboose check-image takes ") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", argv, code, out, errs)
		}
		if b, _ := os.ReadFile(log); len(b) != 0 {
			t.Errorf("docker was run: %q", b)
		}
	}
}
