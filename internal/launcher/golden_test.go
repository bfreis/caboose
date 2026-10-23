package launcher

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/version"
)

// updateGolden rewrites the golden files from the current output:
// CABOOSE_UPDATE_GOLDEN=1 go test -run Golden ./internal/launcher
const updateGolden = "CABOOSE_UPDATE_GOLDEN"

// golden compares got, with the test's temp dir and the version made
// stable, to testdata/golden/name.
func golden(t *testing.T, b *boxApp, name, got string) {
	t.Helper()
	got = strings.ReplaceAll(got, b.tmp, "TMP")
	got = strings.ReplaceAll(got, version.Get().Version, "VERSION")
	path := filepath.Join("testdata", "golden", name)
	if os.Getenv(updateGolden) == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		// Each side as it is, up to a marker: a trailing newline is no
		// blank line on one side only.
		t.Errorf("%s differs from its golden file:\n--- got\n%s--- want\n%s--- end", name, got, want)
	}
}

// caboose status and caboose doctor print exactly what the golden files
// hold: the data behind them may move, the text does not. Nothing in them
// is the machine's: newBoxApp gives no host zone (no /etc/localtime, no
// TZ) and an empty environment, and the VM's size is config.toml's, not
// half this machine's CPUs and memory.
func TestStatusAndDoctorGolden(t *testing.T) {
	for _, iso := range []string{isolationContainer, isolationVM} {
		t.Run(iso, func(t *testing.T) { statusDoctorGolden(t, iso, iso, false) })
	}
}

// On a Mac, status and doctor say so -- the engine OrbStack forwarding
// its own SSH agent -- whatever the build's
// GOOS and this machine's filesystem; under vm docker is still never
// asked (newBoxApp's engine fails the test on any question).
func TestStatusAndDoctorGoldenDarwin(t *testing.T) {
	for _, iso := range []string{isolationContainer, isolationVM} {
		t.Run(iso, func(t *testing.T) { statusDoctorGolden(t, iso, "darwin-"+iso, true) })
	}
}

// statusDoctorGolden runs status and doctor on a running sandbox, then
// on an absent one, against the golden files named for suffix.
func statusDoctorGolden(t *testing.T, iso, suffix string, darwin bool) {
	b := newBoxApp(t, iso, runningBox(iso))
	if darwin {
		pinHost(t, true)
		if iso != isolationVM {
			b.write(t, "os", "OrbStack")
		}
	}
	b.Cfg.VMCPUs, b.Cfg.VMMemory = 4, "6G"
	b.mountLocal("local/linux-arm64")
	if err := b.Status(); err != nil {
		t.Fatal(err)
	}
	out, errs := b.said()
	golden(t, b, "status-running-"+suffix+".out", out)
	golden(t, b, "status-running-"+suffix+".err", errs)

	_ = b.Doctor([]string{"--offline"})
	out, errs = b.said()
	golden(t, b, "doctor-running-"+suffix+".out", out)
	golden(t, b, "doctor-running-"+suffix+".err", errs)

	b.box.Status = "absent"
	if err := b.Status(); err != nil {
		t.Fatal(err)
	}
	out, errs = b.said()
	golden(t, b, "status-absent-"+suffix+".out", out)
	golden(t, b, "status-absent-"+suffix+".err", errs)
	_ = b.Doctor([]string{"--offline"})
	out, errs = b.said()
	golden(t, b, "doctor-absent-"+suffix+".out", out)
	golden(t, b, "doctor-absent-"+suffix+".err", errs)
}

// The host's zone comes through App's seam, never the machine's
// /etc/localtime: a zone the sandbox does not have shows in status, with
// its notes, and in doctor; one read from /etc/timezone counts the same.
func TestStatusAndDoctorHostZone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		link     string
		file     string
		wantZone string
	}{
		{"localtime", "/usr/share/zoneinfo/Europe/Lisbon", "", "Europe/Lisbon"},
		{"relative", "../usr/share/zoneinfo/Etc/UTC", "", "Etc/UTC"},
		{"timezone file", "", "Asia/Kolkata\n", "Asia/Kolkata"},
	} {
		for _, iso := range []string{isolationContainer, isolationVM} {
			t.Run(tc.name+"/"+iso, func(t *testing.T) {
				box := runningBox(iso)
				// The sandbox has no TZ: its bash -c runs for real, in an
				// empty environment.
				box.Exec = func(s backend.ExecSpec) *exec.Cmd {
					if len(s.Argv) == 3 && s.Argv[0] == "bash" && s.Argv[1] == "-c" {
						return exec.Command("env", "-i", "bash", "-c", s.Argv[2])
					}
					return backendtest.Fail()
				}
				b := newBoxApp(t, iso, box)
				b.mountLocal("local/linux-arm64")
				b.tzReadlink = func(p string) (string, error) {
					if p != "/etc/localtime" || tc.link == "" {
						return "", fs.ErrNotExist
					}
					return tc.link, nil
				}
				b.tzReadFile = func(p string) ([]byte, error) {
					if p != "/etc/timezone" || tc.file == "" {
						return nil, fs.ErrNotExist
					}
					return []byte(tc.file), nil
				}
				if err := b.Status(); err != nil {
					t.Fatal(err)
				}
				out, errs := b.said()
				if want := "timezone  : UTC (host: " + tc.wantZone + ")\n"; !strings.Contains(out, want) {
					t.Errorf("status lacks %q:\n%s", want, out)
				}
				if want := "new sessions get '" + tc.wantZone + "' anyway"; !strings.Contains(errs, want) {
					t.Errorf("status's notes lack %q:\n%s", want, errs)
				}
				_ = b.Doctor([]string{"--offline"})
				out, _ = b.said()
				if want := "the host " + tc.wantZone + " now"; !strings.Contains(out, want) {
					t.Errorf("doctor lacks %q:\n%s", want, out)
				}
			})
		}
	}
}
