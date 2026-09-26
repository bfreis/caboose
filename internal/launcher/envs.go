package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

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
		mark := " "
		container := config.ContainerFor(n)
		if n == a.Cfg.Env {
			mark, container = "*", a.Cfg.Container
		}
		fmt.Fprintf(a.Stdout, "%s %-16s container %s (%s)\n", mark, n, container, a.Docker.ContainerState(container))
	}
	return nil
}
