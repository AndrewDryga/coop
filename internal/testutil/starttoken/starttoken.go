// Package starttoken gives tests process start tokens of a known kind, so a record can name a dead
// process, or another operating system's, without one existing.
package starttoken

import "github.com/AndrewDryga/coop/internal/processidentity"

// No process here has either token: no Linux boot is named "0", and nothing on macOS started at
// the epoch.
const (
	linux  = "linux-proc-v1:0:1"
	darwin = "darwin-kinfo-v1:0:1"
)

// Stale is a token of this platform's shape that no process has. Beside a pid that is gone it reads
// as gone, and beside a live pid as a reused one.
func Stale() string {
	if processidentity.Native(linux) {
		return linux
	}
	return darwin
}

// Foreign is a token of the other platform's shape, as coop on the other operating system writes
// it. No pid here can confirm or refute it.
func Foreign() string {
	if processidentity.Native(linux) {
		return darwin
	}
	return linux
}
