package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/proposal"
)

const settingsP = "title = \"Settings\"\nreason = \"my zone\"\n[settings.session]\ntz = \"Europe/Lisbon\"\ntmux = false\n[settings.build]\nauto_build = false\n"

const vmConfig = "isolation = \"vm.default\"\n\n[vm.default]\ncpus = 2\n"

// vmSettingsEnv is an environment under vm, whose machine has 8 CPUs and
// 16 GiB.
func vmSettingsEnv(t *testing.T) *setupEnv {
	t.Helper()
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig(vmConfig)
	e.a.Cfg.VMCPUs = 2
	cpus, mem := settingsHostCPUs, settingsHostMemory
	settingsHostCPUs, settingsHostMemory = func() int { return 8 }, func() uint64 { return 16 << 30 }
	t.Cleanup(func() { settingsHostCPUs, settingsHostMemory = cpus, mem })
	return e
}

func TestApplySettings(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("# mine\n[session]\n# the zone\n#tz = \"\"\nauto_sync = true\n")
	e.a.Cfg.Tmux, e.a.Cfg.AutoBuild = true, true
	p := e.propose("p.toml", settingsP)
	before := e.configFile()

	if err := e.apply("2\n"); err != nil || !e.said("Left pending") || e.configFile() != before {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	there(t, p)

	if err := e.apply("\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("session.tz", "session.tmux", "build.auto_build", "Wrote session.tz", "settings applied need no restart")
	got := e.configFile()
	for _, want := range []string{"# mine", "tz = \"Europe/Lisbon\"", "tmux = false", "[build]", "auto_build = false", "auto_sync = true"} {
		if !strings.Contains(got, want) {
			t.Errorf("config.toml lacks %q:\n%s", want, got)
		}
	}
	gone(t, p)
	if f := e.file(); f.Vals["session.tz"] != "Europe/Lisbon" {
		t.Errorf("tz %v", f.Vals["session.tz"])
	}
}

// A key that needs a restart gets the restart offered.
func TestApplySettingsHostnameOffersRestart(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("")
	e.propose("p.toml", "title = \"h\"\n[settings.session]\nhostname = \"sbx\"\n")
	if err := e.apply("\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if !strings.Contains(e.configFile(), `hostname = "sbx"`) {
		t.Errorf("config.toml:\n%s", e.configFile())
	}
	if e.said("settings applied need no restart") {
		t.Errorf("a hostname needs the restart:\n%s", e.errb)
	}
}

func TestSettingsVMKeys(t *testing.T) {
	// Not under vm: refused, whole.
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("")
	_, refusals, _ := e.a.planSettings(proposal.Settings{CPUs: ptr(4)})
	if r := strings.Join(refusals, "\n"); !strings.Contains(r, "this environment's isolation is container") {
		t.Errorf("under container: %q", r)
	}

	// Under vm with no profile.
	e = newSetupEnv(t, "default", "", false)
	e.writeConfig("")
	e.a.Cfg.Isolation, e.a.Cfg.Profile = config.KindVM, ""
	_, refusals, _ = e.a.planSettings(proposal.Settings{Memory: ptr("8G")})
	if r := strings.Join(refusals, "\n"); !strings.Contains(r, "no [vm.NAME] profile") {
		t.Errorf("no profile: %q", r)
	}

	// Under vm with a profile: written into it.
	e = vmSettingsEnv(t)
	pl, refusals, _ := e.a.planSettings(proposal.Settings{CPUs: ptr(6), Memory: ptr("12G")})
	if len(refusals) > 0 || pl == nil {
		t.Fatalf("%v", refusals)
	}
	if pl.edit.Set["vm.default.cpus"] != 6 || pl.edit.Set["vm.default.memory"] != "12G" || !pl.restart {
		t.Errorf("%+v", pl)
	}
	if !strings.Contains(pl.diff, "cpus = 6") || !strings.Contains(pl.diff, `memory = "12G"`) {
		t.Errorf("diff:\n%s", pl.diff)
	}

	// Past this machine.
	_, refusals, _ = e.a.planSettings(proposal.Settings{CPUs: ptr(9)})
	if r := strings.Join(refusals, "\n"); !strings.Contains(r, "has 8 CPUs") {
		t.Errorf("cpus: %q", r)
	}
	_, refusals, _ = e.a.planSettings(proposal.Settings{Memory: ptr("17G")})
	if r := strings.Join(refusals, "\n"); !strings.Contains(r, "this machine has 16384 MiB") {
		t.Errorf("memory: %q", r)
	}
}

func ptr[T any](v T) *T { return &v }

// A value config.toml has already is dropped with a warning, and with
// nothing left there is nothing to apply.
func TestSettingsUnchangedDropped(t *testing.T) {
	e := vmSettingsEnv(t)
	e.a.Cfg.TZ, e.a.Cfg.Tmux, e.a.Cfg.VMMemory = "UTC", true, "8G"
	pl, refusals, warnings := e.a.planSettings(proposal.Settings{TZ: ptr("UTC"), Tmux: ptr(true), CPUs: ptr(2), Memory: ptr("8192M")})
	if pl != nil || len(refusals) > 0 || len(warnings) != 4 {
		t.Fatalf("%+v %v %v", pl, refusals, warnings)
	}
	pl, _, warnings = e.a.planSettings(proposal.Settings{TZ: ptr("UTC"), Tmux: ptr(false)})
	if pl == nil || len(pl.keys) != 1 || pl.keys[0].key != "session.tmux" || len(warnings) != 1 {
		t.Errorf("%+v %v", pl, warnings)
	}

	e.propose("p.toml", "title = \"t\"\n[settings.session]\ntz = \"UTC\"\n")
	if err := e.apply("\n"); err != nil || !e.said("nothing left in it to apply") {
		t.Fatalf("%v\n%s", err, e.errb)
	}
}

func TestProposalViewSettings(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("[session]\ntz = \"UTC\"\n")
	e.a.Cfg.TZ, e.a.Cfg.Tmux, e.a.Cfg.AutoBuild = "UTC", true, true
	e.propose("p.toml", settingsP)
	v := onlyView(t, e)
	if !v.WebApply || v.Confirm != ConfirmNone || len(v.Parts) != 1 || v.Parts[0].Kind != PartSettings {
		t.Fatalf("%+v", v)
	}
	if s := v.Parts[0].Summary; !strings.Contains(s, `session.tz = "Europe/Lisbon"`) || !strings.Contains(s, "auto_build = false") {
		t.Errorf("summary %q", s)
	}
	joined := strings.Join(v.Parts[0].Diff, "\n")
	if !strings.Contains(joined, `+ tz = "Europe/Lisbon"`) && !strings.Contains(joined, `tz = "Europe/Lisbon"`) {
		t.Errorf("diff %q", v.Parts[0].Diff)
	}
	entries, _ := proposal.List(e.a.Cfg.DataDir)
	before := e.configFile()

	if _, err := e.a.ApplyPlanned(entries[0], strings.Repeat("0", 64), ""); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("a stale hash: %v", err)
	}
	// config.toml edited after the view.
	e.writeConfig(before + "\n[link]\nforward_ports = [3000]\n")
	if _, err := e.a.ApplyPlanned(entries[0], v.Hash, ""); daemonproto.Kind(err) != daemonproto.KindStaleDiff {
		t.Errorf("config.toml edited: %v", err)
	}
	e.writeConfig(before)

	notes, err := e.a.ApplyPlanned(entries[0], v.Hash, "")
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Join(notes, "\n")
	for _, want := range []string{"Wrote session.tz to ", "Wrote session.tmux to ", "Wrote build.auto_build to "} {
		if !strings.Contains(n, want) {
			t.Errorf("notes lack %q: %q", want, notes)
		}
	}
	if got := e.configFile(); !strings.Contains(got, `tz = "Europe/Lisbon"`) || !strings.Contains(got, "auto_build = false") {
		t.Errorf("config.toml:\n%s", got)
	}
}

func TestExportProposalsSettings(t *testing.T) {
	e := vmSettingsEnv(t)
	e.a.Cfg.TZ, e.a.Cfg.AutoBuild = "UTC", true
	e.mkdirAll(e.a.Cfg.DataDir)
	e.a.exportProposals()
	data := readState(t, e)
	for _, want := range []string{"[settings]", `isolation = "vm"`, `vm_profile = "vm.default"`, `tz = "UTC"`, "cpus = 2", "auto_build = true"} {
		if !strings.Contains(data, want) {
			t.Errorf("state.toml lacks %q:\n%s", want, data)
		}
	}
}

func readState(t *testing.T, e *setupEnv) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.a.Cfg.DataDir, proposal.Dir, proposal.CurrentDir, "state.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *setupEnv) mkdirAll(dir string) {
	e.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
}

