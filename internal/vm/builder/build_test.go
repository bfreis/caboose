package builder

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/vm"
)

// guest plays the builder guest: it records each command, with the names
// in the tar it was given, and answers docker image inspect and the probe.
type guest struct {
	calls  []string
	tars   map[string][]string
	probe  string
	failOn string
	// missing are the images the builder's store lacks until pulled;
	// noShell makes the probe's container fail as runc does without
	// /bin/sh.
	missing map[string]bool
	noShell bool
	// noRepo makes every pull fail, as one of an image no registry has.
	noRepo bool
}

// run is answer with what a failed command said written to stderr, as
// the exec port would.
func (f *guest) run(timeout time.Duration, in io.Reader, out, stderr io.Writer, argv ...string) error {
	err := f.answer(timeout, in, out, argv...)
	var se *StepError
	if errors.As(err, &se) && stderr != nil {
		io.WriteString(stderr, se.Said+"\n")
	}
	return err
}

func (f *guest) answer(_ time.Duration, in io.Reader, out io.Writer, argv ...string) error {
	call := strings.Join(argv, " ")
	f.calls = append(f.calls, call)
	if in != nil {
		names, err := tarNames(in)
		if err != nil {
			return err
		}
		f.tars[call] = names
	}
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return &StepError{Argv: argv, Err: errors.New("exit status 1"), Said: "boom"}
	}
	switch {
	case strings.HasPrefix(call, "docker image inspect") && f.missing[argv[len(argv)-1]]:
		return &StepError{Argv: argv, Err: errors.New("exit status 1"), Said: "Error response from daemon: No such image: " + argv[len(argv)-1]}
	case strings.HasPrefix(call, "docker pull") && f.noRepo:
		return &StepError{Argv: argv, Err: errors.New("exit status 1"), Said: "Error response from daemon: pull access denied for " + argv[len(argv)-1]}
	case strings.HasPrefix(call, "docker pull"):
		delete(f.missing, argv[len(argv)-1])
		if out != nil {
			io.WriteString(out, "Pull complete\n")
		}
	case strings.HasPrefix(call, "docker image inspect"):
		fmt.Fprintf(out, `"sha256:%s" {"User":"agent","Env":["PATH=/bin"],"Entrypoint":["/usr/local/bin/caboose-entrypoint"],"WorkingDir":"/work"}`+"\n", argv[len(argv)-1])
	case strings.HasPrefix(call, "docker run") && f.noShell:
		return &StepError{Argv: argv, Err: exitStatus(127),
			Said: `docker: Error response from daemon: failed to create task: exec: "/bin/sh": stat /bin/sh: no such file or directory`}
	case strings.HasPrefix(call, "docker run"):
		io.WriteString(out, f.probe)
	}
	return nil
}

// exitStatus is a real *exec.ExitError of code, as a command's would be.
func exitStatus(code int) error {
	return exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
}

func tarNames(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			sort.Strings(names)
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Uid != 0 || h.Gid != 0 {
			return nil, fmt.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		names = append(names, h.Name)
	}
}

