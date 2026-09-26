package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/version"
)

// Fake docker answers, for scriptedDocker. The image and container are named
// img and box by the tests that use them.
const (
	// imageAbsent: inspect fails as docker does for a missing image, while
	// `docker version` shows the engine answering, which is how ImageLabels
	// tells absent from unreachable. Another tag of the repository exists,
	// so `docker images -q` would list it: that must not count as present.
	imageAbsent = `case "$1" in
  version) exit 0 ;;
  images) echo 0123abcd; exit 0 ;;
  image) [ "$2" = inspect ] && { echo "Error: No such image: $5" >&2; exit 1; } ;;
esac`
	// containerRunning on image sha:1, which the tag points at unless
	// imageIDIs says otherwise.
	containerRunning = `case "$*" in
  "inspect --type=container -f {{.State.Status}} box") echo running; exit 0 ;;
  "inspect --type=container -f {{.Image}} box") echo sha:1; exit 0 ;;
esac`
)

// builtImage makes a `docker build -t img` leave image img with the labels
// json, as a real build would, for the inspect that reads them back.
func builtImage(json string) string {
	return `case "$*" in "build -t img "*) : > "$0.built" ;; esac
[ "$1 $2" = "image inspect" ] && [ -e "$0.built" ] && { echo '` + json + `'; exit 0; }`
}

// daemonDown fails every call as docker does with no engine to talk to.
const daemonDown = `echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2; exit 1`

// invalidRef is a healthy engine refusing the image name itself.
const invalidRef = `case "$1" in
  version) exit 0 ;;
  image) echo "invalid reference format: repository name (library/Img) must be lowercase" >&2; exit 1 ;;
esac`

// imageLabels makes `docker image inspect` print the labels as JSON.
func imageLabels(json string) string {
	return `[ "$1 $2" = "image inspect" ] && { echo '` + json + `'; exit 0; }`
}

func imageIDIs(id string) string {
	return `[ "$*" = "inspect --type=image -f {{.Id}} img" ] && { echo ` + id + `; exit 0; }`
}

// imageIDOf makes `docker inspect` report ref's image ID.
func imageIDOf(ref, id string) string {
	return `[ "$*" = "inspect --type=image -f {{.Id}} ` + ref + `" ] && { echo ` + id + `; exit 0; }`
}

func toJSON(m map[string]string) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// baseID is the ID the tests' base images have, unless one says otherwise.
const baseID = "sha256:ba5e000000000000000000000000000000000000000000000000000000000001"

// defaultLabels are what caboose build puts on an image built on the default
// base, img-base, by this launcher, with edit applied.
func defaultLabels(ver string, edit ...string) string {
	m := map[string]string{
		assets.LabelVersion:   ver,
		assets.LabelLayerHash: assets.LayerHash(),
		assets.LabelBaseKind:  assets.BaseKindDefault,
		assets.LabelBaseHash:  assets.BaseHash(),
		assets.LabelBaseName:  "img-base",
		assets.LabelBaseID:    baseID,
		assets.LabelPlatform:  "linux-arm64",
		assets.LabelUID:       strconv.Itoa(os.Getuid()),
		assets.LabelGID:       strconv.Itoa(os.Getgid()),
		assets.LabelCompat:    strconv.Itoa(assets.Compat),
	}
	return toJSON(edited(m, edit))
}

// byoLabels are defaultLabels for an image built on CABOOSE_BASE_IMAGE base:
// every label set, the base hash to "".
func byoLabels(ver, base string, edit ...string) string {
	m := map[string]string{
		assets.LabelVersion:   ver,
		assets.LabelLayerHash: assets.LayerHash(),
		assets.LabelBaseKind:  assets.BaseKindBYO,
		assets.LabelBaseHash:  "",
		assets.LabelBaseName:  base,
		assets.LabelBaseID:    baseID,
		assets.LabelPlatform:  "linux-arm64-musl",
		assets.LabelUID:       strconv.Itoa(os.Getuid()),
		assets.LabelGID:       strconv.Itoa(os.Getgid()),
		assets.LabelCompat:    strconv.Itoa(assets.Compat),
	}
	return toJSON(edited(m, edit))
}

// edited applies "label=value" pairs to m; "label=" deletes the label, and
// "label==" sets it to "".
func edited(m map[string]string, edit []string) map[string]string {
	for _, e := range edit {
		k, v, _ := strings.Cut(e, "=")
		switch v {
		case "":
			delete(m, k)
		case "=":
			m[k] = ""
		default:
			m[k] = v
		}
	}
	return m
}

const otherHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func versionHeader(context, base string) string {
	v := version.Get()
	commit := "unknown"
	if v.Commit != "" {
		commit = v.Commit[:min(12, len(v.Commit))]
		if v.Modified {
			commit += " (modified)"
		}
	}
	return "version   : " + v.Version + "\n" +
		"commit    : " + commit + "\n" +
		"built     : " + or(v.Date, "unknown") + "\n" +
		"install   : a development build; it does not update itself\n" +
		"context   : " + context[:12] + "\n" +
		"image     : img\n" +
		"base      : " + base + "\n"
}

type versionCase struct {
	name     string
	docker   []string
	code     int
	tail     string   // stdout after the header
	stderr   []string // each must appear
	noStderr bool
}

