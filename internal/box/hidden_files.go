package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/shadowpath"
)

// A launch hides files by their paths, and a box can move a hidden file, or the folder holding
// it, to a path no rule names: `mv config cfg` takes config/prod.yml out of a config/*.yml rule,
// and moving a folder out of a nested .coopignore's scope does the same. The next launch and
// Coop's host tools would then call it visible. The box cannot read a hidden file, so it can only
// move it, never copy it, and a move keeps the file's identity (device and inode). So each launch
// remembers the identity of what it hid, with the path the rules hid it at, and a later decision
// hides a remembered file or folder found at any other path. Back at that path, its rules decide
// it again, so taking a rule out of .coopignore still shows the file.

const (
	hiddenFilesVersion = 1
	// maxHiddenFiles bounds the memory well past any real project's hidden files; past it a launch
	// remembers nothing new rather than grow the record without bound.
	maxHiddenFiles = 100000
)

type hiddenFile struct {
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
	Path string `json:"path"` // repository-relative, slash-separated: where the rules hid it
}

type hiddenFilesRecord struct {
	Version int          `json:"version"`
	Files   []hiddenFile `json:"files"`
}

type fileIdentity struct{ dev, ino uint64 }

// rememberedHidden maps the identity of a file or folder a launch hid to the path it was hidden at.
type rememberedHidden map[fileIdentity]string

func identityOf(info fs.FileInfo) (fileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{dev: uint64(stat.Dev), ino: stat.Ino}, true
}

// hiddenFilesPath is the host-private record of what launches hid in repo, keyed by its real path.
func hiddenFilesPath(repo string) (string, error) {
	root, err := serviceStateRoot("hidden-files")
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		if canonical, err = filepath.Abs(repo); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(root, hex.EncodeToString(sum[:])+".json"), nil
}

// loadHiddenFiles reads what earlier launches hid in repo. The record is host-private, out of every
// box's reach; one that is missing or unreadable remembers nothing, which leaves the rules alone
// to decide, as before any launch remembered.
func loadHiddenFiles(repo string) rememberedHidden {
	path, err := hiddenFilesPath(repo)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var record hiddenFilesRecord
	if json.Unmarshal(data, &record) != nil || record.Version != hiddenFilesVersion {
		return nil
	}
	remembered := rememberedHidden{}
	for _, f := range record.Files {
		remembered[fileIdentity{dev: f.Dev, ino: f.Ino}] = f.Path
	}
	return remembered
}

// movedHere reports whether the file or folder at relSlash is one a launch hid at another path.
func (r rememberedHidden) movedHere(repo, relSlash string) bool {
	info, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(relSlash)))
	if err != nil {
		return false
	}
	id, ok := identityOf(info)
	if !ok {
		return false
	}
	at, remembered := r[id]
	return remembered && at != relSlash
}

// NewShadowDecider returns a predicate reporting whether a repo-relative slash path is
// shadowed from the box: its basename matches SecretGlobs (and AllowGlobs doesn't whitelist
// it), or a .coopignore in the root, an ancestor directory or a folder above the repo short of
// the home folder matches it (AllowGlobs does NOT override an explicit .coopignore — it's the
// user's final say), or it, or a folder above it, is a file a launch hid that a box has since
// moved (see the top of this file). Each directory's .coopignore is loaded once into the
// closure's cache. ComputeMounts and check-secrets share this visibility decision; the scanner
// still checks hidden commit candidates because hiding a file from the box does not prevent
// committing it. A hidden directory hides every descendant, including names an allow rule would
// otherwise rescue. A path under a .coopignore coop cannot read comes back hidden with an
// UnreadableCoopignore, which every caller that can refuse returns, unless a readable rule or a
// secret name, its own or a folder's above it, already hides it.
func NewShadowDecider(repo string) func(relSlash string) (bool, error) {
	decide := shadowpath.NewDecider(repo)
	remembered := loadHiddenFiles(repo)
	if len(remembered) == 0 {
		return decide
	}
	dirs := map[string]bool{"": false} // the repository root is never hidden
	var dirMoved func(string) bool
	dirMoved = func(dirRel string) bool {
		moved, ok := dirs[dirRel]
		if !ok {
			parent := ""
			if i := strings.LastIndexByte(dirRel, '/'); i >= 0 {
				parent = dirRel[:i]
			}
			moved = dirMoved(parent) || remembered.movedHere(repo, dirRel)
			dirs[dirRel] = moved
		}
		return moved
	}
	return func(relSlash string) (bool, error) {
		hidden, err := decide(relSlash)
		if hidden || err != nil {
			return hidden, err
		}
		relSlash = filepath.ToSlash(filepath.Clean(filepath.FromSlash(relSlash)))
		parent := ""
		if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
			parent = relSlash[:i]
		}
		return dirMoved(parent) || remembered.movedHere(repo, relSlash), nil
	}
}

// RememberHiddenFiles records what a launch's mounts hid in repo, so a box that moves one cannot
// show it to the next launch or to Coop's host tools. A file hidden because it was remembered keeps
// the path it was first hidden at; one no longer hidden, back at its path under rules that no
// longer name it, or gone, is forgotten.
func RememberHiddenFiles(repo, workdir string, mounts []Mount) error {
	path, err := hiddenFilesPath(repo)
	if err != nil {
		return err
	}
	before := loadHiddenFiles(repo)
	record := hiddenFilesRecord{Version: hiddenFilesVersion}
	seen := map[fileIdentity]bool{}
	for _, m := range mounts {
		if m.Kind != Decoy && m.Kind != DirDecoy {
			continue
		}
		rel, ok := strings.CutPrefix(m.Target, workdir+"/")
		if !ok || len(record.Files) >= maxHiddenFiles {
			continue
		}
		info, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		id, ok := identityOf(info)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		at := rel
		if earlier, ok := before[id]; ok {
			at = earlier
		}
		record.Files = append(record.Files, hiddenFile{Dev: id.dev, Ino: id.ino, Path: at})
	}
	if len(record.Files) == 0 && len(before) == 0 {
		return nil
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := config.WriteFileAtomicMode(path, data, 0o600); err != nil {
		return errors.Join(errors.New("remember the files this launch hid"), err)
	}
	return nil
}