// config.toml edited between the diff shown and the write: nothing is
// written, and the proposal stays.
func TestApplySettingsPlansAgain(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("[session]\nauto_sync = true\n")
	p := e.propose("p.toml", "title = \"t\"\n[settings.session]\ntz = \"UTC\"\n")
	e.a.beforeProposalWrite = func() {
		e.a.beforeProposalWrite = nil
		e.writeConfig("[session]\nauto_sync = false\n")
	}
	if err := e.apply("1\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	e.wantOut("config.toml changed since it was shown")
	if strings.Contains(e.configFile(), "tz") {
		t.Errorf("written:\n%s", e.configFile())
	}
	there(t, p)
}

// An alias and settings applied in one run get one closing message that
// covers both.
func TestApplyAliasAndSettings(t *testing.T) {
	e := newSetupEnv(t, "default", "", false)
	e.writeConfig("")
	cmd := tool(t, filepath.Join(filepath.Dir(e.a.Cfg.Home), "bin", "awake"))
	e.propose("awake.toml", strings.Replace(awakeProposal, "%s", cmd, 1))
	e.propose("settings.toml", settingsP)
	if err := e.apply("1\nawake\n\n"); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if !strings.Contains(e.configFile(), "[alias.awake]") || !strings.Contains(e.configFile(), "tz = ") {
		t.Fatalf("both not applied with these answers:\n%s\n%s", e.configFile(), e.errb)
	}
	e.wantOut("aliases and settings applied need no build and no restart")
}