func runVersionCases(t *testing.T, header string, env []string, cases []versionCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := scriptedDocker(t, strings.Join(tc.docker, "\n"))
			// No repo root, and no data dir yet: caboose version needs neither,
			// and creates nothing.
			home := sandboxEnv(t, append([]string{"CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box",
				"CABOOSE_REPO_ROOT", "/does/not/exist"}, env...)...)
			code, out, errs := runIt("version")
			if code != tc.code {
				t.Errorf("exit %d, want %d; stderr:\n%s", code, tc.code, errs)
			}
			if out != header+tc.tail {
				t.Errorf("stdout:\n%s\nwant:\n%s%s", out, header, tc.tail)
			}
			for _, w := range tc.stderr {
				if !strings.Contains(errs, w) {
					t.Errorf("stderr lacks %q:\n%s", w, errs)
				}
			}
			if tc.noStderr && errs != "" {
				t.Errorf("stderr:\n%s", errs)
			}
			if _, err := os.Stat(filepath.Join(home, ".caboose")); !os.IsNotExist(err) {
				t.Error("caboose version created the data dir")
			}
			b, _ := os.ReadFile(log)
			for _, verb := range []string{"build ", "run ", "start ", "create ", "pull "} {
				if strings.Contains("\n"+string(b), "\n"+verb) {
					t.Errorf("caboose version ran docker %s:\n%s", verb, b)
				}
			}
		})
	}
}

