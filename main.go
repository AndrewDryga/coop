// Command coop runs coding agents in a sandbox that holds only your project, with the repo's
// secrets hidden from them. See `coop help`.
package main

import (
	"os"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/cli"
)

func main() {
	box.EnableLifeline() // here, not in cli.Main: a test binary must never start itself as one
	os.Exit(cli.Main(os.Args[1:]))
}
