// gennetimage packages only trusted gateway source for release-time embedding.
// It is a developer command, never invoked by coop build or a user repository.
// gatewayimage.Pack decides what goes in; this command writes it.
package main

import (
	"fmt"
	"os"

	"github.com/AndrewDryga/coop/internal/gatewayimage"
)

func main() {
	data, err := gatewayimage.Pack(".")
	if err == nil {
		err = os.WriteFile("internal/gatewayimage/source.tar", data, 0644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("packaged trusted gateway sources (%d bytes)\n", len(data))
}
