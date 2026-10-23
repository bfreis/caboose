// Package nodocker keeps a package's tests off the real docker engine.
//
// The packages that drive docker (the launcher, the backends, the docker
// client, the command and the install script's tests) fake it in every
// test, by a stub first on a PATH of the test's own or a CLI pointed at a
// stub. A test that forgets would reach whatever docker the machine has:
// on a developer's machine the user's own engine, on CI the runner's. Run,
// called from TestMain, makes that a failure instead: the docker it puts
// first on PATH records each call and fails it, and the run fails if
// anything called it, even a test that ignored the error.
package nodocker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/saferm"
)

// calls is the file, beside the stub's directory, that it appends each
// call's arguments to.
const calls = "calls"

// stub finds its log from its own path ("$0" is .../bin/docker), so that a
// test that hands its commands a scrubbed environment is still recorded.
const stub = `#!/bin/sh
printf '%s\n' "docker $*" >>"${0%/*}/../` + calls + `"
echo "docker: a test reached the real docker; fake it (see internal/nodocker)" >&2
exit 125
`

// Install puts the failing docker first on PATH and clears DOCKER_HOST and
// DOCKER_CONTEXT, in dir, which must be empty. It returns a function that
// reports the calls the stub recorded, one per line.
func Install(dir string) (recorded func() []string, err error) {
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
		return nil, err
	}
	path := bin
	if p := os.Getenv("PATH"); p != "" {
		path += string(os.PathListSeparator) + p
	}
	if err := os.Setenv("PATH", path); err != nil {
		return nil, err
	}
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		if err := os.Unsetenv(k); err != nil {
			return nil, err
		}
	}
	log := filepath.Join(dir, calls)
	return func() []string {
		b, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}, nil
}

// Run installs the failing docker in a temp dir of its own, runs the tests
// and returns their exit code, which is 1 when they passed but something
// called docker. TestMain passes the result to os.Exit.
func Run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "nodocker-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodocker:", err)
		return 1
	}
	defer func() { _ = saferm.RemoveAll(os.TempDir(), dir) }()
	recorded, err := Install(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodocker:", err)
		return 1
	}
	code := m.Run()
	if got := recorded(); len(got) > 0 {
		fmt.Fprintf(os.Stderr, "nodocker: the tests ran docker %d time(s), which reaches the real engine; fake it:\n", len(got))
		for i, c := range got {
			if i == 10 {
				fmt.Fprintf(os.Stderr, "\t... and %d more\n", len(got)-i)
				break
			}
			fmt.Fprintf(os.Stderr, "\t%s\n", c)
		}
		if code == 0 {
			code = 1
		}
	}
	return code
}
