package probe

import (
	"os"
	"testing"
)

// TestMain runs the copy userNS starts: in a test, /proc/self/exe is this
// test binary, so its child mode has to be reached here as it is in Main.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == userNSChildArg {
		os.Exit(UserNSChild(os.Args[2], os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
