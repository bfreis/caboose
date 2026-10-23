package docker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseMounts(t *testing.T) {
	got := ParseMounts("/home/agent/.claude\t/h/.caboose/.claude\n/work/h/dev\t/h/dev\tbind\t\n" +
		"/var/lib/docker\t/var/lib/docker/volumes/v/_data\tvolume\tv\n/t\t/a\tb\tbind\t\n")
	want := []Mount{
		{Destination: "/home/agent/.claude", Source: "/h/.caboose/.claude"},
		{Destination: "/work/h/dev", Source: "/h/dev", Type: "bind"},
		{Destination: "/var/lib/docker", Source: "/var/lib/docker/volumes/v/_data", Type: "volume", Name: "v"},
		// A tab in a source stays in it.
		{Destination: "/t", Source: "/a\tb", Type: "bind"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if ms := ParseMounts(""); len(ms) != 0 {
		t.Errorf("empty output gave %v", ms)
	}
}

// Docker Desktop records most Mac bind-mount sources as its VM sees them,
// under /host_mnt, and a few as given; both must read as the Mac's path.
func TestParseMountsUndoesDockerDesktopsHostMnt(t *testing.T) {
	for _, tc := range []struct {
		goos, src, want string
	}{
		{"darwin", "/host_mnt/Users/u/dev", "/Users/u/dev"},
		{"darwin", "/Users/u/dev", "/Users/u/dev"},
		{"darwin", "/host_mntx/a", "/host_mntx/a"},
		{"darwin", "/run/host-services/ssh-auth.sock", "/run/host-services/ssh-auth.sock"},
		{"linux", "/host_mnt/Users/u/dev", "/host_mnt/Users/u/dev"},
	} {
		old := goos
		goos = tc.goos
		got := ParseMounts("/work\t" + tc.src + "\n")
		goos = old
		if len(got) != 1 || got[0].Source != tc.want {
			t.Errorf("%s: %s gave %v, want %s", tc.goos, tc.src, got, tc.want)
		}
	}
}

func TestExitCode(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 3").Run()
	if got := ExitCode(err); got != 3 {
		t.Errorf("ExitCode = %d", got)
	}
	if got := ExitCode(errors.New("x")); got != 1 {
		t.Errorf("ExitCode = %d", got)
	}
}

func TestOutputTrimsNewlinesLikeCommandSubstitution(t *testing.T) {
	c := &CLI{Path: "printf"}
	out, err := c.Output(`a\n\nb\n\n`)
	if err != nil || out != "a\n\nb" {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestContainerStateAbsentWhenDockerFails(t *testing.T) {
	c := &CLI{Path: "false"}
	if s := c.ContainerState("x"); s != "absent" {
		t.Errorf("state = %q", s)
	}
}

func TestParseLabels(t *testing.T) {
	got, err := ParseLabels(`{"dev.bfreis.caboose.version":"v1.2.3","a":""}`)
	want := map[string]string{"dev.bfreis.caboose.version": "v1.2.3", "a": ""}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, %v", got, err)
	}
	for _, in := range []string{"null", "{}"} {
		got, err := ParseLabels(in)
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("%s: got %#v, %v", in, got, err)
		}
	}
	if _, err := ParseLabels("<no value>"); err == nil {
		t.Error("garbage parsed without error")
	}
}

func TestImageLabelsAbsentVersusFailing(t *testing.T) {
	// `false` fails every call, the version check included: docker is
	// unreachable, which must not read as "no such image".
	c := &CLI{Path: "false"}
	if _, exists, err := c.ImageLabels("caboose"); exists || !IsUnreachable(err) {
		t.Errorf("unreachable docker: exists=%v err=%v", exists, err)
	}
}

// fakeDocker writes a shell script standing in for docker.
func fakeDocker(t *testing.T, body string) *CLI {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &CLI{Path: p}
}

func TestImageLabels(t *testing.T) {
	c := fakeDocker(t, `[ "$1" = image ] || exit 9; echo '{"k":"v"}'`)
	labels, exists, err := c.ImageLabels("caboose")
	if err != nil || !exists || labels["k"] != "v" {
		t.Errorf("present: %v %v %v", labels, exists, err)
	}

	// inspect fails, the engine answers `docker version`: not built yet --
	// even with another tag of the repository there, which `images -q`
	// would list (caboose:backup is not caboose:latest).
	c = fakeDocker(t, `case "$1" in
  version) exit 0 ;;
  images) echo 0123abcd; exit 0 ;;
esac
echo "Error: No such image: caboose" >&2; exit 1`)
	if labels, exists, err := c.ImageLabels("caboose"); err != nil || exists || labels != nil {
		t.Errorf("absent: %v %v %v", labels, exists, err)
	}

	// No engine: unreachable, with docker's reason rather than an exit status.
	c = fakeDocker(t, `echo "Cannot connect to the Docker daemon at unix:///x.sock. Is the docker daemon running?" >&2; exit 1`)
	_, exists, err = c.ImageLabels("caboose")
	if exists || !IsUnreachable(err) ||
		err.Error() != "docker image inspect caboose: Cannot connect to the Docker daemon at unix:///x.sock. Is the docker daemon running?" {
		t.Errorf("unreachable: %v %v", exists, err)
	}

	// A healthy engine refusing the name: an error, but not unreachable,
	// and not absent either, which would go on to build under that name.
	c = fakeDocker(t, `[ "$1" = version ] && exit 0
echo "invalid reference format: repository name (library/Caboose) must be lowercase" >&2; exit 1`)
	_, exists, err = c.ImageLabels("Caboose")
	if exists || err == nil || IsUnreachable(err) || !strings.Contains(err.Error(), "must be lowercase") {
		t.Errorf("invalid ref: %v %v", exists, err)
	}
}

// Output keeps docker's stderr out of the terminal but in the error, and the
// error still carries the exit status.
func TestOutputErrorCarriesStderr(t *testing.T) {
	c := fakeDocker(t, `echo out; echo "  it broke  " >&2; exit 3`)
	out, err := c.Output("x")
	if out != "out" || err == nil || err.Error() != "it broke" || ExitCode(err) != 3 {
		t.Errorf("got %q, %v (exit %d)", out, err, ExitCode(err))
	}
	c = fakeDocker(t, `exit 2`)
	if _, err := c.Output("x"); err == nil || err.Error() != "exit status 2" {
		t.Errorf("no stderr: %v", err)
	}
}

// A build runs with the default provenance attestation off, which would
// give every rebuild a new image ID on the containerd image store.
func TestBuildTurnsOffDefaultAttestations(t *testing.T) {
	c := fakeDocker(t, `echo "$* $BUILDX_NO_DEFAULT_ATTESTATIONS"`)
	var out strings.Builder
	if err := c.Build(&out, &out, "-t", "x", "."); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "build -t x . 1\n" {
		t.Errorf("got %q", got)
	}
}

func TestInspectVolume(t *testing.T) {
	c := fakeDocker(t, `[ "$1 $2 $3 $4 $5" = "volume inspect --format {{json .}} --" ] || exit 9
echo '{"Name":"v","Driver":"local","Mountpoint":"/var/lib/docker/volumes/v/_data","Options":{"device":"/x","o":"bind","type":"none"}}'`)
	v, exists, err := c.InspectVolume("v")
	if err != nil || !exists || v.Driver != "local" || !reflect.DeepEqual(v.Options, map[string]string{"device": "/x", "o": "bind", "type": "none"}) {
		t.Errorf("present: %+v %v %v", v, exists, err)
	}
	c = fakeDocker(t, `echo '{"Name":"v","Driver":"local","Options":null}'`)
	if v, exists, err := c.InspectVolume("v"); err != nil || !exists || v.Driver != "local" || v.Options != nil {
		t.Errorf("no options: %+v %v %v", v, exists, err)
	}
	// The engine answers and lists no such volume: absent.
	c = fakeDocker(t, `case "$1 $2" in
  "version "*) exit 0 ;;
  "volume ls") printf 'other\nv2\n'; exit 0 ;;
esac
echo "Error response from daemon: get v: no such volume" >&2; exit 1`)
	if _, exists, err := c.InspectVolume("v"); err != nil || exists {
		t.Errorf("absent: %v %v", exists, err)
	}
	// Unlisted, but inspect fails for another reason (a driver plugin
	// down can leave its volumes out of the listing): an error.
	c = fakeDocker(t, `case "$1 $2" in
  "version "*) exit 0 ;;
  "volume ls") printf 'other\n'; exit 0 ;;
esac
echo "Error response from daemon: plugin not found" >&2; exit 1`)
	if _, _, err := c.InspectVolume("v"); err == nil || IsUnreachable(err) {
		t.Errorf("unlisted, other failure: %v", err)
	}
	// Listed, yet not inspected: an error.
	c = fakeDocker(t, `case "$1 $2" in
  "version "*) exit 0 ;;
  "volume ls") printf 'v\n'; exit 0 ;;
esac
exit 1`)
	if _, _, err := c.InspectVolume("v"); err == nil || IsUnreachable(err) {
		t.Errorf("listed but failing: %v", err)
	}
	// No listing either: an error.
	c = fakeDocker(t, `[ "$1" = version ] && exit 0; exit 1`)
	if _, _, err := c.InspectVolume("v"); err == nil || IsUnreachable(err) {
		t.Errorf("listing fails: %v", err)
	}
	// No engine: unreachable.
	c = &CLI{Path: "false"}
	if _, exists, err := c.InspectVolume("v"); exists || !IsUnreachable(err) {
		t.Errorf("unreachable: %v %v", exists, err)
	}
	// Output that is no volume's: an error.
	c = fakeDocker(t, `echo 'not json'`)
	if _, _, err := c.InspectVolume("v"); err == nil {
		t.Error("unparsable output read")
	}
	// A name that is no volume's never reaches docker.
	c = fakeDocker(t, `exit 0`)
	for _, n := range []string{"", "-x", "--help", ".v", "a/b", "a:b", "a b"} {
		if _, _, err := c.InspectVolume(n); err == nil {
			t.Errorf("%q inspected", n)
		}
	}
}
