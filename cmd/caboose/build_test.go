package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/version"
)

// probePasses makes the probe's `docker run --rm` print out; unlike
// probeRuns it answers nothing else, so a container's `run -d` still fails.
func probePasses(t *testing.T, out string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "probe.out")
	if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	return `[ "$1 $2" = "run --rm" ] && { cat "` + f + `"; exit 0; }`
}

// baseBuilds is a base that is there once built (or pulled), as baseID, and
// passes the check.
func baseBuilds(t *testing.T, ref string) string {
	return imageIDOf(ref, baseID) + "\n" + probePasses(t, probeComplete) + "\n" + imageAbsent
}

// probeMusl is an Alpine image with everything the musl build needs.
var probeMusl = strings.NewReplacer(
	"missing bash\n", "ok bash /bin/bash\n",
	"missing curl\n", "ok curl /usr/bin/curl\n",
	"missing tmux\n", "ok tmux /usr/bin/tmux\n",
	"missing git\n", "ok git /usr/bin/git 2.52.0\n",
	"missing libgcc no libgcc_s.so.1\n", "ok libgcc /usr/lib/libgcc_s.so.1\n",
	"missing libstdc++ no libstdc++.so.6\n", "ok libstdc++ /usr/lib/libstdc++.so.6\n",
	"missing ripgrep\n", "ok ripgrep /usr/bin/rg\n",
).Replace(probeAlpine)

// builds are the `docker build` lines in the log, each with the context it
// listed after it.
type build struct{ argv, context string }

func builds(t *testing.T, log string) []build {
	t.Helper()
	lines := dockerLog(t, log)
	var bs []build
	for i, l := range lines {
		if !strings.HasPrefix(l, "build ") {
			continue
		}
		var ctx []string
		for _, c := range lines[i+1:] {
			if strings.Contains(c, " ") || c == "" {
				break
			}
			ctx = append(ctx, c)
		}
		bs = append(bs, build{l, strings.Join(ctx, " ")})
	}
	return bs
}

// ctxDir is the context dir, the last word of a build line; it must be gone.
func ctxDir(t *testing.T, argv string) string {
	t.Helper()
	f := strings.Fields(argv)
	dir := f[len(f)-1]
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("build context %s was not cleaned up", dir)
	}
	return dir
}

func layerPrefix(image string) string {
	return "build -t " + image + " -f "
}

func layerArgs(base, kind, baseHash, platform string) string {
	// Every label, the empty ones too: a base built FROM a caboose image
	// passes its own on, and an unset one would read as the base's.
	return " --build-arg BASE=" + base +
		" --build-arg CABOOSE_UID=" + strconv.Itoa(os.Getuid()) +
		" --build-arg CABOOSE_GID=" + strconv.Itoa(os.Getgid()) +
		" --label " + assets.LabelVersion + "=" + version.Get().Version +
		" --label " + assets.LabelLayerHash + "=" + assets.LayerHash() +
		" --label " + assets.LabelBaseKind + "=" + kind +
		" --label " + assets.LabelBaseHash + "=" + baseHash +
		" --label " + assets.LabelBaseName + "=" + base +
		" --label " + assets.LabelBaseID + "=" + baseID +
		" --label " + assets.LabelPlatform + "=" + platform +
		" --label " + assets.LabelUID + "=" + strconv.Itoa(os.Getuid()) +
		" --label " + assets.LabelGID + "=" + strconv.Itoa(os.Getgid()) +
		" --label " + assets.LabelCompat + "=" + strconv.Itoa(assets.Compat)
}

