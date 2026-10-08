// Command seed prints the Dockerfile caboose seeds a dockerfile image
// profile's dir with (assets.Seed), for the sections named on its command
// line, for tests/byo/run.sh to build: the suite has no terminal to answer
// setup's questions with. Test-only.
package main

import (
	"fmt"
	"os"

	"github.com/bfreis/caboose/internal/assets"
)

func main() {
	data, err := assets.Seed(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	os.Stdout.Write(data)
}
