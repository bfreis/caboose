package docker

import (
	"os"
	"testing"

	"github.com/bfreis/caboose/internal/nodocker"
)

func TestMain(m *testing.M) { os.Exit(nodocker.Run(m)) }