// A dockerfile profile: its dir built as caboose-base:default with the
// extra args, checked by ID, then the layer FROM it, labelled with what it
// was built from. Only the layer's build writes to stdout, so -q still
// prints one image ID.
func TestBuildOnDockerfile(t *testing.T) {
	log := scriptedDocker(t, baseBuilds(t, "caboose-base:default"))
	home := sandboxEnv(t)
	code, out, errs := runIt("build", "--no-cache")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if out != "docker build stdout\n" {
		t.Errorf("stdout %q", out)
	}
	for _, w := range []string{
		"caboose: building the base image 'caboose-base:default' from ~/.caboose/envs/default/dockerfile/default/Dockerfile (dockerfile.default)\n" +
			"docker build stdout\ndocker build stderr\n",
		"caboose: checking base image 'caboose-base:default' (ba5e00000000) against caboose's requirements\n",
		"caboose: building the caboose layer on 'caboose-base:default' as 'caboose:default'\ndocker build stderr\n",
	} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	bs := builds(t, log)
	if len(bs) != 2 {
		t.Fatalf("builds: %v", bs)
	}
	wantBase := "build -t caboose-base:default --label " + assets.LabelVersion + "=" + version.Get().Version +
		" --no-cache " + imageDir(home)
	if bs[0].argv != wantBase || bs[0].context != "Dockerfile" {
		t.Errorf("base build %s (context %q)\nwant %s", bs[0].argv, bs[0].context, wantBase)
	}
	if strings.Contains(bs[0].argv, "UID") {
		t.Errorf("the base took the host's IDs: %s", bs[0].argv)
	}
	dir := ctxDir(t, bs[1].argv)
	want := layerPrefix("caboose:default") + dir + "/layer.Dockerfile" +
		layerArgs("caboose-base:default", "dockerfile", dockerfileHash(), "linux-arm64") + " --no-cache " + dir
	if bs[1].argv != want {
		t.Errorf("layer build\n%s\nwant\n%s", bs[1].argv, want)
	}
	if bs[1].context != "agent-bin entrypoint.sh layer-user.sh layer.Dockerfile shellrc.bash tmux.conf xdg-open.sh" {
		t.Errorf("layer context held %q", bs[1].context)
	}
	if _, err := os.Stat(filepath.Join(imageDir(home), "Dockerfile")); err != nil {
		t.Errorf("the image dir was removed: %v", err)
	}
	// The check ran on the base's ID, between the two builds.
	b, _ := os.ReadFile(log)
	s := string(b)
	check := strings.Index(s, "\nrun --rm --init --user 0:0 -e CABOOSE_PROBE_ROOT= --entrypoint /bin/sh "+baseID+" -c ")
	if check < 0 || check < strings.Index(s, "build -t caboose-base:default") || check > strings.Index(s, "build -t caboose:default -f") {
		t.Errorf("no check on the base's ID between the builds:\n%s", s)
	}
}

