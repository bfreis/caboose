package imagecheck

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/docker"
)

// fakeDocker is a docker CLI whose `run` is body (sh), and whose `version`
// exits versionRC. The default body really runs the probe: it drops the
// docker flags up to -c and hands the rest to the local /bin/sh, with the
// network stubbed out, so Run is tested against the script it embeds.
func fakeDocker(t *testing.T, body string, versionRC string) *docker.CLI {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		body = `while [ "$1" != -c ]; do shift; done; shift
PATH="` + bin + `:$PATH" exec /bin/sh -c "$@"`
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + filepath.Join(bin, "log") + `"
case "$1" in
    version) exit ` + versionRC + ` ;;
    run) ` + body + ` ;;
esac
exit 1
`
	path := filepath.Join(bin, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &docker.CLI{Path: path}
}

func TestArgs(t *testing.T) {
	got := Args("alpine:3.20", "", []byte("SCRIPT"), 501, 20)
	// --init, so a ^C reaches the probe's sh, which as PID 1 ignores it.
	want := []string{"run", "--rm", "--init", "--user", "0:0", "-e", "CABOOSE_PROBE_ROOT=", "--entrypoint", "/bin/sh",
		"alpine:3.20", "-c", "SCRIPT", "caboose-probe", "501", "20"}
	if !slices.Equal(got, want) {
		t.Errorf("args %q", got)
	}
	// A build's --platform picks the variant checked.
	got = Args("alpine:3.20", "linux/amd64", []byte("SCRIPT"), 501, 20)
	want = append([]string{"run", "--rm", "--init", "--platform", "linux/amd64"}, want[3:]...)
	if !slices.Equal(got, want) {
		t.Errorf("args %q", got)
	}
}

func TestRunProbes(t *testing.T) {
	r, err := Run(fakeDocker(t, "", "0"), "img", "", 4242, 4343)
	if err != nil {
		t.Fatal(err)
	}
	if r.Image != "img" || r.UID != 4242 || r.GID != 4343 || r.NoShell {
		t.Errorf("%+v", r)
	}
	if c, ok := r.Check("sh"); !ok || !c.OK {
		t.Errorf("sh %+v", c)
	}
}

// What docker and podman say when the image has no /bin/sh: an answer, not
// an error.
func TestRunNoShell(t *testing.T) {
	for name, body := range map[string]string{
		"docker": `echo 'docker: Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown.' >&2; exit 127`,
		"podman": "echo 'Error: crun: executable file `/bin/sh` not found: No such file or directory: OCI runtime attempted to invoke a command that was not found' >&2; exit 127",
		"126":    `echo 'exec: "/bin/sh": permission denied' >&2; exit 126`,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Run(fakeDocker(t, body, "0"), "distroless", "", 1000, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if !r.NoShell || r.OK() || r.Image != "distroless" {
				t.Errorf("%+v", r)
			}
		})
	}
}

func TestRunFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, versionRC string
		unreachable           bool
		want                  string
	}{
		{"engine down", `echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2; exit 1`, "1",
			true, "docker run img: Cannot connect to the Docker daemon"},
		{"pull denied", `echo "docker: Error response from daemon: pull access denied for img" >&2; exit 125`, "0",
			false, "docker run img: docker: Error response from daemon: pull access denied"},
		// Exit 127 from something other than the entrypoint is not "no sh".
		{"other 127", `echo "something else went wrong" >&2; exit 127`, "0", false, "something else went wrong"},
		{"not the probe", `echo "Welcome!"; exit 0`, "0", false, "checking image 'img': not the probe's output"},
		{"truncated", `echo "probe 1"; echo "ok sh /bin/sh"; exit 0`, "0", false, "truncated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Run(fakeDocker(t, tc.body, tc.versionRC), "img", "", 1000, 1000)
			if err == nil {
				t.Fatalf("no error: %+v", r)
			}
			if docker.IsUnreachable(err) != tc.unreachable {
				t.Errorf("unreachable = %v: %v", !tc.unreachable, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %q, want %q", err, tc.want)
			}
		})
	}
}
