// Command caboose attaches a Claude Code session to the long-lived caboose
// sandbox container. See internal/launcher for how it works, and Usage
// (`caboose help`) for its commands and flags. Nothing reaches `claude` but
// what follows `caboose claude`; any other word caboose does not know is
// refused, with what to type instead.
//
// restart and stop name the sessions they are about to end and ask first;
// FORCE=1 skips the question, and with no tty they refuse outright.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/launcher"
)

// sourceDir may be stamped at build time with
// -ldflags "-X main.sourceDir=/path/to/checkout". It is only a hint: without
// it the checkout is found next to the executable (see
// launcher.FindCheckout).
var sourceDir string

func main() {
	// Docker Desktop's CLI follows a failed `docker exec -t` with an
	// advert for Docker Debug ("What's next: ..."), under whatever caboose
	// or git said about the failure. Every docker caboose runs, the attach
	// included, inherits this; a value of the user's own is kept.
	if _, set := os.LookupEnv("DOCKER_CLI_HINTS"); !set {
		os.Setenv("DOCKER_CLI_HINTS", "false")
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr io.Writer) int {
	// A command line caboose does not accept, and help, come before
	// anything else: neither needs a configuration, nor may a broken one
	// stand in their way.
	inv, err := Parse(argv)
	if err != nil {
		return usageError(stderr, err)
	}
	if inv.Command == "help" {
		if len(inv.Args) == 1 {
			fmt.Fprint(stdout, CommandUsage(inv.Args[0]))
		} else {
			fmt.Fprint(stdout, Usage())
		}
		return 0
	}

	cfg, err := config.Load(os.Getenv, config.OSFS{}, inv.Env)
	switch {
	case err != nil && inv.Command == "update":
		// Machine-wide: no environment's config.toml stands in its way.
		cfg = config.Machine(os.Getenv)
	case err != nil:
		return exit(stderr, launcher.Die("%v", err))
	}

	app := &launcher.App{
		Cfg:    cfg,
		Docker: &docker.CLI{Path: "docker", Stderr: stderr},
		Stdout: stdout,
		Stderr: stderr,
	}
	// Neither needs the roots, and setup, which runs before they are
	// resolved, can end in a launch.
	app.Checkout = launcher.FindCheckout(sourceDir)
	app.Suffix = cfg.Session
	if inv.Session != "" {
		app.Suffix = inv.Session
	}
	// Building, asking for the version, checking an image and doctor need
	// no repo root; everything else does. version and doctor above all must
	// not: they are what to run when the setup looks wrong. Nor
	// check-image, which is what to run before there is a setup.
	if inv.Command == "update" {
		// Machine-wide, like the binary it replaces: no environment.
		return exit(stderr, app.Update(inv.Args))
	}
	if inv.Command == "link" {
		// A detached helper a launch starts: no update check, and nothing
		// said on a terminal it does not have.
		if err := cfg.CheckEnv(config.OSFS{}); err != nil {
			return exit(stderr, launcher.Die("%v", err))
		}
		return exit(stderr, app.Link(inv.Args))
	}
	// Every other command may start an update in the background, and says
	// once that one happened.
	app.AutoUpdate()
	switch inv.Command {
	case "env":
		return exit(stderr, app.Env(inv.Args))
	case "setup":
		// Before CheckEnv: setup is how an environment comes to exist, and
		// asks before creating one.
		return exit(stderr, app.Setup(inv.Args))
	}
	// Every other command works in the environment, which must exist.
	if err := cfg.CheckEnv(config.OSFS{}); err != nil {
		return exit(stderr, launcher.Die("%v", err))
	}
	switch inv.Command {
	case "build":
		return exit(stderr, app.Build(inv.Args))
	case "version":
		return exit(stderr, app.Version())
	case "check-image":
		return exit(stderr, app.CheckImage(inv.Args))
	case "doctor":
		// Before the roots are resolved: a missing root is one of the
		// problems it reports, not a reason to stop.
		return exit(stderr, app.Doctor(inv.Args))
	case "apply":
		// Before the roots are resolved too: a proposal may be what fixes
		// them, and the restart it ends in resolves them itself.
		return exit(stderr, app.Apply())
	}
	if err := cfg.ResolveRoots(); err != nil {
		return exit(stderr, launcher.Die("%v", err))
	}

	switch inv.Command {
	case "status":
		err = app.Status()
	case "stop":
		err = app.Stop()
	case "restart":
		err = app.Restart()
	case "prune":
		err = app.Prune()
	case "detach":
		err = app.Detach()
	case "logs":
		err = app.Logs(inv.Args)
	case "shell":
		err = app.Shell(inv.Args)
	case "sync":
		err = app.Sync(inv.Args)
	case "sandbox-config":
		err = app.SandboxConfig(inv.Args)
	default:
		// No command, or claude: its arguments are claude's.
		err = app.Attach(inv.Args)
	}
	return exit(stderr, err)
}

// usageError reports a command line caboose does not accept, and what to
// type instead: exit 2, as a usage error conventionally is.
func usageError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "caboose: %v\n", err)
	var ue *UsageError
	if errors.As(err, &ue) && ue.Hint != "" {
		for _, l := range strings.Split(ue.Hint, "\n") {
			fmt.Fprintf(stderr, "  %s\n", l)
		}
	}
	return 2
}

func exit(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	var ee *launcher.ExitError
	if errors.As(err, &ee) {
		if ee.Msg != "" {
			fmt.Fprintf(stderr, "caboose: %s\n", ee.Msg)
		}
		return ee.Code
	}
	fmt.Fprintf(stderr, "caboose: %v\n", err)
	return 1
}
