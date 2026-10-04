// Command caboose-agent is the sandbox's end of the link to the host, and a
// vm guest's init. It is built for linux, embedded in the launcher, and
// baked into the image's layer; see internal/agent.
package main

import (
	"os"

	"github.com/bfreis/caboose/internal/agent"
)

func main() {
	// A vm guest's kernel runs it as /init, with no arguments.
	if os.Getpid() == 1 {
		os.Exit(agent.Init())
	}
	os.Exit(agent.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
