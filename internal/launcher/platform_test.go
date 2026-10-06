package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
)

// creating is a boxApp under iso with no sandbox yet, whose image has
// labels.
func creating(t *testing.T, iso string, labels map[string]string) *boxApp {
	t.Helper()
	b := newBoxApp(t, iso, &backendtest.Fake{})
	b.setImage(t, boxImageID, labels)
	return b
}

// claudeMounts are the three platform mounts of the Spec b's sandbox was
// created from, data-dir-relative.
func claudeMounts(b *boxApp) map[string]string {
	mounts := map[string]string{}
	spec, _ := b.box.Spec()
	for _, m := range spec.Mounts {
		if strings.HasPrefix(m.Target, "/home/agent/.local/") || strings.HasPrefix(m.Target, "/home/agent/.cache/") {
			mounts[m.Target] = strings.TrimPrefix(m.Source, b.data+"/")
		}
	}
	return mounts
}

func wantPlatformMounts(t *testing.T, got map[string]string, platform string) {
	t.Helper()
	want := map[string]string{
		"/home/agent/.local/bin":          "local/" + platform + "/bin",
		"/home/agent/.local/share/claude": "local/" + platform + "/share/claude",
		"/home/agent/.cache/claude":       "local/" + platform + "/cache/claude",
	}
	for dst, w := range want {
		if got[dst] != w {
			t.Errorf("%s: mounted %q, want %q", dst, got[dst], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("mounts = %v", got)
	}
}

// The image's platform label names the dir.
func TestCreateContainerPlatformFromLabel(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := creating(t, iso, currentLabels("linux-x64-musl"))
			if err := b.createContainer(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			wantPlatformMounts(t, claudeMounts(b), "linux-x64-musl")
		})
	}
}

// The musl build is told to use the image's ripgrep, at creation and from
// the label; a glibc image is told nothing.
func TestCreateContainerRipgrepOnMusl(t *testing.T) {
	for _, iso := range isolations {
		for platform, want := range map[string]bool{"linux-x64-musl": true, "linux-arm64-musl": true, "linux-x64": false, "linux-arm64": false} {
			b := creating(t, iso, currentLabels(platform))
			if err := b.createContainer(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			spec, _ := b.box.Spec()
			if got := slices.Contains(spec.Env, "USE_BUILTIN_RIPGREP=0"); got != want {
				t.Errorf("%s, %s: USE_BUILTIN_RIPGREP=0 set %v, want %v", iso, platform, got, want)
			}
			if n := slices.IndexFunc(spec.Env, func(e string) bool { return strings.HasPrefix(e, "USE_BUILTIN_RIPGREP") }); n >= 0 &&
				slices.ContainsFunc(spec.Env[n+1:], func(e string) bool { return strings.HasPrefix(e, "USE_BUILTIN_RIPGREP") }) {
				t.Errorf("%s, %s: set twice: %q", iso, platform, spec.Env)
			}
		}
	}
}

// An image without a platform it can name is none caboose build made: no
// sandbox is created from it, and no platform dir either.
func TestCreateContainerRefusesUnknownPlatform(t *testing.T) {
	withPlatform := func(p string) map[string]string {
		l := currentLabels(p)
		if p == "" {
			delete(l, assets.LabelPlatform)
		}
		return l
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"no labels", nil, "image 'img' was not built by caboose build (it has no platform label): run 'caboose build'"},
		{"no platform label", withPlatform(""), "it has no platform label"},
		{"another platform", withPlatform("windows-x64"), `its platform label says "windows-x64"`},
	} {
		for _, iso := range isolations {
			t.Run(tc.name+"/"+iso, func(t *testing.T) {
				b := creating(t, iso, tc.labels)
				err := b.createContainer(false)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want %q", err, tc.want)
				}
				if len(b.box.Specs) != 0 {
					t.Errorf("created: %+v", b.box.Specs)
				}
				if _, err := os.Lstat(filepath.Join(b.data, "local")); err == nil {
					t.Error("a platform dir was created")
				}
			})
		}
	}
}