// A probe report that passes: parsed by imagecheck, as the real one is.
func passingProbe(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/probe-ok.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contexts(t *testing.T) (string, string) {
	t.Helper()
	d := t.TempDir()
	base, layer := filepath.Join(d, "base"), filepath.Join(d, "layer")
	for p, s := range map[string]string{
		base + "/Dockerfile":                           "FROM scratch\n",
		layer + "/layer.Dockerfile":                    "ARG BASE\nFROM ${BASE}\n",
		layer + "/agent-bin/caboose-agent-linux-arm64": "elf",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base, layer
}

// The build is caboose build's steps, in the guest: the base from its
// context, the check on its ID, the layer from its context, then the same
// build again as the root disk, and the image's config.
func TestBuild(t *testing.T) {
	base, layer := contexts(t)
	f := &guest{tars: map[string][]string{}, probe: passingProbe(t)}
	g := &Guest{run: f.run}
	r, err := g.Build(Request{
		BaseContext: base, BaseTag: "caboose-base", BaseLabels: []string{"a=b"},
		Probe: []byte("probe"), UID: 501, GID: 20,
		LayerContext: layer, LayerFile: "layer.Dockerfile", Image: "caboose", LayerLabels: []string{"c=d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	layerArgs := "--provenance=false -f layer.Dockerfile --build-arg BASE=caboose-base --build-arg CABOOSE_UID=501 --build-arg CABOOSE_GID=20 --label c=d"
	want := []string{
		"docker build --progress=plain --provenance=false -t caboose-base --label a=b -",
		"docker image inspect --format {{json .Id}} {{json .Config}} caboose-base",
		"docker run --network=host --rm --init --user 0:0 -e CABOOSE_PROBE_ROOT= --entrypoint /bin/sh sha256:caboose-base -c probe caboose-probe 501 20",
		"docker build --progress=plain -t caboose " + layerArgs + " -",
		"caboose-builder root " + layerArgs,
		"docker image inspect --format {{json .Id}} {{json .Config}} caboose",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := f.tars[want[0]]; !reflect.DeepEqual(got, []string{"Dockerfile"}) {
		t.Errorf("base context %q", got)
	}
	wantLayer := []string{"agent-bin/", "agent-bin/caboose-agent-linux-arm64", "layer.Dockerfile"}
	for _, c := range want[3:5] {
		if got := f.tars[c]; !reflect.DeepEqual(got, wantLayer) {
			t.Errorf("%s: context %q", c, got)
		}
	}
	if r.BaseID != "sha256:caboose-base" || r.ImageID != "sha256:caboose" || r.Base != "caboose-base" {
		t.Errorf("result %+v", r)
	}
	wantCfg := Config{User: "agent", Env: []string{"PATH=/bin"}, Entrypoint: []string{"/usr/local/bin/caboose-entrypoint"}, WorkingDir: "/work"}
	if !reflect.DeepEqual(r.Config, wantCfg) {
		t.Errorf("config %+v", r.Config)
	}
}

// A user's base is pulled only when missing or when asked.
func TestBuildPull(t *testing.T) {
	_, layer := contexts(t)
	for _, pull := range []bool{false, true} {
		f := &guest{tars: map[string][]string{}, probe: passingProbe(t)}
		g := &Guest{run: f.run}
		_, err := g.Build(Request{BaseRef: "example/img:1", Pull: pull, Probe: []byte("p"),
			LayerContext: layer, LayerFile: "layer.Dockerfile", Image: "caboose"})
		if err != nil {
			t.Fatal(err)
		}
		pulled := false
		for _, c := range f.calls {
			pulled = pulled || c == "docker pull example/img:1"
		}
		if pulled != pull {
			t.Errorf("pull %v: pulled %v (%q)", pull, pulled, f.calls)
		}
	}
}

// A base the check refuses stops the build before the layer, saying why.
func TestBuildCheckRefuses(t *testing.T) {
	base, layer := contexts(t)
	probe := strings.Replace(passingProbe(t), "ok sh", "missing sh", 1)
	f := &guest{tars: map[string][]string{}, probe: probe}
	g := &Guest{run: f.run}
	_, err := g.Build(Request{BaseContext: base, BaseTag: "b", Probe: []byte("p"),
		LayerContext: layer, LayerFile: "layer.Dockerfile", Image: "caboose"})
	var ce *CheckError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "-t caboose ") {
			t.Fatalf("built the layer on a refused base: %q", f.calls)
		}
	}
}

// A failed step says which, and what it said last.
func TestBuildStepFails(t *testing.T) {
	base, layer := contexts(t)
	f := &guest{tars: map[string][]string{}, probe: passingProbe(t), failOn: "caboose-builder root"}
	g := &Guest{run: f.run}
	_, err := g.Build(Request{BaseContext: base, BaseTag: "b", Probe: []byte("p"),
		LayerContext: layer, LayerFile: "layer.Dockerfile", Image: "caboose"})
	if err == nil || !strings.Contains(err.Error(), "writing the root disk") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// Has tells a missing image from a builder that cannot answer.
func TestHas(t *testing.T) {
	f := &guest{tars: map[string][]string{}, missing: map[string]bool{"gone:1": true}}
	g := &Guest{run: f.run}
	if ok, err := g.Has("here:1"); !ok || err != nil {
		t.Errorf("here:1: %v, %v", ok, err)
	}
	if ok, err := g.Has("gone:1"); ok || err != nil {
		t.Errorf("gone:1: %v, %v", ok, err)
	}
	f.failOn = "inspect"
	if _, err := g.Has("here:1"); err == nil {
		t.Error("a failing inspect read as an answer")
	}
}

// Pull shows docker's progress where asked.
func TestPull(t *testing.T) {
	f := &guest{tars: map[string][]string{}, missing: map[string]bool{"example/img:1": true}}
	g := &Guest{run: f.run}
	var progress strings.Builder
	if err := g.Pull("example/img:1", &progress); err != nil {
		t.Fatal(err)
	}
	if progress.String() != "Pull complete\n" || f.missing["example/img:1"] {
		t.Errorf("progress %q, still missing %v", progress.String(), f.missing)
	}
}

// Check is caboose check-image's answer: the probe on the image's ID,
// the report named as asked; an image without /bin/sh is an answer too.
func TestCheck(t *testing.T) {
	f := &guest{tars: map[string][]string{}, probe: passingProbe(t)}
	g := &Guest{run: f.run}
	id, rep, err := g.Check("example/img:1", []byte("probe"), 501, 20)
	if err != nil {
		t.Fatal(err)
	}
	if id != "sha256:example/img:1" || rep.Image != "example/img:1" || !rep.OK() {
		t.Errorf("id %q, report %+v", id, rep)
	}
	want := "docker run --network=host --rm --init --user 0:0 -e CABOOSE_PROBE_ROOT= --entrypoint /bin/sh sha256:example/img:1 -c probe caboose-probe 501 20"
	if f.calls[len(f.calls)-1] != want {
		t.Errorf("probe run %q", f.calls[len(f.calls)-1])
	}

	f.noShell = true
	_, rep, err = g.Check("example/img:1", []byte("probe"), 501, 20)
	if err != nil || !rep.NoShell || rep.OK() {
		t.Fatalf("no /bin/sh: %+v, %v", rep, err)
	}

	f.noShell, f.failOn = false, "docker run"
	if _, _, err := g.Check("example/img:1", []byte("probe"), 501, 20); err == nil || !strings.Contains(err.Error(), "cannot check image 'example/img:1'") {
		t.Errorf("a failed probe: %v", err)
	}
}

// Only a step whose output is the build's shows its stderr: a lookup that
// finds no image is an answer, and a failed pull or probe says it in the
// error, once.
func TestQuiet(t *testing.T) {
	base, layer := contexts(t)
	var shown bytes.Buffer
	f := &guest{tars: map[string][]string{}, probe: passingProbe(t), missing: map[string]bool{"gone:1": true}, noRepo: true}
	g := &Guest{b: &Builder{Stderr: &shown}, run: f.run}
	if ok, err := g.Has("gone:1"); ok || err != nil {
		t.Fatalf("Has: %v, %v", ok, err)
	}
	if err := g.Pull("gone:1", io.Discard); err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("Pull: %v", err)
	}
	f.failOn = "docker run"
	if _, _, err := g.Check("here:1", []byte("probe"), 501, 20); err == nil {
		t.Fatal("a failed probe passed")
	}
	if shown.Len() != 0 {
		t.Fatalf("shown on stderr: %q", shown.String())
	}
	f.failOn = "caboose-builder root"
	if _, err := g.Build(Request{BaseContext: base, BaseTag: "b", Probe: []byte("p"),
		LayerContext: layer, LayerFile: "layer.Dockerfile", Image: "caboose"}); err == nil {
		t.Fatal("a failed build passed")
	}
	if shown.String() != "boom\n" {
		t.Errorf("a build step's stderr, shown: %q", shown.String())
	}
}

func TestTailWriter(t *testing.T) {
	var w tailWriter
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&w, "line %d\n", i)
	}
	got := strings.Split(w.String(), "\n")
	if len(got) != 20 || got[19] != "line 999" {
		t.Fatalf("tail %q", got)
	}
	var b bytes.Buffer
	b.WriteString(w.String())
}

// The builder's boot is ready only once its resolver has answered: its
// first steps look names up with whichever libc a user's image has.
func TestBootSpecWaitsForResolver(t *testing.T) {
	if s := bootSpec(""); !s.WaitResolver || s.Ready != readyFile || s.Cmd[0] != entry {
		t.Errorf("builder boot spec %+v", s)
	}
}

// With the outbound proxy, the builder's environment names it, for its
// dockerd's pulls; without it, nothing does.
func TestBootSpecEgress(t *testing.T) {
	for _, e := range agentproto.EgressEnv(agentproto.EgressListen) {
		if !slices.Contains(bootSpec(agentproto.EgressListen).Env, e) {
			t.Errorf("builder env lacks %s", e)
		}
		if slices.Contains(bootSpec("").Env, e) {
			t.Errorf("builder env without the proxy has %s", e)
		}
	}
}

// Through the outbound proxy, every docker build runs on the guest's own
// network with the proxy's variables as build args -- before a user's own
// flags, which win -- and the probe has them in its environment.
func TestBuildThroughEgress(t *testing.T) {
	base, layer := contexts(t)
	f := &guest{tars: map[string][]string{}, probe: passingProbe(t)}
	g := &Guest{run: f.run, egress: agentproto.EgressListen}
	_, err := g.Build(Request{
		BaseContext: base, BaseTag: "caboose-base", BaseArgs: []string{"--no-cache"},
		Probe: []byte("probe"), UID: 501, GID: 20,
		LayerContext: layer, LayerFile: "layer.Dockerfile", Image: "caboose", LayerArgs: []string{"--build-arg", "X=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy := "--network=host --build-arg HTTP_PROXY=http://127.0.0.1:9128 --build-arg HTTPS_PROXY=http://127.0.0.1:9128 " +
		"--build-arg http_proxy=http://127.0.0.1:9128 --build-arg https_proxy=http://127.0.0.1:9128 " +
		"--build-arg NO_PROXY=localhost,127.0.0.1,::1,.localhost,172.16.0.0/12 --build-arg no_proxy=localhost,127.0.0.1,::1,.localhost,172.16.0.0/12"
	layerArgs := "--provenance=false -f layer.Dockerfile --build-arg BASE=caboose-base --build-arg CABOOSE_UID=501 --build-arg CABOOSE_GID=20 " +
		proxy + " --build-arg X=1"
	want := []string{
		"docker build --progress=plain --provenance=false -t caboose-base " + proxy + " --no-cache -",
		"docker image inspect --format {{json .Id}} {{json .Config}} caboose-base",
		"docker run --network=host -e HTTP_PROXY=http://127.0.0.1:9128 -e HTTPS_PROXY=http://127.0.0.1:9128 " +
			"-e http_proxy=http://127.0.0.1:9128 -e https_proxy=http://127.0.0.1:9128 " +
			"-e NO_PROXY=localhost,127.0.0.1,::1,.localhost,172.16.0.0/12 -e no_proxy=localhost,127.0.0.1,::1,.localhost,172.16.0.0/12 " +
			"--rm --init --user 0:0 -e CABOOSE_PROBE_ROOT= --entrypoint /bin/sh sha256:caboose-base -c probe caboose-probe 501 20",
		"docker build --progress=plain -t caboose " + layerArgs + " -",
		"caboose-builder root " + layerArgs,
		"docker image inspect --format {{json .Id}} {{json .Config}} caboose",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

type fakeLink struct{ closed int }

func (l *fakeLink) Close() error { l.closed++; return nil }

// The builder's link is opened once it is up, the proxy waited for, and
// the link closed with the guest; a proxy that does not come up fails the
// start, and nothing is linked without the proxy.
func TestLinkEgress(t *testing.T) {
	dir := vm.Dir(t.TempDir())
	var linked []vm.Dir
	l := &fakeLink{}
	b := &Builder{Dir: dir, Egress: agentproto.EgressListen, Link: func(d vm.Dir) (io.Closer, error) {
		linked = append(linked, d)
		return l, nil
	}}
	f := &guest{tars: map[string][]string{}}
	g := &Guest{b: b, run: f.run}
	if err := g.linkEgress(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(linked, []vm.Dir{dir}) || !reflect.DeepEqual(f.calls, []string{"/run/caboose/agent wait-proxy 30"}) ||
		g.egress != agentproto.EgressListen {
		t.Fatalf("linked %v, calls %q, egress %q", linked, f.calls, g.egress)
	}
	if err := g.Stop(); err != nil || l.closed != 1 {
		t.Errorf("stop: %v, link closed %d times", err, l.closed)
	}
	if err := g.Stop(); err != nil || l.closed != 1 {
		t.Errorf("stop again: %v, link closed %d times", err, l.closed)
	}

	f = &guest{tars: map[string][]string{}, failOn: "wait-proxy"}
	g = &Guest{b: b, run: f.run}
	if err := g.linkEgress(); err == nil || !strings.Contains(err.Error(), "outbound proxy did not come up") {
		t.Errorf("a proxy that does not come up: %v", err)
	}

	linked = nil
	b.Egress = ""
	f = &guest{tars: map[string][]string{}}
	g = &Guest{b: b, run: f.run}
	if err := g.linkEgress(); err != nil || linked != nil || f.calls != nil {
		t.Errorf("without the proxy: %v, linked %v, calls %q", err, linked, f.calls)
	}
}
