package backend

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/docker"
)

// echoDocker is a docker that prints its arguments, one a line, and fails
// with "no" on stderr when the last one is "fail".
func echoDocker(t *testing.T) *Docker {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
for a; do printf '%s\n' "$a"; last=$a; done
[ "$last" = fail ] && { echo no >&2; exit 3; }
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewDocker(&docker.CLI{Path: p}, "box")
}

func argv(t *testing.T, d *Docker, s ExecSpec) []string {
	t.Helper()
	out, err := Capture(d, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// Each ExecSpec is the docker exec it always was: flags before the
// container, the command after it.
func TestDockerCommand(t *testing.T) {
	d := echoDocker(t)
	for _, tc := range []struct {
		s    ExecSpec
		want []string
	}{
		{ExecSpec{Argv: []string{"tmux", "ls"}}, []string{"exec", "box", "tmux", "ls"}},
		{ExecSpec{Argv: []string{"chmod", "0666", "/s"}, User: "0"}, []string{"exec", "-u", "0", "box", "chmod", "0666", "/s"}},
		{ExecSpec{Argv: []string{"bash"}, Env: []string{"TERM=xterm", "TZ=UTC"}, Dir: "/work/p", Stdin: true, TTY: true},
			[]string{"exec", "-i", "-t", "-e", "TERM=xterm", "-e", "TZ=UTC", "-w", "/work/p", "box", "bash"}},
		{ExecSpec{Argv: []string{"agent", "link"}, Stdin: true}, []string{"exec", "-i", "box", "agent", "link"}},
	} {
		if got := argv(t, d, tc.s); !slices.Equal(got, tc.want) {
			t.Errorf("%+v:\n got %q\nwant %q", tc.s, got, tc.want)
		}
	}
}

// A failure says what the command said on stderr, and keeps its status.
func TestCaptureFailure(t *testing.T) {
	_, err := Output(echoDocker(t), "fail")
	var f *docker.Failure
	if !errors.As(err, &f) || f.Error() != "no" || docker.ExitCode(err) != 3 {
		t.Errorf("err = %v", err)
	}
	if Quiet(echoDocker(t), "fail") == nil || Quiet(echoDocker(t), "ok") != nil {
		t.Error("Quiet does not tell success from failure")
	}
}

// The user's own arguments come after all of caboose's, where docker takes
// the last of a single-value flag, and the image and its command last.
func TestRunArgv(t *testing.T) {
	got := RunArgv("box", Spec{
		Image: "img", Cmd: []string{"--cc-supervise"}, Hostname: "caboose",
		Env:     []string{"A=1"},
		Mounts:  []Mount{{Source: "/h", Target: "/work"}, {Source: "/m", Target: "/etc/m", ReadOnly: true}},
		Volumes: []Volume{{Name: "docker", Target: "/var/lib/docker"}},
		Groups:  []string{"999"},
		Runtime: "runsc", User: "0:0",
		Labels:  []string{"k=v"},
		RunArgs: []string{"--cpus=2"},
	})
	want := []string{"run", "-d", "--name", "box", "--hostname", "caboose", "--restart", "unless-stopped", "--init",
		"-e", "A=1", "-v", "/h:/work", "-v", "/m:/etc/m:ro", "-v", "box-docker:/var/lib/docker", "--group-add", "999", "--runtime", "runsc", "--user", "0:0",
		"--label", "k=v", "--cpus=2", "img", "--cc-supervise"}
	if !slices.Equal(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if got := RunArgv("box", Spec{Image: "img", Hostname: "h"}); slices.Contains(got, "--runtime") || slices.Contains(got, "--user") {
		t.Errorf("defaults spelled out: %q", got)
	}
}

// A container has no outbound proxy of caboose's: a Spec with one is
// refused before docker runs.
func TestDockerRefusesEgress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch \"$0.ran\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := NewDocker(&docker.CLI{Path: p}, "box")
	if err := d.Create(Spec{Image: "img", Egress: "127.0.0.1:9128"}); err == nil || !strings.Contains(err.Error(), "outbound proxy") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(p + ".ran"); err == nil {
		t.Error("docker ran")
	}
}

// A container addressed by ID is named by it in every call, after the
// pinned engine's --host; anything but a full ID is refused.
func TestDockerByID(t *testing.T) {
	id := strings.Repeat("0a", 32)
	for _, bad := range []string{"", "caboose-default", id[:12], strings.ToUpper(id), id + "0"} {
		if _, err := NewDockerByID(&docker.CLI{Path: "docker"}, bad); err == nil {
			t.Errorf("NewDockerByID(%q) was taken", bad)
		}
	}
	e := echoDocker(t)
	e.CLI.Host = "unix:///engine.sock"
	d, err := NewDockerByID(e.CLI, id)
	if err != nil {
		t.Fatal(err)
	}
	got := argv(t, d, ExecSpec{User: "root", Argv: []string{"true"}})
	want := []string{"--host", "unix:///engine.sock", "exec", "-u", "root", id, "true"}
	if !slices.Equal(got, want) {
		t.Fatalf("exec argv %q, want %q", got, want)
	}
}

// Started is the running container's State.StartedAt, asked by name, and
// nothing for one that does not run, or a time docker cannot give.
func TestDockerStarted(t *testing.T) {
	for _, tc := range []struct {
		says string
		want time.Time
		ok   bool
	}{
		{"true 2026-10-18T12:00:00.123456789Z", time.Date(2026, 10, 18, 12, 0, 0, 123456789, time.UTC), true},
		{"false 2026-10-18T12:00:00Z", time.Time{}, false},
		{"true 0001-01-01T00:00:00Z", time.Time{}, false},
		{"true soon", time.Time{}, false},
		{"", time.Time{}, false},
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "docker")
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"" + dir + "/args\"\necho '" + tc.says + "'\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		got, ok := NewDocker(&docker.CLI{Path: p}, "box").Started()
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("%q: %v, %v", tc.says, got, ok)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "args"))
		if string(args) != "inspect --type=container -f {{.State.Running}} {{.State.StartedAt}} box\n" {
			t.Errorf("asked %q", args)
		}
	}
}

