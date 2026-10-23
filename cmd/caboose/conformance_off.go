//go:build !conformance

package main

import (
	"fmt"
	"io"
)

// runConformance, in a binary built without the conformance tag (every
// release), refuses: Parse accepts no "conformance" command there, and
// this keeps every reference to the suite out of the binary, so the
// linker leaves its code out.
func runConformance(_ []string, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, "caboose: this binary was built without the conformance suite")
	return 2
}