// A launch against a running sandbox creates no platform dir: those are
// made only for the image a sandbox is created from.
func TestLaunchCreatesNoPlatformDirUnderARunningContainer(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, runningBox(iso))
			b.mountLocal("local/linux-arm64")
			mkdirs(t, b.data, "local/linux-arm64/bin", "local/linux-arm64/share/claude")
			if err := b.ensureRunning(false); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(filepath.Join(b.data, "local"))
			if len(entries) != 1 {
				t.Errorf("local now holds %v", entries)
			}
			if len(b.box.Calls) != 0 {
				t.Errorf("calls %q", b.box.Calls)
			}
		})
	}
}

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusShowsPlatforms(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, runningBox(iso))
			b.mountLocal("local/linux-arm64")
			data := b.data
			mkdirs(t, data, "local/linux-arm64/share/claude", "local/linux-arm64/bin", "local/linux-x64-musl/share/claude")
			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			s, _ := b.said()
			for _, w := range []string{
				// The lines tests/run.sh parses stay as they were.
				b.App.nounLabel() + " box (running)\n",
				"data dir  : " + data + "\n",
				"platform  : linux-arm64\n",
				"local dir : " + data + "/local/linux-arm64\n",
				"\t" + data + "/local/linux-arm64/share/claude\n",
				"\ndisk used per platform (local/<platform>, each with its own versions):\n",
				"\tlinux-arm64  (mounted)\n",
				"\tlinux-x64-musl\n",
			} {
				if !strings.Contains(s, w) {
					t.Errorf("status lacks %q:\n%s", w, s)
				}
			}
			if strings.Index(s, "linux-arm64  (mounted)") > strings.Index(s, "\tlinux-x64-musl\n") {
				t.Errorf("platforms not sorted:\n%s", s)
			}
		})
	}
}

// caboose prune prunes in the sandbox with the current
// CABOOSE_KEEP_VERSIONS, reports the mounted platform's usage, and points
// at the other platforms' dirs without touching them.
func TestPruneNotesOtherPlatforms(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, runningBox(iso))
			b.mountLocal("local/linux-arm64")
			data := b.data
			mkdirs(t, data, "local/linux-arm64/share/claude", "local/linux-arm64/bin", "local/linux-x64-musl/share/claude")
			if err := b.Prune(nil); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			out, e := b.said()
			if !strings.HasPrefix(out, "caboose: now using ") || !strings.Contains(out, data+"/local/linux-arm64/share/claude") {
				t.Errorf("stdout %q", out)
			}
			if !strings.Contains(e, "caboose: "+data+"/local/linux-x64-musl also holds ") ||
				!strings.Contains(e, "for a platform this "+b.noun()+" does not use;") ||
				!strings.Contains(e, "delete it by hand if no image of yours needs it any more") ||
				strings.Contains(e, "local/linux-arm64 also") {
				t.Errorf("stderr:\n%s", e)
			}
			if _, err := os.Stat(filepath.Join(data, "local/linux-x64-musl/share/claude")); err != nil {
				t.Error("another platform's dir was touched")
			}
			i := slices.IndexFunc(b.box.Execs, func(s backend.ExecSpec) bool {
				return slices.Equal(s.Argv, []string{Entrypoint, "--cc-prune"})
			})
			if i < 0 || !slices.Equal(b.box.Execs[i].Env, []string{"CABOOSE_KEEP_VERSIONS=2"}) {
				t.Errorf("pruned with: %+v", b.box.Execs)
			}
		})
	}
}

// caboose status reads the sandbox's environment and lists its versions
// with bash alone: printenv and ls are not image requirements (a BYO image
// may lack them), bash is. The sandbox runs each bash -c for real, in an
// environment of its own, and fails anything else.
func TestStatusNeedsOnlyBash(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			box := runningBox(iso)
			b := newBoxApp(t, iso, box)
			b.mountLocal("local/linux-arm64")
			home := filepath.Join(b.tmp, "home")
			mkdirs(t, home, ".local/share/claude/versions/2.1.9", ".local/share/claude/versions/2.1.10")
			box.Exec = func(s backend.ExecSpec) *exec.Cmd {
				if len(s.Argv) == 3 && s.Argv[0] == "bash" && s.Argv[1] == "-c" {
					return exec.Command("env", "-i", "HOME="+home, "TZ=Europe/Paris", "CABOOSE_KEEP_VERSIONS=3", "bash", "-c", s.Argv[2])
				}
				return backendtest.Fail()
			}
			b.Cfg.TZ = "Europe/Paris"
			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			s, errs := b.said()
			for _, w := range []string{
				"timezone  : Europe/Paris (host: Europe/Paris)\n",
				"(retaining 3):\n",
				"  2.1.10\n  2.1.9\n",
			} {
				if !strings.Contains(s, w) {
					t.Errorf("status lacks %q:\n%s", w, s)
				}
			}
			if !strings.Contains(errs, b.noun()+" was created with keep_versions = 3, config.toml has 2.") {
				t.Errorf("stderr:\n%s", errs)
			}
			for _, e := range box.Execs {
				if e.Argv[0] == "printenv" || e.Argv[0] == "ls" {
					t.Errorf("status ran %q", e.Argv)
				}
			}
		})
	}
}