// volumeDocker is a docker that logs each call, its arguments joined by
// spaces, to log, and keeps volumes as files of dir: vol-NAME holds what
// inspect prints of volume NAME. df is what system df prints.
func volumeDocker(t *testing.T, df string) (d *Docker, dir string, log func() []string) {
	t.Helper()
	dir = t.TempDir()
	p := filepath.Join(dir, "docker")
	script := `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/log"
[ -f "$d/fail" ] && case "$*" in "$(cat "$d/fail")"*) echo "boom on $(cat "$d/fail")" >&2; exit 1 ;; esac
case "$1 $2" in
  "version "*) exit 0 ;;
  "volume inspect") [ -f "$d/vol-$6" ] && { cat "$d/vol-$6"; exit 0; }; echo "Error: no such volume: $6" >&2; exit 1 ;;
  "volume ls") [ "$3" = --quiet ] && { ls "$d" | sed -n 's/^vol-//p'; exit 0; }; cat "$d/ls"; exit 0 ;;
  "volume create") eval "n=\${$#}"; echo '{"Name":"'"$n"'","Driver":"local"}' > "$d/vol-$n"; exit 0 ;;
  "system df") [ -f "$d/slow" ] && sleep 30; cat "$d/df"; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "df"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewDocker(&docker.CLI{Path: p}, "box"), dir, func() []string {
		b, _ := os.ReadFile(filepath.Join(dir, "log"))
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

// Create makes each volume the engine lacks, labelled as the container's,
// leaves one it has alone, gives them all to VolumeOwner in a throwaway
// container of the same image and runtime, as root, with no network,
// running chown on the volumes' roots alone, and only then runs the
// sandbox.
func TestDockerCreateVolumes(t *testing.T) {
	d, dir, log := volumeDocker(t, "")
	if err := os.WriteFile(filepath.Join(dir, "vol-box-data"), []byte(`{"Driver":"local","Labels":{"dev.bfreis.caboose.sandbox":"box"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Spec{Image: "img", Hostname: "h", Runtime: "runsc", VolumeOwner: "501:20",
		Volumes: []Volume{{Name: "cache", Target: "/home/agent/.cache/go-build"}, {Name: "data", Target: "/data"}}}
	if err := d.Create(s); err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, l := range log() {
		if !strings.HasPrefix(l, "volume inspect") && !strings.HasPrefix(l, "version") && !strings.HasPrefix(l, "volume ls --quiet") {
			calls = append(calls, l)
		}
	}
	want := []string{
		"volume create --label dev.bfreis.caboose.sandbox=box --label dev.bfreis.caboose.volume=cache -- box-cache",
		"run --rm --network none --user 0:0 --runtime runsc --entrypoint chown -v box-cache:/home/agent/.cache/go-build -v box-data:/data img 501:20 /home/agent/.cache/go-build /data",
		strings.Join(RunArgv("box", s), " "),
	}
	if !slices.Equal(calls, want) {
		t.Errorf("\n got %q\nwant %q", calls, want)
	}
	if !strings.Contains(want[2], "-v box-cache:/home/agent/.cache/go-build -v box-data:/data") {
		t.Errorf("run: %s", want[2])
	}

	// No owner (a container that runs as root): no chown.
	d, _, log = volumeDocker(t, "")
	s.VolumeOwner = ""
	if err := d.Create(s); err != nil {
		t.Fatal(err)
	}
	if l := strings.Join(log(), "\n"); strings.Contains(l, "chown") || !strings.Contains(l, "volume create") {
		t.Errorf("calls:\n%s", l)
	}
	// No volumes: neither.
	d, _, log = volumeDocker(t, "")
	if err := d.Create(Spec{Image: "img", Hostname: "h", VolumeOwner: "501:20"}); err != nil {
		t.Fatal(err)
	}
	if l := strings.Join(log(), "\n"); strings.Contains(l, "chown") || strings.Contains(l, "volume") {
		t.Errorf("calls:\n%s", l)
	}
}

// A volume of the name that is not one caboose made for the container is
// refused, as a *VolumeError, with nothing created or run: one labelled
// as another container's (two containers' names can meet), one with no
// label of caboose's (made by hand), and one that is not docker's own
// storage (another driver, or driver options, as a volume bound to a
// host directory has).
func TestDockerCreateVolumeOfAnother(t *testing.T) {
	for _, tc := range []struct{ vol, want string }{
		{`{"Driver":"local","Labels":{"dev.bfreis.caboose.sandbox":"box-x"}}`,
			"docker volume box-cache is container box-x's (its label dev.bfreis.caboose.sandbox), not box's: their names meet"},
		{`{"Driver":"local","Labels":null}`,
			"docker volume box-cache was not made by caboose (it has no label dev.bfreis.caboose.sandbox), and caboose mounts only its own: if nothing of yours needs it, 'docker volume rm box-cache' deletes it"},
		{`{"Driver":"local","Labels":{"dev.bfreis.caboose.volume":"cache"}}`,
			"was not made by caboose (it has no label dev.bfreis.caboose.sandbox)"},
		{`{"Driver":"nfs","Labels":{"dev.bfreis.caboose.sandbox":"box"}}`,
			`docker volume box-cache is not docker's own storage (driver "nfs", options map[]), and caboose mounts only a plain local volume, whose root it gives to the sandbox's user: if nothing of yours needs it, 'docker volume rm box-cache' deletes it`},
		{`{"Driver":"local","Options":{"type":"none","o":"bind","device":"/Users/me"},"Labels":{"dev.bfreis.caboose.sandbox":"box"}}`,
			`docker volume box-cache is not docker's own storage (driver "local", options map[device:/Users/me o:bind type:none])`},
	} {
		d, dir, log := volumeDocker(t, "")
		if err := os.WriteFile(filepath.Join(dir, "vol-box-cache"), []byte(tc.vol), 0o644); err != nil {
			t.Fatal(err)
		}
		err := d.Create(Spec{Image: "img", Hostname: "h", VolumeOwner: "1:1", Volumes: []Volume{{Name: "cache", Target: "/c"}}})
		var ve *VolumeError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", tc.vol, err)
		}
		for _, l := range log() {
			if strings.HasPrefix(l, "run") || strings.HasPrefix(l, "volume create") {
				t.Errorf("%s: ran %s", tc.vol, l)
			}
		}
	}
}

// A volume docker fails to inspect, make or give to the user fails Create
// as a *VolumeError that holds what docker said and what to do, though it
// unwraps to docker's *exec.ExitError: docker's stderr was captured, so
// the launcher has to print it.
func TestDockerCreateVolumeFailures(t *testing.T) {
	for _, tc := range []struct{ fail, want string }{
		{"volume inspect", "volume cache: docker volume inspect box-cache: boom on volume inspect; the container was not created: once docker answers ('docker info'), run caboose again"},
		{"volume create", "creating docker volume box-cache: boom on volume create; the container was not created: fix what docker says, then run caboose again"},
		{"run --rm", "giving the volumes' roots to the sandbox's user (1:1), in a throwaway container of the image running its chown as root: boom on run --rm; the container was not created"},
	} {
		d, dir, log := volumeDocker(t, "")
		if err := os.WriteFile(filepath.Join(dir, "fail"), []byte(tc.fail), 0o644); err != nil {
			t.Fatal(err)
		}
		err := d.Create(Spec{Image: "img", Hostname: "h", VolumeOwner: "1:1", Volumes: []Volume{{Name: "cache", Target: "/c"}}})
		var ve *VolumeError
		var ee *exec.ExitError
		if !errors.As(err, &ve) || !errors.As(err, &ee) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", tc.fail, err)
		}
		for _, l := range log() {
			if strings.HasPrefix(l, "run -d") {
				t.Errorf("%s: ran %s", tc.fail, l)
			}
		}
	}
}

