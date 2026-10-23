package backend

import (
	"os"
	"testing"

	"github.com/bfreis/caboose/internal/nodocker"
)

// beforeTests, when a platform's tests set it, runs before any test: the
// test binary may be started as a helper rather than to run tests.
var beforeTests func()

func TestMain(m *testing.M) {
	if beforeTests != nil {
		beforeTests()
	}
	os.Exit(nodocker.Run(m))
}
