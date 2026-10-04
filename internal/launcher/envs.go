package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
)

// Env is `caboose env [list]`. Environments are created by setup
// (`caboose -e NAME setup`), which asks first.
func (a *App) Env(args []string) error {
	if len(args) == 0 || len(args) == 1 && args[0] == "list" {
		return a.listEnvs()
	}
	return Die("usage: caboose env [list] ('caboose -e NAME setup' creates an environment)")
}

// listEnvs shows every environment, the current one marked, each with its
// container and that container's state.
func (a *App) listEnvs() error {
	names := map[string]bool{config.DefaultEnv: true, a.Cfg.Env: true}
	entries, err := os.ReadDir(filepath.Join(a.Cfg.CabooseHome, "envs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Die("%v", err)
	}
	for _, e := range entries {
		if e.IsDir() && config.ValidEnv(e.Name()) {
			names[e.Name()] = true
		}
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		// Another environment's isolation is its own config's, not read
		// here: its sandbox is asked of docker, as a container.
		if n == a.Cfg.Env {
			fmt.Fprintf(a.Stdout, "* %-16s %s %s (%s)\n", n, a.noun(), a.Cfg.Container, a.state())
			continue
		}
		container := config.ContainerFor(n)
		fmt.Fprintf(a.Stdout, "  %-16s container %s (%s)\n", n, container, backend.NewDocker(a.Docker, container).State())
	}
	return nil
}