// --pull is for the base: a Dockerfile's build gets it, the layer never
// (its FROM is local only), and a ref is pulled first instead.
func TestBuildPull(t *testing.T) {
	log := scriptedDocker(t, baseBuilds(t, "caboose-base:default"))
	sandboxEnv(t)
	if code, _, errs := runIt("build", "--pull", "--progress=plain"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	bs := builds(t, log)
	if len(bs) != 2 || !strings.Contains(bs[0].argv, " --pull --progress=plain ") ||
		strings.Contains(bs[1].argv, "--pull") || !strings.Contains(bs[1].argv, " --progress=plain ") {
		t.Errorf("builds: %v", bs)
	}

	// A local user's base is pulled only because --pull asks.
	log = scriptedDocker(t, imageLabels("{}")+"\n"+baseBuilds(t, "node:22"))
	sandboxEnv(t, "CABOOSE_BASE_IMAGE", "node:22")
	code, _, errs := runIt("build", "--pull=true")
	if code != 1 || !strings.Contains(errs, "caboose: pulling base image 'node:22' (ref.default), as --pull asks\n") {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	for _, l := range dockerLog(t, log) {
		if strings.HasPrefix(l, "build ") {
			t.Errorf("built after a failed pull: %s", l)
		}
	}
}

// A ref: no base build, a pull only when it is not local, the
// layer FROM it with its name, and the platform the check found -- here
// musl's.
func TestBuildOnOwnBase(t *testing.T) {
	for _, local := range []bool{true, false} {
		name := map[bool]string{true: "local", false: "pulled"}[local]
		t.Run(name, func(t *testing.T) {
			present := imageMissingButPullable
			if local {
				present = imageLabels("{}")
			}
			log := scriptedDocker(t, present+"\n"+imageIDOf("alpine:3", baseID)+"\n"+probePasses(t, probeMusl)+"\n"+imageAbsent)
			sandboxEnv(t, "CABOOSE_BASE_IMAGE", "alpine:3")
			code, out, errs := runIt("build", "--no-cache")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			if out != "docker build stdout\n" {
				t.Errorf("stdout %q", out)
			}
			var pulled bool
			for _, l := range dockerLog(t, log) {
				pulled = pulled || l == "pull alpine:3"
			}
			if pulled == local {
				t.Errorf("pulled %v; stderr:\n%s", pulled, errs)
			}
			bs := builds(t, log)
			if len(bs) != 1 {
				t.Fatalf("builds: %v", bs)
			}
			dir := ctxDir(t, bs[0].argv)
			want := layerPrefix("caboose:default") + dir + "/layer.Dockerfile" +
				layerArgs("alpine:3", "ref", "", "linux-arm64-musl") + " --no-cache " + dir
			if bs[0].argv != want {
				t.Errorf("layer build\n%s\nwant\n%s", bs[0].argv, want)
			}
		})
	}
}

// A base that fails the check stops the build before the layer, with what
// is missing and where to see the rest.
func TestBuildRefusesAFailingBase(t *testing.T) {
	log := scriptedDocker(t, imageLabels("{}")+"\n"+imageIDOf("alpine", baseID)+"\n"+probePasses(t, probeAlpine))
	sandboxEnv(t, "CABOOSE_BASE_IMAGE", "alpine")
	code, out, errs := runIt("build")
	if code != 1 || out != "" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
	for _, w := range []string{
		"caboose: base image 'alpine' does not meet caboose's requirements:\n",
		"caboose:   bash: missing -- ",
		"caboose:   libgcc: missing (no libgcc_s.so.1) -- ",
		"caboose: 'caboose check-image alpine' prints the full checklist.\n",
		"caboose: not building the caboose layer on 'alpine'\n",
	} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	for _, l := range dockerLog(t, log) {
		if strings.HasPrefix(l, "build ") {
			t.Errorf("docker %s", l)
		}
	}
}

// A base whose ID cannot be read afterwards (a build that tagged nothing,
// say) is not checked or built on.
func TestBuildNeedsTheBaseID(t *testing.T) {
	log := scriptedDocker(t, probePasses(t, probeComplete))
	sandboxEnv(t)
	code, _, errs := runIt("build")
	if code != 1 || !strings.Contains(errs, "caboose: cannot read the ID of base image 'caboose-base:default'\n") {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	if bs := builds(t, log); len(bs) != 1 {
		t.Errorf("builds: %v", bs)
	}
}

// ^C during a step is noticed after it, whatever the step returned: here the
// check (with --init, which is what lets ^C stop the probe at all) comes back
// complete, as if nothing happened, and the build still stops there, as
// interrupted, before the layer. The fake sends the launcher -- this test
// process, which the build's signal.Notify keeps alive -- a SIGINT, as the
// terminal would along with docker.
func TestBuildInterruptedDuringTheCheck(t *testing.T) {
	f := filepath.Join(t.TempDir(), "probe.out")
	if err := os.WriteFile(f, []byte(probeComplete), 0o644); err != nil {
		t.Fatal(err)
	}
	log := scriptedDocker(t, imageIDOf("caboose-base:default", baseID)+"\n"+
		`[ "$1 $2 $3" = "run --rm --init" ] && { kill -INT $PPID; sleep 0.3; cat "`+f+`"; exit 0; }`+"\n"+imageAbsent)
	sandboxEnv(t)
	code, _, errs := runIt("build")
	if code != 130 || !strings.HasSuffix(errs, "caboose: interrupted\n") {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	if bs := builds(t, log); len(bs) != 1 {
		t.Errorf("builds after ^C: %v", bs)
	}
}

// Same after a base build that ^C cut short but that exited 0 regardless.
func TestBuildInterruptedDuringTheBase(t *testing.T) {
	log := scriptedDocker(t, `[ "$1 $2 $3" = "build -t caboose-base:default" ] && { kill -INT $PPID; sleep 0.3; exit 0; }`+"\n"+baseBuilds(t, "caboose-base:default"))
	sandboxEnv(t)
	code, _, errs := runIt("build")
	if code != 130 || !strings.HasSuffix(errs, "caboose: interrupted\n") {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	for _, l := range dockerLog(t, log) {
		if strings.HasPrefix(l, "run ") || strings.HasPrefix(l, "build -t caboose:default -f") {
			t.Errorf("went on after ^C: %s", l)
		}
	}
}

// A build's --platform reaches the pull and the check of a user's base, so
// what is checked is the variant the layer is built on -- and the layer's
// build gets it too.
func TestBuildPlatformReachesTheCheck(t *testing.T) {
	for _, form := range [][]string{{"--platform", "linux/amd64"}, {"--platform=linux/amd64"}} {
		log := scriptedDocker(t, imageMissingButPullable+"\n"+imageIDOf("alpine:3", baseID)+"\n"+
			`[ "$1 $2 $3 $4 $5" = "run --rm --init --platform linux/amd64" ] && { cat "`+probeFile(t, probeMusl)+`"; exit 0; }`+"\n"+imageAbsent)
		sandboxEnv(t, "CABOOSE_BASE_IMAGE", "alpine:3")
		code, _, errs := runIt(append([]string{"build"}, form...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", form, code, errs)
		}
		lines := dockerLog(t, log)
		if !slices.Contains(lines, "pull --platform linux/amd64 alpine:3") {
			t.Errorf("%v: no pull for the platform:\n%s", form, strings.Join(lines, "\n"))
		}
		bs := builds(t, log)
		if len(bs) != 1 || !strings.Contains(bs[0].argv, " "+strings.Join(form, " ")+" ") {
			t.Errorf("%v: builds %v", form, bs)
		}
	}
	// A Dockerfile: its build and the check both get it.
	log := scriptedDocker(t, imageIDOf("caboose-base:default", baseID)+"\n"+
		`[ "$1 $2 $3 $4 $5" = "run --rm --init --platform linux/amd64" ] && { cat "`+probeFile(t, probeComplete)+`"; exit 0; }`+"\n"+imageAbsent)
	sandboxEnv(t)
	if code, _, errs := runIt("build", "--platform", "linux/amd64"); code != 0 {
		t.Fatalf("dockerfile: exit %d: %s", code, errs)
	}
	if bs := builds(t, log); len(bs) != 2 || !strings.Contains(bs[0].argv, " --platform linux/amd64 ") {
		t.Errorf("dockerfile: builds %v", bs)
	}
	// apko builds for the engine: no --platform.
	log = scriptedDocker(t, "")
	sandboxEnv(t, "CABOOSE_APKO", "")
	if code, _, errs := runIt("build", "--platform", "linux/amd64"); code != 1 ||
		errs != "caboose: an apko image (apko.default) is built for the docker engine's architecture, so caboose build takes no --platform\n" {
		t.Errorf("apko: exit %d: %s", code, errs)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("apko: docker was run:\n%s", b)
	}
}

func probeFile(t *testing.T, out string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "probe.out")
	if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// The images' names are the launcher's: a -t or --tag among the extra args
// is refused before docker is asked anything.
func TestBuildRefusesATag(t *testing.T) {
	for _, arg := range []string{"-t", "-tfoo", "--tag", "--tag=foo"} {
		log := scriptedDocker(t, baseBuilds(t, "caboose-base:default"))
		sandboxEnv(t)
		code, _, errs := runIt("build", arg, "x")
		want := "--tag"
		if strings.HasPrefix(arg, "-t") {
			want = "-t"
		}
		if code != 1 || errs != "caboose: caboose build names its images after the environment itself (caboose:default), so it takes no "+want+"\n" {
			t.Errorf("%s: exit %d, stderr %q", arg, code, errs)
		}
		if b, _ := os.ReadFile(log); len(b) != 0 {
			t.Errorf("%s: docker was run:\n%s", arg, b)
		}
	}
}

// A ref naming the environment's own image -- in any spelling docker
// takes for the same image -- would build the layer over its own base, and
// stack one more on every rebuild: a build refuses it before docker is asked
// anything, and so does a launch. (The reviewer's repro.)
func TestImageIsItsOwnBase(t *testing.T) {
	for _, base := range []string{
		"caboose:default",
		"docker.io/library/caboose:default",
		"library/caboose:default",
	} {
		for _, argv := range [][]string{{"build"}, {"claude", "-p", "hi"}} {
			log := fakeDocker(t)
			home := sandboxEnv(t, "CABOOSE_BASE_IMAGE", base)
			inProject(t, home)
			code, _, errs := runIt(argv...)
			if code != 1 || !strings.Contains(errs, "caboose: image in [ref.default] ('"+base+"') is the environment's own image") {
				t.Errorf("%v %v: exit %d, stderr:\n%s", base, argv, code, errs)
			}
			for _, l := range dockerLog(t, log) {
				if strings.HasPrefix(l, "build ") || strings.HasPrefix(l, "run ") || strings.HasPrefix(l, "image ") {
					t.Errorf("%v %v: docker %s", base, argv, l)
				}
			}
		}
	}
	// A different tag of the same repository is a different image.
	log := scriptedDocker(t, imageLabels("{}")+"\n"+baseBuilds(t, "caboose:base"))
	sandboxEnv(t, "CABOOSE_BASE_IMAGE", "caboose:base")
	if code, _, errs := runIt("build"); code != 0 {
		t.Errorf("caboose:default on caboose:base: exit %d: %s", code, errs)
	}
	if bs := builds(t, log); len(bs) != 1 {
		t.Errorf("builds %v", bs)
	}
}

// A dockerfile profile's dir is dockerfile/<name> in the environment's
// directory, and may hold more than the Dockerfile: it is the context,
// built in place (not copied, not removed afterwards).
func TestBuildOnANamedDockerfileProfile(t *testing.T) {
	log := scriptedDocker(t, baseBuilds(t, "caboose-base:default"))
	home := sandboxEnv(t)
	dir := filepath.Join(home, ".caboose", "envs", "default", "dockerfile", "mine")
	writeConfig(t, home, "image = \"dockerfile.mine\"\n[dockerfile.mine]\n")
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, data := range map[string]string{"Dockerfile": "FROM ubuntu:26.04\nCOPY files/ /x/\n", "files/a": "a\n"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := assets.DirHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, _, errs := runIt("build")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(errs, "caboose: building the base image 'caboose-base:default' from ~/.caboose/envs/default/dockerfile/mine/Dockerfile (dockerfile.mine)\n") {
		t.Errorf("stderr:\n%s", errs)
	}
	bs := builds(t, log)
	if len(bs) != 2 {
		t.Fatalf("builds: %v", bs)
	}
	if want := "build -t caboose-base:default --label " + assets.LabelVersion + "=" + version.Get().Version + " " + dir; bs[0].argv != want || bs[0].context != "Dockerfile files" {
		t.Errorf("base build %s (context %q)\nwant %s", bs[0].argv, bs[0].context, want)
	}
	for _, label := range []string{
		" --label " + assets.LabelBaseKind + "=dockerfile ",
		" --label " + assets.LabelBaseHash + "=" + hash + " ",
		" --label " + assets.LabelBaseName + "=caboose-base:default ",
	} {
		if !strings.Contains(bs[1].argv, label) {
			t.Errorf("layer build lacks %q:\n%s", label, bs[1].argv)
		}
	}
}

// A dockerfile profile whose dir has no Dockerfile is not a guess: the
// build says what writes one.
func TestBuildNeedsADockerfile(t *testing.T) {
	log := scriptedDocker(t, "")
	home := sandboxEnv(t)
	if err := os.Remove(filepath.Join(imageDir(home), "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runIt("build")
	if code != 1 || !strings.Contains(errs, "caboose: image profile dockerfile.default builds from ~/.caboose/envs/default/dockerfile/default, which has no Dockerfile:\n") ||
		!strings.Contains(errs, "'caboose setup image' writes caboose's there") {
		t.Errorf("exit %d\n%s", code, errs)
	}
	if len(builds(t, log)) != 0 {
		t.Error("something was built")
	}
}

// apko.default, the default: its packages are resolved and built
// in-process, for the engine's architecture, which docker is asked first.
// Without an engine that answers, nothing is resolved.
func TestBuildApkoAsksTheEngine(t *testing.T) {
	log := scriptedDocker(t, "")
	home := sandboxEnv(t, "CABOOSE_APKO", "")
	code, _, errs := runIt("build")
	if code != 1 || !strings.Contains(errs, "caboose: cannot ask the docker engine its architecture") {
		t.Errorf("exit %d\n%s", code, errs)
	}
	if lines := dockerLog(t, log); len(lines) != 1 || lines[0] != "info --format {{.Architecture}}" {
		t.Errorf("docker: %q", lines)
	}
	if _, err := os.Stat(filepath.Join(home, ".caboose", "envs", "default", "apko-default.lock.json")); !os.IsNotExist(err) {
		t.Errorf("a lock was written: %v", err)
	}
}
