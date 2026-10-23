//go:build conformance

package main

import (
	"io"

	"github.com/bfreis/caboose/internal/launcher"
)

// The conformance suite's command, in a binary built with -tags
// conformance (make conformance) and in no other: a release has no
// command that creates environments to attack them.
func init() {
	Commands = append(Commands, Command{Name: "conformance", Usage: "ACTION", Args: ownArgs, Hidden: true,
		Summary: "run the security conformance suite",
		Help: `caboose conformance run --isolation container|gvisor|vm
                        [--trust fallible|hostile] [--out FILE] [--skip-wipe]
                        [--only ID,ID...] [--keep] [--probe-dir DIR]
caboose conformance list | leftovers | verify-doc
caboose conformance matrix [FILE...]

'run' creates an environment of its own under a new directory in /tmp,
has its sandbox try what a sandbox must not do, judges each attempt from
the host, writes the results (never over a file), and removes the
environment. It refuses to run inside a sandbox.
'list' prints the checks; 'leftovers' lists what an interrupted run left,
with the commands that remove it, and removes nothing; 'verify-doc' checks
docs/security.md's check ids and its matrix against the suite; 'matrix' merges
results files into docs/security-matrix.json (the latest verdict per check and
isolation) and rewrites the matrix in docs/security.md, or only re-renders it
with no file. It needs the checkout; exit 2 for a file it refuses.

Exit status: 0 every check passed (or was a gap or skipped), 1 a check
failed, 2 a check was an error or the suite refused to start or go on.`})
}

// runConformance is `caboose conformance ARGS`. It runs before any
// configuration is loaded, so nothing of the caller's environment or
// config.toml reaches the throwaway environment it makes. Only this file
// calls into the suite, so a binary built without the tag links none of
// it.
func runConformance(args []string, stdout, stderr io.Writer) int {
	return launcher.Conformance(args, launcher.DefaultConformanceIO(stdout, stderr, launcher.FindCheckout(sourceDir)))
}
