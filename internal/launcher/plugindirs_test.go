package launcher

import (
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// Every isolation creates its sandbox with Claude Code's plugin folder
// naming caboose's mod, once, and labels it.
func TestCreateContainerPluginDirs(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := creating(t, iso, currentLabels("linux-x64"))
			if err := b.createContainer(false); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			spec, _ := b.box.Spec()
			want := "CLAUDE_CODE_PLUGIN_DIRS=/etc/claude-code/mods/caboose"
			n := 0
			for _, e := range spec.Env {
				if strings.HasPrefix(e, datadir.PluginDirsEnv+"=") {
					n++
					if e != want {
						t.Errorf("env %q, want %q", e, want)
					}
				}
			}
			if n != 1 {
				t.Errorf("%d entries for the variable: %q", n, spec.Env)
			}
			if !hasSeq(spec.Labels, assets.LabelPluginDirs+"=/etc/claude-code/mods/caboose") {
				t.Errorf("labels: %q", spec.Labels)
			}
		})
	}
}

// The mod's folder is where the managed directory is mounted.
func TestPluginDirsAreUnderTheManagedTarget(t *testing.T) {
	if !strings.HasPrefix(datadir.PluginDirs, datadir.ManagedTarget+"/mods/") {
		t.Errorf("%s is not under %s/mods", datadir.PluginDirs, datadir.ManagedTarget)
	}
	if !slices.Contains(ownedEnv, datadir.PluginDirsEnv) {
		t.Error("run_args may set the variable")
	}
}

// docker run arguments cannot set or replace the variable, in any form.
func TestRunArgsRefusePluginDirs(t *testing.T) {
	for _, arg := range []string{
		"--env=CLAUDE_CODE_PLUGIN_DIRS=x",
		"--env=CLAUDE_CODE_PLUGIN_DIRS",
		"--env=CLAUDE_CODE_PLUGIN_DIRS=",
	} {
		if err := checkRunArgs([]string{arg}, nil, nil); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_PLUGIN_DIRS") {
			t.Errorf("%s: err = %v", arg, err)
		}
	}
	for _, arg := range []string{"-e", "-e=CLAUDE_CODE_PLUGIN_DIRS=x", "-eCLAUDE_CODE_PLUGIN_DIRS=x"} {
		if err := checkRunArgs([]string{arg}, nil, nil); err == nil {
			t.Errorf("%s was accepted", arg)
		}
	}
	// The variable among caboose's own arguments, too.
	if err := checkRunArgs([]string{"--env=FOO=1"}, []string{"-e", "CLAUDE_CODE_PLUGIN_DIRS=/etc/claude-code/mods/caboose"}, nil); err != nil {
		t.Errorf("an unrelated variable: %v", err)
	}
	if err := checkRunArgs([]string{"--env-file=/x"}, nil, nil); err == nil {
		t.Error("--env-file was accepted")
	}
}

// A sandbox made before the label has no mod, and is told to restart; so is
// one made with other folders.
func TestPluginDirsDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		drift  string
	}{
		{"same", map[string]string{assets.LabelPluginDirs: datadir.PluginDirs}, ""},
		{"no label", map[string]string{}, "records no plugin folders setting"},
		{"no labels", nil, "records no plugin folders setting"},
		{"other folders", map[string]string{assets.LabelPluginDirs: "/x"}, `created with plugin folders "/x"`},
		{"empty", map[string]string{assets.LabelPluginDirs: ""}, `created with plugin folders ""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := &backendtest.Fake{Status: "running", SandboxLabels: tc.labels}
			a := &App{Cfg: &config.Config{Env: "default", Container: "box"}, Backend: box}
			got := a.pluginDirsDrift()
			if (tc.drift == "") != (got == "") || !strings.Contains(got, tc.drift) {
				t.Errorf("drift %q, want %q", got, tc.drift)
			}
		})
	}
}

func TestModsLine(t *testing.T) {
	for _, tc := range []struct {
		sessions, with int
		want           string
	}{
		{0, 0, "no Claude Code sessions running"},
		{1, 1, "1 Claude Code session, all with caboose's mod"},
		{3, 3, "3 Claude Code sessions, all with caboose's mod"},
		{3, 1, "3 Claude Code sessions, 2 without caboose's mod (a settings env replacing CLAUDE_CODE_PLUGIN_DIRS drops it)"},
		{1, 0, "1 Claude Code session, 1 without caboose's mod (a settings env replacing CLAUDE_CODE_PLUGIN_DIRS drops it)"},
	} {
		if got := modsLine(tc.sessions, tc.with); got != tc.want {
			t.Errorf("%d/%d: %q, want %q", tc.sessions, tc.with, got, tc.want)
		}
	}
}