func TestVersion(t *testing.T) {
	header := versionHeader(assets.ContextHash(), "img-base (the embedded Dockerfile, context "+assets.BaseHash()[:12]+")")
	current := imageLabels(defaultLabels("v9"))
	runVersionCases(t, header, nil, []versionCase{
		{"missing", []string{imageAbsent}, 0,
			"local     : not built yet\ncontainer : box (absent)\n",
			[]string{"run 'caboose build'"}, false},
		{"matches", []string{current}, 0,
			"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
		// The default base is identified by its hash: its ID is never asked,
		// and a different one does not matter.
		{"matches whatever the base's ID", []string{current, imageIDOf("img-base", "sha256:other")}, 0,
			"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
		{"another Dockerfile", []string{imageLabels(defaultLabels("v8", assets.LabelBaseHash+"="+otherHash))}, 0,
			"local     : differs (built by v8, from a different Dockerfile than this launcher embeds (base context 0123456789ab, this launcher's " +
				assets.BaseHash()[:12] + "))\ncontainer : box (absent)\n",
			[]string{"the next launch that creates the container rebuilds it", "'caboose restart' moves a running container onto it"}, false},
		{"another layer", []string{imageLabels(defaultLabels("v8", assets.LabelLayerHash+"="+otherHash))}, 0,
			"local     : differs (built by v8, with a different layer than this launcher embeds (layer context 0123456789ab, this launcher's " +
				assets.LayerHash()[:12] + "))\ncontainer : box (absent)\n",
			[]string{"'caboose build'"}, false},
		{"built on a user's base", []string{imageLabels(byoLabels("v9", "node:22"))}, 0,
			"local     : differs (built by v9, on CABOOSE_BASE_IMAGE 'node:22', not on the embedded Dockerfile's base)\ncontainer : box (absent)\n",
			[]string{"'caboose build'"}, false},
		{"unlabelled", []string{imageLabels("null")}, 0,
			"local     : unlabelled (not built by caboose build)\ncontainer : box (absent)\n",
			[]string{"'caboose build' to build it, replacing what the name holds now"}, false},
		// Hashes but no kind of base: not the layer caboose build makes.
		{"no base kind", []string{imageLabels(defaultLabels("v9", assets.LabelBaseKind+"="))}, 0,
			"local     : unlabelled (not built by caboose build)\ncontainer : box (absent)\n", nil, false},
		{"docker unreachable", []string{daemonDown}, 1,
			"local     : unknown (docker did not answer)\n",
			[]string{"caboose: cannot inspect image 'img': is the docker engine running? " +
				"(docker image inspect img: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. " +
				"Is the docker daemon running?)\n"}, false},
		{"invalid image name", []string{invalidRef}, 1,
			"local     : unknown (docker could not look it up)\n",
			[]string{"caboose: cannot inspect image 'img': docker image inspect img: invalid reference format: " +
				"repository name (library/Img) must be lowercase\n"}, false},
		{"container on the current image",
			[]string{current, containerRunning, imageIDIs("sha:1")}, 0,
			"local     : matches (built by v9)\ncontainer : box (running, on the current image)\n", nil, true},
		{"container on an older image",
			[]string{current, containerRunning, imageIDIs("sha:2")}, 0,
			"local     : matches (built by v9)\ncontainer : box (running, on an older image)\n",
			[]string{"'caboose restart' to move it onto the current image"}, false},
	})
}

// On CABOOSE_BASE_IMAGE the base's ID is part of the image's identity, and
// the embedded Dockerfile is not: an image built by this launcher on it
// carries no base hash, so none can differ.
func TestVersionOnOwnBase(t *testing.T) {
	env := []string{"CABOOSE_BASE_IMAGE", "node:22"}
	header := versionHeader(assets.LayerHash(), "node:22 (CABOOSE_BASE_IMAGE, ba5e00000000)")
	current := imageLabels(byoLabels("v9", "node:22"))
	here := imageIDOf("node:22", baseID)
	runVersionCases(t, header, env, []versionCase{
		{"matches", []string{current, here}, 0,
			"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
		{"another layer", []string{imageLabels(byoLabels("v8", "node:22", assets.LabelLayerHash+"="+otherHash)), here}, 0,
			"local     : differs (built by v8, with a different layer than this launcher embeds (layer context 0123456789ab, this launcher's " +
				assets.LayerHash()[:12] + "))\ncontainer : box (absent)\n", nil, false},
		{"another base", []string{imageLabels(byoLabels("v9", "node:20")), here}, 0,
			"local     : differs (built by v9, on 'node:20', not on CABOOSE_BASE_IMAGE 'node:22')\ncontainer : box (absent)\n", nil, false},
		{"the default base", []string{imageLabels(defaultLabels("v9")), here}, 0,
			"local     : differs (built by v9, on the embedded Dockerfile's base, not on CABOOSE_BASE_IMAGE 'node:22')\ncontainer : box (absent)\n", nil, false},
	})
	// A pull (or a rebuild) of the base since: a new ID.
	runVersionCases(t, versionHeader(assets.LayerHash(), "node:22 (CABOOSE_BASE_IMAGE, 999900000000)"), env, []versionCase{
		{"the base has changed", []string{current, imageIDOf("node:22", "sha256:9999000000000000")}, 0,
			"local     : differs (built by v9, on 'node:22' as ba5e00000000, which it no longer names (now 999900000000))\ncontainer : box (absent)\n",
			[]string{"'caboose build'"}, false},
	})
	// The base gone from the local store: nothing to compare its ID with.
	runVersionCases(t, versionHeader(assets.LayerHash(), "node:22 (CABOOSE_BASE_IMAGE, not in the local store)"), env, []versionCase{
		{"base not local", []string{current}, 0,
			"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
	})
}

// A user's base built FROM a caboose image -- the default base, or a whole
// sandbox -- inherits its labels, the base hash among them. The layer sets
// every label itself (the base hash to "", the kind to byo), so what the
// base passed on cannot make the image read as on the default base: it
// matches, rather than being stale for good. (The reviewer's repro.)
func TestVersionBaseBuiltFromCaboose(t *testing.T) {
	env := []string{"CABOOSE_BASE_IMAGE", "mine"}
	header := versionHeader(assets.LayerHash(), "mine (CABOOSE_BASE_IMAGE, ba5e00000000)")
	here := imageIDOf("mine", baseID)
	runVersionCases(t, header, env, []versionCase{
		// As a build by this launcher labels it: an empty base hash.
		{"built now", []string{imageLabels(byoLabels("v9", "mine")), here}, 0,
			"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
		// Were an inherited hash to show through, the kind still decides.
		{"inherited hash", []string{imageLabels(byoLabels("v9", "mine", assets.LabelBaseHash+"="+assets.BaseHash())), here}, 0,
			"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
	})
	// caboose's own base as CABOOSE_BASE_IMAGE: a separate image from
	// CABOOSE_IMAGE, so allowed, and current once built on.
	runVersionCases(t, versionHeader(assets.LayerHash(), "img-base (CABOOSE_BASE_IMAGE, ba5e00000000)"),
		[]string{"CABOOSE_BASE_IMAGE", "img-base"}, []versionCase{
			{"caboose's base", []string{imageLabels(byoLabels("v9", "img-base")), imageIDOf("img-base", baseID)}, 0,
				"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
		})
	// The default kind with no base hash is no image of this launcher's base.
	runVersionCases(t, versionHeader(assets.ContextHash(), "img-base (the embedded Dockerfile, context "+assets.BaseHash()[:12]+")"), nil,
		[]versionCase{
			{"default kind, empty hash", []string{imageLabels(defaultLabels("v8", assets.LabelBaseHash+"=="))}, 0,
				"local     : differs (built by v8, from a different Dockerfile than this launcher embeds (base context , this launcher's " +
					assets.BaseHash()[:12] + "))\ncontainer : box (absent)\n", nil, false},
		})
}

// Base names compare as docker resolves them: node:22 is
// docker.io/library/node:22, and alpine is alpine:latest.
func TestVersionBaseNamesNormalised(t *testing.T) {
	for _, tc := range []struct{ env, label string }{
		{"node:22", "docker.io/library/node:22"},
		{"docker.io/node:22", "node:22"},
		{"alpine", "alpine:latest"},
	} {
		runVersionCases(t, versionHeader(assets.LayerHash(), tc.env+" (CABOOSE_BASE_IMAGE, ba5e00000000)"),
			[]string{"CABOOSE_BASE_IMAGE", tc.env}, []versionCase{
				{tc.env, []string{imageLabels(byoLabels("v9", tc.label)), imageIDOf(tc.env, baseID)}, 0,
					"local     : matches (built by v9)\ncontainer : box (absent)\n", nil, true},
			})
	}
}

// The layer's agent user has the IDs of the host user it was built for: an
// image built for another is stale, whichever of the two differs.
func TestVersionOtherHostUser(t *testing.T) {
	header := versionHeader(assets.ContextHash(), "img-base (the embedded Dockerfile, context "+assets.BaseHash()[:12]+")")
	me := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	runVersionCases(t, header, nil, []versionCase{
		{"another uid", []string{imageLabels(defaultLabels("v9", assets.LabelUID+"=4242"))}, 0,
			"local     : differs (built by v9, for another host user (uid 4242, gid " + strconv.Itoa(os.Getgid()) +
				"; this one is " + me + "))\ncontainer : box (absent)\n", nil, false},
		{"another gid", []string{imageLabels(defaultLabels("v9", assets.LabelGID+"=4343"))}, 0,
			"local     : differs (built by v9, for another host user (uid " + strconv.Itoa(os.Getuid()) +
				", gid 4343; this one is " + me + "))\ncontainer : box (absent)\n", nil, false},
	})
}

// CABOOSE_BASE_IMAGE naming CABOOSE_IMAGE itself, in any spelling: said by
// caboose version, which still answers. (A build and a launch refuse it: see
// TestImageIsItsOwnBase.)
func TestVersionImageIsItsOwnBase(t *testing.T) {
	env := []string{"CABOOSE_BASE_IMAGE", "docker.io/library/img:latest"}
	header := versionHeader(assets.LayerHash(), "docker.io/library/img:latest (CABOOSE_BASE_IMAGE, 1a7e00000000)")
	runVersionCases(t, header, env, []versionCase{
		{"self", []string{imageLabels(byoLabels("v9", "img")), imageIDOf("docker.io/library/img:latest", "sha256:1a7e000000000000")}, 0,
			"local     : differs (built by v9, on 'docker.io/library/img:latest' as ba5e00000000, which it no longer names (now 1a7e00000000))\ncontainer : box (absent)\n",
			[]string{"caboose: CABOOSE_BASE_IMAGE ('docker.io/library/img:latest') names the same image as CABOOSE_IMAGE ('img')"}, false},
	})
}

func TestVersionRejectsArguments(t *testing.T) {
	log := fakeDocker(t)
	sandboxEnv(t)
	code, out, errs := runIt("version", "--json")
	if code != 2 || out != "" || errs != "caboose: version takes no arguments (got --json)\n  see 'caboose help version'\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("docker was run: %q", b)
	}
}

func TestStatusShowsLauncherVersion(t *testing.T) {
	fakeDocker(t)
	sandboxEnv(t)
	_, out, _ := runIt("status")
	if !strings.Contains(out, "\nversion   : "+version.Get().Version+"\n") {
		t.Errorf("stdout:\n%s", out)
	}
}

// inProject puts the test in a project under the sandbox's repo root, for
// a launch to get as far as creating the container.
func inProject(t *testing.T, home string) {
	t.Helper()
	root, err := config.Physical(filepath.Join(home, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "proj")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
}

func dockerLog(t *testing.T, path string) []string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The first launch with no image builds it -- base, check, layer -- then
// goes on to create the container (whose `docker run` the fake fails).
// Nothing of the build may reach stdout: that is claude's, and may be piped
// somewhere.
func TestFirstLaunchBuildsTheImage(t *testing.T) {
	log := scriptedDocker(t, builtImage(defaultLabels("v9"))+"\n"+imageAbsent+"\n"+baseBuilds(t, "img-base"))
	home := sandboxEnv(t, "CABOOSE_IMAGE", "img")
	inProject(t, home)
	code, out, errs := runIt("claude", "-p", "hi")
	if code != 1 {
		t.Errorf("exit %d: %s", code, errs)
	}
	if out != "" {
		t.Errorf("stdout %q", out)
	}
	for _, w := range []string{
		"caboose: no image 'img' yet — building it from the Dockerfile embedded in this launcher\n",
		"caboose: building the base image 'img-base' from the Dockerfile embedded in this launcher\n",
		"docker build stdout\ndocker build stderr\ncaboose: checking base image 'img-base' (ba5e00000000)",
		"caboose: building the caboose layer on 'img-base' as 'img'\ndocker build stdout\ndocker build stderr\n",
		"caboose: built image 'img'\n",
	} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	if strings.Contains(errs, "docker pull") {
		t.Errorf("a local image name got the registry note:\n%s", errs)
	}
	var order []string
	for _, l := range dockerLog(t, log) {
		switch {
		case strings.HasPrefix(l, "build -t img-base "):
			order = append(order, "base")
		case strings.HasPrefix(l, "run --rm "):
			order = append(order, "check")
		case strings.HasPrefix(l, "build -t img "):
			order = append(order, "layer")
		case strings.HasPrefix(l, "run -d "):
			order = append(order, "run")
		}
	}
	if got := strings.Join(order, " "); got != "base check layer run" {
		t.Errorf("docker calls: %s", got)
	}
	// The image just built is mounted its platform's Claude Code dir.
	if _, err := os.Stat(filepath.Join(home, ".caboose/envs/default/data/local/linux-arm64/share/claude")); err != nil {
		t.Errorf("no platform dir for the image: %v", err)
	}
}

// On CABOOSE_BASE_IMAGE the first launch says what it builds on, pulls the
// base when it is not local -- all on stderr -- and builds no base.
func TestFirstLaunchOnOwnBase(t *testing.T) {
	log := scriptedDocker(t, builtImage(byoLabels("v9", "node:22"))+"\n"+imageMissingButPullable+"\n"+imageIDOf("node:22", baseID)+"\n"+
		probePasses(t, probeComplete)+"\n"+imageAbsent)
	home := sandboxEnv(t, "CABOOSE_IMAGE", "img", "CABOOSE_BASE_IMAGE", "node:22")
	inProject(t, home)
	code, out, errs := runIt("claude", "-p", "hi")
	if code != 1 || out != "" {
		t.Errorf("exit %d, stdout %q: %s", code, out, errs)
	}
	for _, w := range []string{
		"caboose: no image 'img' yet — building the caboose layer on 'node:22' (CABOOSE_BASE_IMAGE)\n",
		"caboose: base image 'node:22' (CABOOSE_BASE_IMAGE) is not in the local store; pulling it (docker's output follows)\n",
		"pull progress on stdout\npull progress on stderr\n",
		"caboose: building the caboose layer on 'node:22' as 'img'\n",
	} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	var order []string
	for _, l := range dockerLog(t, log) {
		switch {
		case strings.HasPrefix(l, "pull "):
			order = append(order, l)
		case strings.HasPrefix(l, "build -t img "):
			order = append(order, "layer")
		case strings.HasPrefix(l, "build "):
			order = append(order, "other build: "+l)
		case strings.HasPrefix(l, "run -d "):
			order = append(order, "run")
		}
	}
	if got := strings.Join(order, ", "); got != "pull node:22, layer, run" {
		t.Errorf("docker calls: %s", got)
	}
}

func TestFirstLaunchRegistryImageNote(t *testing.T) {
	scriptedDocker(t, imageAbsent)
	home := sandboxEnv(t, "CABOOSE_IMAGE", "ghcr.io/x/y")
	inProject(t, home)
	_, _, errs := runIt("claude", "-p", "hi")
	if !strings.Contains(errs, "'ghcr.io/x/y' is not in the local store; to use the registry's image instead, ^C and 'docker pull' it.") {
		t.Errorf("stderr:\n%s", errs)
	}
}

func TestNoAutoBuild(t *testing.T) {
	log := scriptedDocker(t, imageAbsent)
	home := sandboxEnv(t, "CABOOSE_IMAGE", "img", "CABOOSE_NO_AUTO_BUILD", "1")
	inProject(t, home)
	code, _, errs := runIt("claude", "-p", "hi")
	want := "caboose: image 'img' not found — run 'caboose build' first\n" +
		"       (CABOOSE_NO_AUTO_BUILD is set, so a launch does not build it)\n"
	if code != 1 || !strings.HasSuffix(errs, want) || strings.Contains(errs, "building") {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	for _, l := range dockerLog(t, log) {
		if strings.HasPrefix(l, "build ") || strings.HasPrefix(l, "run ") {
			t.Errorf("docker %s", l)
		}
	}
}

// Which commands may build a missing image: the ones that open a session
// or recreate the container do, as a first launch does; the ones that only
// look after a container say what to do instead of building for minutes.
func TestWhichCommandsBuild(t *testing.T) {
	for _, tc := range []struct {
		argv  []string
		build bool
	}{
		{[]string{"claude", "-p", "hi"}, true},
		{[]string{"shell", "-c", "true"}, true},
		{[]string{"restart"}, true},
		{[]string{"logs", "--tail", "1"}, false},
		{[]string{"prune"}, false},
	} {
		t.Run(tc.argv[0], func(t *testing.T) {
			log := scriptedDocker(t, imageAbsent)
			home := sandboxEnv(t, "CABOOSE_IMAGE", "img")
			inProject(t, home)
			code, _, errs := runIt(tc.argv...)
			if code == 0 {
				t.Error("the fake docker let it succeed")
			}
			built := false
			for _, l := range dockerLog(t, log) {
				built = built || strings.HasPrefix(l, "build ")
				if !tc.build && strings.HasPrefix(l, "run ") {
					t.Errorf("docker %s", l)
				}
			}
			if built != tc.build {
				t.Errorf("built %v, want %v; stderr:\n%s", built, tc.build, errs)
			}
			if !tc.build && !strings.Contains(errs, "caboose: no container yet, and no image 'img' to create it from — run caboose in a project\n") {
				t.Errorf("stderr:\n%s", errs)
			}
		})
	}
}

// caboose shell from outside the repo root fails before building or creating
// anything, as the default launch does.
func TestShellChecksCwdFirst(t *testing.T) {
	log := scriptedDocker(t, imageAbsent)
	home := sandboxEnv(t, "CABOOSE_IMAGE", "img")
	t.Chdir(t.TempDir())
	code, _, errs := runIt("shell")
	if code != 1 || !strings.Contains(errs, "is outside the mounted repo root") {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	for _, l := range dockerLog(t, log) {
		if strings.HasPrefix(l, "build ") || strings.HasPrefix(l, "run ") || strings.HasPrefix(l, "image ") {
			t.Errorf("docker %s", l)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".caboose")); !os.IsNotExist(err) {
		t.Error("the data dir was created")
	}
}

// An unreachable docker is not a missing image, and must not start a build;
// nor must a name docker rejects outright, which gets docker's reason, not
// a question about the engine.
func TestLaunchWithDockerUnreachable(t *testing.T) {
	for _, tc := range []struct{ name, docker, want string }{
		{"silent", "", "caboose: cannot inspect image 'img': is the docker engine running?"},
		{"daemon down", daemonDown, "caboose: cannot inspect image 'img': is the docker engine running? " +
			"(docker image inspect img: Cannot connect to the Docker daemon"},
		{"invalid name", invalidRef, "caboose: cannot inspect image 'img': docker image inspect img: invalid reference format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := scriptedDocker(t, tc.docker)
			home := sandboxEnv(t, "CABOOSE_IMAGE", "img")
			inProject(t, home)
			code, _, errs := runIt("claude", "-p", "hi")
			if code != 1 || !strings.Contains(errs, tc.want) {
				t.Errorf("exit %d, stderr:\n%s", code, errs)
			}
			if tc.docker == invalidRef && strings.Contains(errs, "engine running") {
				t.Errorf("an invalid name blamed the engine:\n%s", errs)
			}
			for _, l := range dockerLog(t, log) {
				if strings.HasPrefix(l, "build ") || strings.HasPrefix(l, "run ") {
					t.Errorf("docker %s", l)
				}
			}
		})
	}
}

const staleWarning = "caboose: image 'img' is out of date: built by "

// A launch warns about a stale image, whether it creates the container or
// finds one, and stays quiet for a current one, an unlabelled one, or when
// docker does not answer. The running case gets no further than readiness
// (CABOOSE_READY_TIMEOUT=0, and every exec fails), which is after the check.
// It costs one image inspect, plus, on CABOOSE_BASE_IMAGE, one of the base's
// ID -- and that only when nothing else already says stale.
func TestLaunchWarnsAboutStaleImage(t *testing.T) {
	current := imageLabels(defaultLabels("v9"))
	byoCurrent := imageLabels(byoLabels("v9", "node:22"))
	cases := []struct {
		name   string
		byo    bool
		docker []string
		warn   string // "" for none
		baseID int    // inspects of the base's ID
	}{
		{"creating, stale", false, []string{imageLabels(defaultLabels("v8", assets.LabelBaseHash+"="+otherHash))},
			"built by v8, from a different Dockerfile than this launcher embeds", 0},
		{"creating, current", false, []string{current}, "", 0},
		{"creating, unlabelled", false, []string{imageLabels("null")}, "", 0},
		{"running, stale", false, []string{containerRunning, imageLabels(defaultLabels("v8", assets.LabelLayerHash+"="+otherHash))},
			"with a different layer", 0},
		{"running, current", false, []string{containerRunning, current}, "", 0},
		{"running, unlabelled", false, []string{containerRunning, imageLabels("null")}, "", 0},
		{"running, docker errors", false, []string{containerRunning}, "", 0},
		{"own base, current", true, []string{containerRunning, byoCurrent, imageIDOf("node:22", baseID)}, "", 1},
		{"own base, pulled since", true, []string{containerRunning, byoCurrent, imageIDOf("node:22", "sha256:9999000000000000")},
			"on 'node:22' as ba5e00000000, which it no longer names", 1},
		{"own base, not local", true, []string{containerRunning, byoCurrent}, "", 1},
		{"own base, stale layer", true, []string{containerRunning, imageLabels(byoLabels("v8", "node:22", assets.LabelLayerHash+"="+otherHash))},
			"with a different layer", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := scriptedDocker(t, strings.Join(tc.docker, "\n"))
			// CABOOSE_NO_AUTO_BUILD: the warning, not the rebuild a
			// creation makes otherwise (TestLaunchRebuildsForDrift).
			env := []string{"CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box", "CABOOSE_READY_TIMEOUT", "0",
				"CABOOSE_NO_AUTO_BUILD", "1"}
			if tc.byo {
				env = append(env, "CABOOSE_BASE_IMAGE", "node:22")
			}
			home := sandboxEnv(t, env...)
			inProject(t, home)
			code, _, errs := runIt("claude", "-p", "hi")
			if code == 0 {
				t.Error("the fake docker let a launch succeed")
			}
			warned := strings.Contains(errs, staleWarning)
			if warned != (tc.warn != "") || !strings.Contains(errs, tc.warn) {
				t.Errorf("warned %v, want %q; stderr:\n%s", warned, tc.warn, errs)
			}
			if warned && !strings.Contains(errs, "run 'caboose build', then 'caboose restart'") {
				t.Errorf("no advice in:\n%s", errs)
			}
			inspects, baseIDs := 0, 0
			for _, l := range dockerLog(t, log) {
				if strings.HasPrefix(l, "image inspect ") {
					inspects++
				}
				if l == "inspect --type=image -f {{.Id}} node:22" {
					baseIDs++
				}
				if strings.HasPrefix(l, "build ") || strings.HasPrefix(l, "pull ") {
					t.Errorf("an existing image was rebuilt: %s", l)
				}
			}
			if inspects != 1 || baseIDs != tc.baseID {
				t.Errorf("%d image inspects, want 1; %d of the base's ID, want %d", inspects, baseIDs, tc.baseID)
			}
		})
	}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// A launch that creates the container on an image built on another base
// than the configured one rebuilds it: CABOOSE_BASE_IMAGE set, changed or
// unset since is a change the user made, not drift. The same build as a
// first launch's, with a note naming both bases, then the container.
func TestLaunchRebuildsForAnotherBase(t *testing.T) {
	for _, tc := range []struct {
		name, base, labels, why string
	}{
		{"back to the default base", "", byoLabels("v9", "node:22"),
			"image 'img' was built on 'node:22'; CABOOSE_BASE_IMAGE is unset now, which means the embedded Dockerfile's base — rebuilding\n" +
				"caboose: building it from the Dockerfile embedded in this launcher; a few minutes"},
		{"onto a user's base", "node:22", defaultLabels("v9"),
			"image 'img' was built on the embedded Dockerfile's base; CABOOSE_BASE_IMAGE now names 'node:22' — rebuilding\n" +
				"caboose: building the caboose layer on 'node:22' (CABOOSE_BASE_IMAGE); a few minutes"},
		{"onto another user's base", "node:22", byoLabels("v9", "node:20"),
			"image 'img' was built on 'node:20'; CABOOSE_BASE_IMAGE now names 'node:22' — rebuilding\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := baseBuilds(t, "img-base")
			if tc.base != "" {
				build = imageIDOf(tc.base, baseID) + "\n" + probePasses(t, probeComplete)
			}
			log := scriptedDocker(t, imageLabels(tc.labels)+"\n"+build)
			env := []string{"CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box"}
			if tc.base != "" {
				env = append(env, "CABOOSE_BASE_IMAGE", tc.base)
			}
			home := sandboxEnv(t, env...)
			inProject(t, home)
			code, out, errs := runIt("claude", "-p", "hi")
			if code != 1 || out != "" {
				t.Errorf("exit %d, stdout %q", code, out)
			}
			if !strings.Contains(errs, "caboose: "+tc.why) || !strings.Contains(errs, "caboose: built image 'img'\n") {
				t.Errorf("stderr lacks %q:\n%s", tc.why, errs)
			}
			if strings.Contains(errs, staleWarning) {
				t.Errorf("warned as well as rebuilt:\n%s", errs)
			}
			var order []string
			for _, l := range dockerLog(t, log) {
				switch {
				case strings.HasPrefix(l, "build -t img-base "):
					order = append(order, "base")
				case strings.HasPrefix(l, "build -t img -f "):
					if want := " --build-arg BASE=" + or(tc.base, "img-base") + " "; !strings.Contains(l, want) {
						t.Errorf("layer build lacks %q: %s", want, l)
					}
					order = append(order, "layer")
				case strings.HasPrefix(l, "run -d "):
					order = append(order, "run")
				}
			}
			want := "layer run"
			if tc.base == "" {
				want = "base layer run"
			}
			if got := strings.Join(order, " "); got != want {
				t.Errorf("docker calls: %s, want %s", got, want)
			}
		})
	}
}

// Every staleness a rebuild cures is rebuilt for when the container is
// created -- a launcher that updated itself leaves the image it built
// before stale -- unless CABOOSE_NO_AUTO_BUILD says a launch builds
// nothing, when it stays a warning with the advice to build. With a
// container to keep, it is a warning either way: the running one is never
// moved off its image, and the advice is that a caboose restart alone
// rebuilds. Base names compare normalised, so a respelling is no change.
func TestLaunchRebuildsForDrift(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		docker     []string
		warn       string // "" for none
	}{
		{"same base, new ID", "node:22", []string{imageLabels(byoLabels("v9", "node:22")), imageIDOf("node:22", "sha256:9999000000000000")},
			"on 'node:22' as ba5e00000000, which it no longer names"},
		{"another layer", "node:22", []string{imageLabels(byoLabels("v8", "node:22", assets.LabelLayerHash+"="+otherHash))},
			"with a different layer"},
		{"another Dockerfile", "", []string{imageLabels(defaultLabels("v8", assets.LabelBaseHash+"="+otherHash))},
			"from a different Dockerfile"},
		{"another host user", "", []string{imageLabels(defaultLabels("v9", assets.LabelUID+"=4242"))},
			"for another host user"},
		{"the base respelled", "docker.io/library/node:22", []string{imageLabels(byoLabels("v9", "node:22")),
			imageIDOf("docker.io/library/node:22", baseID)}, ""},
		{"running, another layer", "", []string{containerRunning, imageLabels(defaultLabels("v8", assets.LabelLayerHash+"="+otherHash))},
			"with a different layer"},
		{"running, another base", "node:22", []string{containerRunning, imageLabels(byoLabels("v9", "node:20"))},
			"on 'node:20', not on CABOOSE_BASE_IMAGE 'node:22'"},
	} {
		for _, noBuild := range []bool{false, true} {
			name := tc.name
			if noBuild {
				name += ", CABOOSE_NO_AUTO_BUILD"
			}
			t.Run(name, func(t *testing.T) {
				log := scriptedDocker(t, strings.Join(tc.docker, "\n"))
				env := []string{"CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box", "CABOOSE_READY_TIMEOUT", "0"}
				if tc.base != "" {
					env = append(env, "CABOOSE_BASE_IMAGE", tc.base)
				}
				if noBuild {
					env = append(env, "CABOOSE_NO_AUTO_BUILD", "1")
				}
				home := sandboxEnv(t, env...)
				inProject(t, home)
				_, _, errs := runIt("claude", "-p", "hi")
				running := strings.HasPrefix(tc.name, "running")
				switched := tc.name == "running, another base"
				// On a user's base the fake docker fails the rebuild before
				// any docker build: what counts is that one was started.
				built := strings.Contains(errs, "rebuilding")
				for _, l := range dockerLog(t, log) {
					built = built || strings.HasPrefix(l, "build ") || strings.HasPrefix(l, "pull ")
				}
				rebuilds := tc.warn != "" && !running && !noBuild
				if built != rebuilds {
					t.Fatalf("rebuilt %v, want %v; stderr:\n%s", built, rebuilds, errs)
				}
				if rebuilds {
					if !strings.Contains(errs, "is out of date (built by ") || !strings.Contains(errs, tc.warn) {
						t.Errorf("stderr:\n%s", errs)
					}
					return
				}
				if warned := strings.Contains(errs, staleWarning); warned != (tc.warn != "") || !strings.Contains(errs, tc.warn) {
					t.Errorf("warned %v, want %q; stderr:\n%s", warned, tc.warn, errs)
				}
				if tc.warn == "" {
					return
				}
				advice := "run 'caboose build', then 'caboose restart'"
				switch {
				case noBuild:
				case switched:
					advice = "run 'caboose restart' to rebuild it on the configured base and move onto it"
				default:
					advice = "the next 'caboose restart' rebuilds it and moves onto it"
				}
				if !strings.Contains(errs, advice) {
					t.Errorf("stderr lacks %q:\n%s", advice, errs)
				}
			})
		}
	}
}

// With CABOOSE_NO_AUTO_BUILD, or for a command that does not build, a
// changed base refuses to create the container instead, naming both bases:
// creating it on the old one is the mistake the rebuild is for.
func TestChangedBaseWithoutBuilding(t *testing.T) {
	why := "caboose: image 'img' was built on 'node:20'; CABOOSE_BASE_IMAGE now names 'node:22'"
	for _, tc := range []struct {
		name  string
		env   []string
		argv  []string
		wants string
	}{
		{"CABOOSE_NO_AUTO_BUILD", []string{"CABOOSE_NO_AUTO_BUILD", "1"}, []string{"claude", "-p", "hi"},
			why + " —\n       run 'caboose build' to rebuild it on that base first\n" +
				"       (CABOOSE_NO_AUTO_BUILD is set, so a launch does not rebuild it)\n"},
		{"logs", nil, []string{"logs"},
			why + ",\n       and there is no container yet — run caboose in a project (it rebuilds the image first)\n" +
				"       or 'caboose build'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := scriptedDocker(t, imageLabels(byoLabels("v9", "node:20")))
			home := sandboxEnv(t, append([]string{"CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box",
				"CABOOSE_BASE_IMAGE", "node:22"}, tc.env...)...)
			inProject(t, home)
			code, _, errs := runIt(tc.argv...)
			if code != 1 || !strings.HasSuffix(errs, tc.wants) {
				t.Errorf("exit %d, stderr:\n%s\nwant suffix:\n%s", code, errs, tc.wants)
			}
			for _, l := range dockerLog(t, log) {
				if strings.HasPrefix(l, "build ") || strings.HasPrefix(l, "run ") {
					t.Errorf("docker %s", l)
				}
			}
		})
	}
}

// caboose version's advice for an image on another base: a caboose restart
// rebuilds it, unless CABOOSE_NO_AUTO_BUILD says a launch builds nothing.
func TestVersionChangedBaseAdvice(t *testing.T) {
	header := versionHeader(assets.LayerHash(), "node:22 (CABOOSE_BASE_IMAGE, ba5e00000000)")
	docker := []string{imageLabels(byoLabels("v9", "node:20")), imageIDOf("node:22", baseID)}
	tail := "local     : differs (built by v9, on 'node:20', not on CABOOSE_BASE_IMAGE 'node:22')\ncontainer : box (absent)\n"
	runVersionCases(t, header, []string{"CABOOSE_BASE_IMAGE", "node:22"}, []versionCase{
		{"rebuilt by a launch", docker, 0, tail, []string{
			"caboose: the next launch that creates the container rebuilds it on the configured base, so\n" +
				"caboose: 'caboose restart' moves a running container onto it (this kills running sessions);\n" +
				"caboose: 'caboose build' rebuilds it now, without touching the container.\n"}, false},
	})
	runVersionCases(t, header, []string{"CABOOSE_BASE_IMAGE", "node:22", "CABOOSE_NO_AUTO_BUILD", "1"}, []versionCase{
		{"CABOOSE_NO_AUTO_BUILD", docker, 0, tail, []string{"caboose: run 'caboose build' to rebuild it from this launcher,\n"}, false},
	})
}

// caboose restart onto a changed base builds before it removes the container,
// so the sessions run on through the build, and a base the check refuses
// leaves the container as it was.
func TestRestartRebuildsBeforeRemoving(t *testing.T) {
	for _, tc := range []struct {
		name, probe string
		want        string
	}{
		{"built", probeComplete, "layer rm"},
		{"refused", probeAlpine, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := scriptedDocker(t, containerRunning+"\n"+imageLabels(byoLabels("v9", "node:20"))+"\n"+
				imageIDOf("node:22", baseID)+"\n"+probePasses(t, tc.probe))
			home := sandboxEnv(t, "CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box", "CABOOSE_BASE_IMAGE", "node:22",
				"CABOOSE_READY_TIMEOUT", "0")
			inProject(t, home)
			_, _, errs := runIt("restart")
			if !strings.Contains(errs, "caboose: image 'img' was built on 'node:20'; CABOOSE_BASE_IMAGE now names 'node:22' — rebuilding\n") {
				t.Errorf("stderr:\n%s", errs)
			}
			var order []string
			for _, l := range dockerLog(t, log) {
				switch {
				case strings.HasPrefix(l, "build "):
					order = append(order, "layer")
				case l == "rm -f box":
					order = append(order, "rm")
				}
			}
			if got := strings.Join(order, " "); got != tc.want {
				t.Errorf("docker calls: %q, want %q; stderr:\n%s", got, tc.want, errs)
			}
			if n := strings.Count(errs, "was built on"); n != 1 {
				t.Errorf("said %d times:\n%s", n, errs)
			}
		})
	}
}

// With an image dir, the base is built from it, and version names it with
// its hash.
func TestVersionOnImageDir(t *testing.T) {
	scriptedDocker(t, imageAbsent)
	home := sandboxEnv(t, "CABOOSE_IMAGE", "img", "CABOOSE_REPO_ROOT", "/does/not/exist")
	dir := filepath.Join(home, ".caboose", "envs", "default", "image")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, _ := assets.DirHash(dir)
	_, out, _ := runIt("version")
	if !strings.Contains(out, "\nbase      : img-base (built from "+dir+", "+hash[:12]+")\n") ||
		!strings.Contains(out, "\ncontext   : "+assets.LayerHash()[:12]+"\n") {
		t.Errorf("stdout:\n%s", out)
	}
}

// containerCompat makes the running box's labels carry compat n ("" for
// none, as on an image caboose build did not make, which is not judged).
func containerCompat(n string) string {
	labels := "{}"
	if n != "" {
		labels = `{"` + assets.LabelCompat + `":"` + n + `"}`
	}
	return `[ "$*" = "inspect --type=container -f {{json .Config.Labels}} box" ] && { echo '` + labels + `'; exit 0; }`
}

// A launcher that updated itself works on with a container an older one
// created -- unless their compat differs, which it refuses, saying restart;
// doctor calls it a problem.
func TestLaunchChecksCompat(t *testing.T) {
	for _, tc := range []struct {
		compat string
		refuse string
	}{
		{"", ""},
		{strconv.Itoa(assets.Compat), ""},
		{strconv.Itoa(assets.Compat + 1), "box was created by a newer caboose, which this one cannot work with"},
		{"0", "box was created by an older caboose, which this one cannot work with"},
	} {
		t.Run("compat "+or(tc.compat, "none"), func(t *testing.T) {
			log := scriptedDocker(t, strings.Join([]string{`[ "$1" = version ] && exit 0`, containerRunning, containerCompat(tc.compat),
				imageLabels(defaultLabels("v9"))}, "\n"))
			home := sandboxEnv(t, "CABOOSE_IMAGE", "img", "CABOOSE_CONTAINER", "box", "CABOOSE_READY_TIMEOUT", "0")
			inProject(t, home)
			code, _, errs := runIt("claude", "-p", "hi")
			refused := strings.Contains(errs, "cannot work with")
			if refused != (tc.refuse != "") || !strings.Contains(errs, tc.refuse) {
				t.Fatalf("exit %d, stderr:\n%s", code, errs)
			}
			if refused {
				if code != 1 || !strings.Contains(errs, "run 'caboose restart' to recreate it") {
					t.Errorf("exit %d, stderr:\n%s", code, errs)
				}
				for _, l := range dockerLog(t, log) {
					if strings.HasPrefix(l, "exec ") || strings.HasPrefix(l, "start ") {
						t.Errorf("used the container: %s", l)
					}
				}
				_, out, _ := runIt("doctor", "--offline")
				if row(out, "container", "problem: "+tc.refuse) == "" || !fixFor(out, "container", "caboose restart") {
					t.Errorf("doctor:\n%s", out)
				}
			}
		})
	}
}

// caboose update is machine-wide: an environment's config.toml it cannot
// read -- one a newer caboose wrote, say -- does not stand in its way, as
// it does in every other command's.
func TestUpdateIgnoresABrokenConfig(t *testing.T) {
	home := sandboxEnv(t)
	dir := filepath.Join(home, ".caboose", "envs", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("from_the_future = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runIt("version"); code == 0 || !strings.Contains(errs, "from_the_future") {
		t.Fatalf("version: exit %d, stderr %q", code, errs)
	}
	// A test binary is a development build, which update refuses to
	// replace: what counts is that it got that far.
	code, _, errs := runIt("update")
	if code != 1 || !strings.Contains(errs, "is a development build") || strings.Contains(errs, "from_the_future") {
		t.Errorf("update: exit %d, stderr %q", code, errs)
	}
}
