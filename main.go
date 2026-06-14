// Command agent runs a coding agent in a sandbox it can't escape, with the
// repo's secrets shadowed out of reach. See `coop help`.
package main

import (
	"os"

	"github.com/AndrewDryga/coop/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
