// Command caboose-agent is the sandbox's end of the link to the host. It is
// built for linux, embedded in the launcher, and baked into the image's
// layer; see internal/agent.
package main

import (
	"os"

	"github.com/bfreis/caboose/internal/agent"
)

func main() {
	os.Exit(agent.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
