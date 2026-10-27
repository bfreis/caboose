package proposal

import (
	"strings"
	"testing"
)

const allSettings = `
[settings.session]
tz = "Europe/Lisbon"
hostname = "sbx-1"
tmux = false
[settings.build]
auto_build = false
[settings.vm]
cpus = 6
memory = "12G"
`

func TestSettingsProposal(t *testing.T) {
	p, err := Parse("s.toml", []byte("title = \"Settings\"\n"+allSettings))
	if err != nil {
		t.Fatal(err)
	}
	s := p.Settings
	if s == nil || p.Root != nil || p.Alias != nil || p.Writable != nil {
		t.Fatalf("%+v", p)
	}
	if *s.TZ != "Europe/Lisbon" || *s.Hostname != "sbx-1" || *s.Tmux || *s.AutoBuild || *s.CPUs != 6 || *s.Memory != "12G" {
		t.Errorf("%+v", s)
	}

	// Dotted keys decode the same.
	dotted := "title = \"Settings\"\n[settings]\nsession.tz = \"Europe/Lisbon\"\nsession.hostname = \"sbx-1\"\nsession.tmux = false\nbuild.auto_build = false\nvm.cpus = 6\nvm.memory = \"12G\"\n"
	q, err := Parse("s.toml", []byte(dotted))
	if err != nil {
		t.Fatal(err)
	}
	if *q.Settings.TZ != *s.TZ || *q.Settings.CPUs != *s.CPUs || *q.Settings.Tmux != *s.Tmux || *q.Settings.Memory != *s.Memory || *q.Settings.AutoBuild != *s.AutoBuild || *q.Settings.Hostname != *s.Hostname {
		t.Errorf("dotted %+v", q.Settings)
	}

	// One key is a proposal, and false is a value.
	r, err := Parse("s.toml", []byte("title = \"t\"\n[settings.session]\ntmux = false\n"))
	if err != nil || r.Settings.Tmux == nil || *r.Settings.Tmux || r.Settings.TZ != nil || r.Settings.CPUs != nil {
		t.Errorf("%+v %v", r, err)
	}
}

func TestSettingsProposalRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"empty", "[settings]\n", "[settings] proposes nothing"},
		{"empty session", "[settings.session]\n", "[settings] proposes nothing"},
		{"unknown key", "[settings.session]\nauto_sync = true\n", "settings.session.auto_sync cannot be proposed"},
		{"unknown table", "[settings.link]\nforward_ports = [22]\n", "settings.link"},
		{"unknown top", "[settings]\nisolation = \"gvisor.default\"\n", "settings.isolation cannot be proposed"},
		{"another vm key", "[settings.vm]\negress = true\n", "settings.vm.egress cannot be proposed"},
		{"with a root", "[settings.session]\ntmux = true\n[roots]\nx = \"~/x\"\n", "proposed alone"},
		{"with packages", "[settings.session]\ntmux = true\n[packages]\nadd = [\"jq\"]\n", "proposed alone"},
		{"with a writable", "[settings.session]\ntmux = true\n[writable]\npath = \"/work/dev/p\"\n", "proposed alone"},
		{"with an alias", "[settings.session]\ntmux = true\n[alias.t]\ncommand = [\"/bin/true\"]\n", "proposed alone"},
		{"tz path", "[settings.session]\ntz = \"../../etc/passwd\"\n", "not a time zone"},
		{"tz empty", "[settings.session]\ntz = \"\"\n", "not a time zone"},
		{"tz escape", "[settings.session]\ntz = \"UTC\\u001b[2J\"\n", "character U+001B"},
		{"tz long", "[settings.session]\ntz = \"" + strings.Repeat("a", MaxSettingText+1) + "\"\n", "longer than"},
		{"hostname empty", "[settings.session]\nhostname = \"\"\n", "hostname is empty"},
		{"hostname dots", "[settings.session]\nhostname = \"a.b\"\n", "not a hostname"},
		{"tmux string", "[settings.session]\ntmux = \"yes\"\n", "not a proposal"},
		{"cpus zero", "[settings.vm]\ncpus = 0\n", "settings.vm.cpus is 0"},
		{"cpus huge", "[settings.vm]\ncpus = 257\n", "settings.vm.cpus is 257"},
		{"cpus string", "[settings.vm]\ncpus = \"4\"\n", "not a proposal"},
		{"memory bad", "[settings.vm]\nmemory = \"lots\"\n", "settings.vm.memory"},
		{"memory small", "[settings.vm]\nmemory = \"100M\"\n", "under 512M"},
		{"memory escape", "[settings.vm]\nmemory = \"8G\\u202e\"\n", "character U+202E"},
	} {
		_, err := Parse("s.toml", []byte("title = \"t\"\n"+tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}
