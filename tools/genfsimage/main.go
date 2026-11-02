// genfsimage packages trusted sources for the filesystem helper image.
package main

import (
	"fmt"
	"os"

	"github.com/AndrewDryga/coop/internal/fsimage"
)

func main() {
	data, err := fsimage.Pack(".")
	if err == nil {
		err = os.WriteFile("internal/fsimage/source.tar", data, 0644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("packaged trusted filesystem sources (%d bytes)\n", len(data))
}
