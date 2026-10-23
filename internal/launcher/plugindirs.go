package launcher

import (
	"fmt"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/datadir"
)

// Claude Code loads caboose's mod from the managed directory the sandbox
// has read-only (datadir.PluginDirs), named by CLAUDE_CODE_PLUGIN_DIRS in
// the sandbox's environment, under every isolation. The environment is
// fixed at creation, so the sandbox is labelled with the value
// (assets.LabelPluginDirs), and one that records none is told to caboose
// restart. A setting of the user's own that replaces the
// variable drops the mod; the host's count of sessions says so.

// withPluginDirs puts the mod's folder in spec's environment and labels it.
func withPluginDirs(spec *backend.Spec) {
	spec.Env = append(spec.Env, datadir.PluginDirsEnv+"="+datadir.PluginDirs)
	spec.Labels = append(spec.Labels, assets.LabelPluginDirs+"="+datadir.PluginDirs)
}

// pluginDirsDrift says how the sandbox's plugin folders differ from this
// launcher's, or "" when they do not or cannot be told.
func (a *App) pluginDirsDrift() string {
	if d := a.missingLabel(assets.LabelPluginDirs, "plugin folders setting (caboose's Claude Code mod)"); d != "" {
		return d
	}
	labels, err := a.box().Labels()
	if err != nil || labels[assets.LabelPluginDirs] == datadir.PluginDirs {
		return ""
	}
	return fmt.Sprintf("the %s was created with plugin folders %q; this caboose sets %q", a.noun(), labels[assets.LabelPluginDirs], datadir.PluginDirs)
}

// warnIfPluginDirsDrifted is warnIfRunArgsDrifted for the plugin folders.
func (a *App) warnIfPluginDirsDrifted() {
	if d := a.pluginDirsDrift(); d != "" {
		a.Note("%s.", d)
		a.Note("run 'caboose restart' to recreate it with it (this kills running sessions).")
	}
}
