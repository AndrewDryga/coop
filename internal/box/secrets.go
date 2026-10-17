package box

import "github.com/AndrewDryga/coop/internal/shadowpath"

// These aliases keep the box package's public contract while the lower-level shadowpath package
// gives preset loading and synthesized repository copies the exact same visibility decision.
var (
	SecretGlobs = shadowpath.SecretGlobs
	AllowGlobs  = shadowpath.AllowGlobs
)

const CoopIgnoreFile = shadowpath.CoopIgnoreFile

// UnreadableCoopignore is the refusal of a .coopignore coop cannot read, which the commands report
// by name.
type UnreadableCoopignore = shadowpath.UnreadableCoopignore

// BuiltinSecret reports whether a path's file name, or a folder name above it, alone makes it a
// secret, before any .coopignore; a scan that cannot read a .coopignore still names such a file.
func BuiltinSecret(relSlash string) bool { return shadowpath.BuiltinSecret(relSlash) }
