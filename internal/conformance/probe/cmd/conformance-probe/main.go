// Command conformance-probe is the conformance suite's probe
// (internal/conformance/probe), built outside the image and placed by the
// suite into its own throwaway sandbox. It is in no release.
package main

import (
	"os"

	"github.com/bfreis/caboose/internal/conformance/probe"
)

func main() {
	os.Exit(probe.Main("/work", os.Args[1:], os.Stdout, os.Stderr))
}
