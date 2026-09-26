// Command preset prints the Dockerfile caboose setup would write for the
// sections named on its command line, for tests/byo/run.sh to build: the
// suite has no terminal to answer setup's questions with. Test-only.
package main

import (
	"fmt"
	"os"

	"github.com/bfreis/caboose/internal/assets"
)

func main() {
	data, err := assets.Preset(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "preset:", err)
		os.Exit(1)
	}
	os.Stdout.Write(data)
}
