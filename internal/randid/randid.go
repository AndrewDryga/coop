// Package randid makes random identifiers. crypto/rand.Read never fails on the systems coop builds
// for: since Go 1.24 it stops the program instead of returning an error, so nothing here returns
// one, and no caller needs an error branch that cannot run.
package randid

import (
	"crypto/rand"
	"encoding/hex"
)

// Hex is n random bytes as lowercase hex, 2n characters.
func Hex(n int) string {
	return hex.EncodeToString(Bytes(n))
}

// Bytes is n random bytes.
func Bytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
