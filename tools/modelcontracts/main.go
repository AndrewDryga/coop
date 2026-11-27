// modelcontracts exports provider-owned native proxy contracts for controllers.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/AndrewDryga/coop/internal/agent"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: modelcontracts <provider> <selection>")
		os.Exit(2)
	}
	a, ok := agent.Get(os.Args[1])
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown provider")
		os.Exit(2)
	}
	native := a.NativeCredentials()
	if native.Broker.Routes == nil {
		fmt.Fprintln(os.Stderr, "provider has no native proxy contract")
		os.Exit(1)
	}
	routes, err := native.Broker.Routes(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(routes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