// The ownership helper runs in the user namespace the sandbox's run_args
// give it, the last --userns= of them: under an engine's userns-remap a
// chown in the remapped namespace would give the volumes to a user the
// sandbox, run with --userns=host, is not.
func TestOwnerArgvUserns(t *testing.T) {
	s := Spec{Image: "img", VolumeOwner: "1:1", Volumes: []Volume{{Name: "cache", Target: "/c"}}}
	want := "run --rm --network none --user 0:0 --entrypoint chown -v box-cache:/c img 1:1 /c"
	if got := strings.Join(OwnerArgv("box", s), " "); got != want {
		t.Errorf("no userns:\n got %s\nwant %s", got, want)
	}
	s.RunArgs = []string{"--userns=other", "--memory=1g", "--userns=host"}
	want = "run --rm --network none --user 0:0 --userns host --entrypoint chown -v box-cache:/c img 1:1 /c"
	if got := strings.Join(OwnerArgv("box", s), " "); got != want {
		t.Errorf("userns:\n got %s\nwant %s", got, want)
	}
}

// ListVolumes is the volumes labelled as the container's whose name is
// its for their volume label, with the size system df says only when
// sizes are asked for; RemoveVolume deletes the one for a name.
func TestDockerListRemoveVolumes(t *testing.T) {
	d, dir, log := volumeDocker(t, `{"Volumes":[{"Name":"box-cache","Size":"1.2GB"},{"Name":"other","Size":"9GB"}]}`)
	ls := "box-cache\tcache\nbox-old\told\nbox-x\tcache\nstray\t\n"
	if err := os.WriteFile(filepath.Join(dir, "ls"), []byte(ls), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := d.ListVolumes(true)
	want := []EngineVolume{{Name: "cache", Engine: "box-cache", Size: "1.2GB"}, {Name: "old", Engine: "box-old"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %+v, %v; want %+v", got, err, want)
	}
	if l := log()[0]; l != `volume ls --filter label=dev.bfreis.caboose.sandbox=box --format {{.Name}}{{"\t"}}{{.Label "dev.bfreis.caboose.volume"}}` {
		t.Errorf("ls: %s", l)
	}
	n := len(log())
	got, err = d.ListVolumes(false)
	want = []EngineVolume{{Name: "cache", Engine: "box-cache"}, {Name: "old", Engine: "box-old"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("no sizes: got %+v, %v; want %+v", got, err, want)
	}
	if l := log(); len(l) != n+1 || strings.Contains(strings.Join(l[n:], "\n"), "system df") {
		t.Errorf("no sizes asked df: %q", l[n:])
	}
	if err := d.RemoveVolume("old"); err != nil {
		t.Fatal(err)
	}
	if l := log(); l[len(l)-1] != "volume rm -- box-old" {
		t.Errorf("rm: %q", l)
	}
}

// A system df slower than VolumeSizeTimeout leaves the sizes unknown, and
// the listing comes back anyway.
func TestDockerListVolumesSlowSizes(t *testing.T) {
	d, dir, _ := volumeDocker(t, `{"Volumes":[{"Name":"box-cache","Size":"1.2GB"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "ls"), []byte("box-cache\tcache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(t time.Duration) { VolumeSizeTimeout = t }(VolumeSizeTimeout)
	VolumeSizeTimeout = 200 * time.Millisecond
	start := time.Now()
	got, err := d.ListVolumes(true)
	want := []EngineVolume{{Name: "cache", Engine: "box-cache"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %+v, %v; want %+v", got, err, want)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s", took)
	}
}
