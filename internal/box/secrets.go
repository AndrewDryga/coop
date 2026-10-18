package box

import "github.com/AndrewDryga/coop/internal/shadowpath"

const CoopIgnoreFile = shadowpath.CoopIgnoreFile

// UnreadableCoopignore is the refusal of a .coopignore coop cannot read, which the commands report
// by name.
type UnreadableCoopignore = shadowpath.UnreadableCoopignore
