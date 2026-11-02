package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

const volumeP = "title = \"A cache\"\nreason = \"go builds start cold\"\n[volume]\nname = \"gocache\"\npath = \"~/.cache/go-build\"\nsize = \"40G\"\n"

// A volume proposal under vm adds [PROFILE.volume.NAME] to the profile in
// use, the file's comments kept, and is removed once applied.
func TestApplyVolume(t *testing.T) {
	e := vmSettingsEnv(t)
	e.writeConfig("isolation = \"vm.default\"\n\n# mine\n[vm.default]\ncpus = 2\n")
	p := e.propose("p.toml", volumeP)
	before := e.configFile()
	if err := e.apply("2\n"); err != nil || !e.said("Left pending") || e.configFile() != before {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	there(t, p)
	if err := e.apply("\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("A volume of vm.default ([vm.default.volume.gocache])", "Wrote [vm.default.volume.gocache]", "caboose restart")
	f := e.file()
	vols := f.Volumes["vm.default"]
	if len(vols) != 1 || vols[0].Name != "gocache" || vols[0].Size != 40<<30 || vols[0].Path != config.ContainerHome+"/.cache/go-build" {
		t.Fatalf("volumes %+v\n%s", vols, e.configFile())
	}
	if !strings.Contains(e.configFile(), "# mine") {
		t.Errorf("the comment went:\n%s", e.configFile())
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the proposal is still there: %v", err)
	}
}

// What the host refuses of a volume proposal: no profile in use, a volume
// the profile has, a size outside vm, and whatever config.toml would refuse
// once written (a root's path, nesting, too many); nothing is written.
func TestPlanVolumeRefusals(t *testing.T) {
	vol := func(name, path, size string) proposal.Volume {
		return proposal.Volume{Name: name, Path: path, Size: size}
	}
	many := "isolation = \"vm.default\"\n[vm.default]\n"
	for i := range config.MaxVMVolumes {
		many += fmt.Sprintf("[vm.default.volume.v%d]\npath = \"/v%d\"\n", i, i)
	}
	for _, tc := range []struct {
		name, config string
		vol          proposal.Volume
		want         string
	}{
		{"no profile", "[session]\ntmux = true\n", vol("data", "/data", ""), "config.toml defines none in use (isolation container)"},
		{"there already, vm", vmConfig + "[vm.default.volume.data]\npath = \"/data\"\n", vol("data", "/other", "8G"),
			"[vm.default.volume.data] is in config.toml already, at /data: a new size is asked for with caboose-agent request volume-resize"},
		{"there already, container", "[container.default]\n[container.default.volume.data]\npath = \"/data\"\n", vol("data", "/other", ""), "'caboose volume rm data' on the host first"},
		{"a size under container", "[container.default]\n", vol("data", "/data", "8G"), "Under container a volume has no size"},
		{"a root's path", vmConfig, vol("data", "/work/dev/x", ""), "at, inside or around root dev"},
		{"nesting", vmConfig + "[vm.default.volume.data]\npath = \"/data\"\n", vol("inner", "/data/x", ""), "nest"},
		{"too many", many, vol("data", "/data", ""), "takes at most 19"},
	} {
		e := vmSettingsEnv(t)
		e.writeConfig(tc.config)
		before := e.configFile()
		pl, refusals, _ := e.a.planVolume(tc.vol)
		if pl != nil || len(refusals) != 1 || !strings.Contains(refusals[0], tc.want) {
			t.Errorf("%s: %+v %q", tc.name, pl, refusals)
		}
		if e.configFile() != before {
			t.Errorf("%s: config.toml changed", tc.name)
		}
	}
}

// Under vm a volume with no size gets the default; under container it has
// none, and the warning says caboose attaches none there yet.
func TestPlanVolumeSizes(t *testing.T) {
	e := vmSettingsEnv(t)
	pl, refusals, _ := e.a.planVolume(proposal.Volume{Name: "data", Path: "/data"})
	if pl == nil || len(refusals) > 0 || pl.edit.Set["vm.default.volume.data.size"] != "64G" || pl.edit.Set["vm.default.volume.data.path"] != "/data" {
		t.Fatalf("%+v %q", pl, refusals)
	}
	e.writeConfig("[container.default]\n")
	pl, refusals, warnings := e.a.planVolume(proposal.Volume{Name: "data", Path: "/data"})
	if pl == nil || len(refusals) > 0 || pl.edit.Set["container.default.volume.data.size"] != nil {
		t.Fatalf("%+v %q", pl, refusals)
	}
	if w := strings.Join(warnings, "\n"); !strings.Contains(w, "Under container caboose makes no volumes yet") {
		t.Errorf("warnings %q", w)
	}
	// A size past the space free is a warning, not a refusal; a disk
	// already there is named.
	e = vmSettingsEnv(t)
	vmFreeSpace = func(string) (int64, error) { return 10 << 30, nil }
	if _, refusals, w := e.a.planVolume(proposal.Volume{Name: "data", Path: "/data", Size: "40G"}); len(refusals) > 0 || !strings.Contains(strings.Join(w, "\n"), "has 10G free") {
		t.Errorf("free space: %q %q", refusals, w)
	}
	disk := e.a.volumeDisk("data")
	if err := os.MkdirAll(filepath.Dir(disk), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(disk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, w := e.a.planVolume(proposal.Volume{Name: "data", Path: "/data"}); !strings.Contains(strings.Join(w, "\n"), "is here already") {
		t.Errorf("a disk there: %q", w)
	}
}

// The web page applies a volume proposal as one part, confirmed by the diff
// alone, and only while config.toml is what was shown.
func TestProposalViewVolume(t *testing.T) {
	e := vmSettingsEnv(t)
	e.propose("p.toml", volumeP)
	v := onlyView(t, e)
	if !v.WebApply || v.Confirm != ConfirmNone || len(v.Parts) != 1 || v.Parts[0].Kind != PartVolume {
		t.Fatalf("%+v", v)
	}
	if s := v.Parts[0].Summary; !strings.Contains(s, "add volume gocache to vm.default, mounted at ~/.cache/go-build, of up to 40G") {
		t.Errorf("summary %q", s)
	}
	if _, err := (&daemonproto.ProposalInfo{File: "p.toml", Title: "t", Hash: v.Hash, Confirm: daemonproto.ProposalConfirm{Kind: daemonproto.ProposalConfirmNone},
		Parts: []daemonproto.ProposalPart{{Kind: daemonproto.ProposalPartVolume, Summary: "s"}}}).Validate(); err != nil {
		t.Errorf("the protocol refuses a volume part: %v", err)
	}
	entries, _ := proposal.List(e.a.Cfg.DataDir)
	before := e.configFile()
	e.writeConfig(before + "\n[link]\nforward_ports = [3000]\n")
	if _, err := e.a.ApplyPlanned(entries[0], v.Hash, ""); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("config.toml edited: %v", err)
	}
	e.writeConfig(before)
	notes, err := e.a.ApplyPlanned(entries[0], v.Hash, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Join(notes, "\n"); !strings.Contains(n, "Wrote [vm.default.volume.gocache] to ") {
		t.Errorf("notes %q", notes)
	}
	if got := e.configFile(); !strings.Contains(got, "[vm.default.volume.gocache]") || !strings.Contains(got, `size = "40G"`) {
		t.Errorf("config.toml:\n%s", got)
	}
}

// What a proposal is made against tells sessions the profile's volumes and
// its volume_limit.
func TestExportProposalsVolumes(t *testing.T) {
	e := vmSettingsEnv(t)
	e.writeConfig(vmConfig + "volume_limit = \"300G\"\n[vm.default.volume.cache]\npath = \"~/.cache/go-build\"\nsize = \"48G\"\n")
	cfg, err := config.Load(e.a.Cfg.Getenv, config.OSFS{}, "default")
	if err != nil {
		t.Fatal(err)
	}
	e.a.Cfg.Volumes, e.a.Cfg.VMVolumeLimit = cfg.Volumes, cfg.VMVolumeLimit
	e.mkdirAll(e.a.Cfg.DataDir)
	e.a.exportProposals()
	data := readState(t, e)
	for _, want := range []string{`volume_profile = "vm.default"`, `volume_limit = "300G"`, "[volumes.cache]", `path = "/home/agent/.cache/go-build"`, `size = "48G"`} {
		if !strings.Contains(data, want) {
			t.Errorf("state.toml lacks %q:\n%s", want, data)
		}
	}
	// No profile in use: none can be proposed, and none is listed.
	e.writeConfig("[session]\ntmux = true\n")
	e.a.Cfg.Volumes = nil
	e.a.exportProposals()
	var keys []string
	for _, l := range strings.Split(readState(t, e), "\n") {
		if !strings.HasPrefix(l, "#") {
			keys = append(keys, l)
		}
	}
	if data := strings.Join(keys, "\n"); !strings.Contains(data, `volume_profile = ""`) || strings.Contains(data, "[volumes") || strings.Contains(data, "volume_limit") {
		t.Errorf("no profile:\n%s", data)
	}
}

// config.toml changed between showing a volume proposal and writing it:
// nothing is written, and the proposal stays; a change that makes it
// refused says so.
func TestApplyVolumeChangedMeanwhile(t *testing.T) {
	e := vmSettingsEnv(t)
	p := e.propose("p.toml", volumeP)
	e.a.beforeProposalWrite = func() {
		e.a.beforeProposalWrite = nil
		e.writeConfig(vmConfig + "memory = \"4G\"\n")
	}
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("config.toml changed since it was shown")
	if strings.Contains(e.configFile(), "gocache") {
		t.Errorf("written:\n%s", e.configFile())
	}
	there(t, p)

	e.a.beforeProposalWrite = func() {
		e.a.beforeProposalWrite = nil
		e.writeConfig(vmConfig + "[vm.default.volume.gocache]\npath = \"/elsewhere\"\n")
	}
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("It changed since it was shown")
	if strings.Contains(e.configFile(), "go-build") {
		t.Errorf("written:\n%s", e.configFile())
	}
	there(t, p)
}
