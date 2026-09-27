package box

import "github.com/AndrewDryga/coop/internal/shadowpath"

// These aliases keep the box package's public contract while the lower-level shadowpath package
// gives preset loading and synthesized repository copies the exact same visibility decision.
var (
	SecretGlobs = shadowpath.SecretGlobs
	AllowGlobs  = shadowpath.AllowGlobs
)

const CoopIgnoreFile = shadowpath.CoopIgnoreFile
