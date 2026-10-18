package box

import (
	"strings"

	"github.com/AndrewDryga/coop/internal/shadowpath"
)

const CoopIgnoreFile = shadowpath.CoopIgnoreFile

// IsCoopignoreName reports whether name, a path's last part as Git or a folder listing spells it,
// can be a folder's .coopignore: a case-insensitive Mac disk opens .coopignore as .COOPIGNORE, so a
// change to any case of the name can change what boxes are shown.
func IsCoopignoreName(name string) bool { return strings.EqualFold(name, CoopIgnoreFile) }

// UnreadableCoopignore is the refusal of a .coopignore coop cannot read, which the commands report
// by name.
type UnreadableCoopignore = shadowpath.UnreadableCoopignore
