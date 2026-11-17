package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/shadowpath"
	"golang.org/x/sys/unix"
)

// A launch hides files by their paths, and a box can move a hidden file, or the folder holding
// it, to a path no rule names: `mv config cfg` takes config/prod.yml out of a config/*.yml rule,
// and moving a folder out of a nested .coopignore's scope does the same. The next launch and
// Coop's host tools would then call it visible. The box cannot read a hidden file, so it can only
// move it, never copy it, and a move keeps the file's identity (device and inode). So each launch
// remembers the identity of what it hid, with the path the rules hid it at, and a later decision
// hides a remembered file or folder found at any other path. Back at that path, its rules decide
// it again, so taking a rule out of .coopignore still shows the file.
//
// Device and inode alone are not proof: Linux ext4 and XFS give a deleted file's inode number to
// the next new file, so after an atomic save replaces a hidden .env, an ordinary file written next
// could take its number and be hidden on every later launch. So each remembered entry also keeps a
// proof taken when it was hidden (hiddenFileProof), and a file found under its identity counts as
// that entry only when the proof still matches.

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
	// Proof is hiddenFileProof when it was hidden. Empty, identity alone decides.
	Proof string `json:"proof,omitempty"`
}

type hiddenFilesRecord struct {
	Version int `json:"version"`
	// RootDev is the repository root's device when the record was written. A reboot can renumber
	// it, so a load re-anchors the files recorded on it to the root's live device; Dev itself is
	// never trusted across a reboot. A file on another filesystem mounted inside keeps its own Dev.
	RootDev uint64       `json:"root_dev,omitempty"`
	Files   []hiddenFile `json:"files"`
}

// rootDevice is the live device of the repository root, or false when it cannot be read.
func rootDevice(repo string) (uint64, bool) {
	info, err := os.Stat(repo)
	if err != nil {
		return 0, false
	}
	id, ok := identityOf(info)
	return id.dev, ok
}

type fileIdentity struct{ dev, ino uint64 }

// hiddenOrigin is where a launch hid a file or folder, with the proof it took of it then.
type hiddenOrigin struct{ path, proof string }

// rememberedHidden maps the identity of a file or folder a launch hid to where it was hidden.
type rememberedHidden map[fileIdentity]hiddenOrigin

// originAt is the path a launch hid the object now at path (identity id) at, when it is that
// object: an entry with a proof matches only while the object there still gives the same one.
func (r rememberedHidden) originAt(id fileIdentity, path string) (string, bool) {
	origin, ok := r[id]
	if !ok {
		return "", false
	}
	if origin.proof != "" && proofAt(id, path) != origin.proof {
		return "", false
	}
	return origin.path, true
}

// hiddenFileProof is what, beside device and inode, shows a file found under a remembered identity
// is the one a launch hid: the inode generation where the filesystem keeps one (a reused inode gets
// a new one; a move or an edit in place keeps it), else its size and modification time, which an
// edit changes. A folder with no generation has only its identity: its size and time change with
// every entry written in it. Tests replace it, since real inode reuse cannot be forced.
var hiddenFileProof = func(path string, info fs.FileInfo) string {
	if gen, ok := inodeGenerationOf(path, info); ok {
		return "gen:" + strconv.FormatUint(uint64(gen), 10)
	}
	if info.IsDir() {
		return ""
	}
	return "stat:" + strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// inodeGenerationOf is inodeGeneration; a test replaces it to take the fallback on any filesystem.
var inodeGenerationOf = inodeGeneration

// proofAt is hiddenFileProof of the object at path, or "" when it is gone or no longer has
// identity id. An empty proof falls back to identity alone, the side that hides.
func proofAt(id fileIdentity, path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	if now, ok := identityOf(info); !ok || now != id {
		return ""
	}
	return hiddenFileProof(path, info)
}

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
	live, ok := rootDevice(repo)
	remembered := rememberedHidden{}
	for _, f := range record.Files {
		if ok && record.RootDev != 0 && f.Dev == record.RootDev {
			f.Dev = live
		}
		remembered[fileIdentity{dev: f.Dev, ino: f.Ino}] = hiddenOrigin{path: f.Path, proof: f.Proof}
	}
	return remembered
}

// movedHere reports whether the file or folder at relSlash is one a launch hid at another path.
func (r rememberedHidden) movedHere(repo, relSlash string) bool {
	path := filepath.Join(repo, filepath.FromSlash(relSlash))
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	id, ok := identityOf(info)
	if !ok {
		return false
	}
	at, remembered := r.originAt(id, path)
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
	return withHiddenFilesLock(context.Background(), repo, func() error {
		return rememberHiddenFiles(repo, workdir, mounts)
	})
}

// Concurrent launches must see each other's remembered identities before deciding which existing
// aliases hide. The kernel backend captures and persists under this lock; the mount backend decides
// its mounts first (ComputeMounts) and takes it only to persist them.
func withHiddenFilesLock(ctx context.Context, repo string, apply func() error) error {
	path, err := hiddenFilesPath(repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("hidden-file identity lock is not a regular file"), err)
	}
	if err := flockWait(ctx, file, unix.LOCK_EX); errors.Is(err, errAccountAuthorityBusy) {
		// flockWait's busy error speaks of credentials; this lock guards the checkout's hidden files.
		return fmt.Errorf("another coop launch of %s still holds its hidden-file record — retry", repo)
	} else if err != nil {
		return err
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return apply()
}

func rememberHiddenFiles(repo, workdir string, mounts []Mount) error {
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
		path := filepath.Join(repo, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		id, ok := identityOf(info)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		at := rel
		if earlier, ok := before.originAt(id, path); ok {
			at = earlier
		}
		record.Files = append(record.Files, hiddenFile{Dev: id.dev, Ino: id.ino, Path: at, Proof: hiddenFileProof(path, info)})
	}
	if len(record.Files) == 0 && len(before) == 0 {
		return nil
	}
	if err := preserveHiddenAliases(repo, before, seen, hiddenDirs(workdir, mounts), &record); err != nil {
		return err
	}
	return writeHiddenFiles(repo, record)
}

// The mount backend skips hidden descendants and .git. It must not discard an
// identity a complete filesystem admission recorded there, or a subsequent host
// atomic save can leave its old alias exposed. Ordinary unchanged records need
// no additional walk; deliberate declassification at the original path remains.
//
// A folder the mount plan hides that Coop cannot read is skipped (the box only sees its decoy), and
// an entry that vanishes mid-walk is no alias. Past the walk bound the identities still missing are
// kept, not forgotten. Any other walk error fails the launch, so the record is rewritten next time.
func preserveHiddenAliases(repo string, before rememberedHidden, seen map[fileIdentity]bool, hidden []string, record *hiddenFilesRecord) error {
	missing := rememberedHidden{}
	for id, origin := range before {
		if !seen[id] {
			missing[id] = origin
		}
	}
	if len(missing) == 0 {
		return nil
	}
	keepMissing := func() error {
		for id, origin := range missing {
			if len(record.Files) >= maxHiddenFiles {
				return errors.New("hidden-file alias record exceeds admission bounds")
			}
			record.Files = append(record.Files, hiddenFile{Dev: id.dev, Ino: id.ino, Path: origin.path, Proof: origin.proof})
		}
		return fs.SkipAll
	}
	visited := 0
	err := filepath.WalkDir(repo, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if rel, err := filepath.Rel(repo, name); err == nil && errors.Is(walkErr, fs.ErrPermission) && underHiddenDir(filepath.ToSlash(rel), hidden) {
				return nil
			}
			return walkErr
		}
		visited++
		if visited > 1_000_000 {
			return keepMissing()
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		id, ok := identityOf(info)
		origin, missingID := missing[id]
		if !ok || !missingID || origin.proof != "" && hiddenFileProof(name, info) != origin.proof {
			return nil // not a remembered object, or a new one that took its inode number
		}
		rel, err := filepath.Rel(repo, name)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == origin.path {
			return nil
		}
		if len(record.Files) >= maxHiddenFiles {
			return errors.New("hidden-file alias record exceeds admission bounds")
		}
		record.Files = append(record.Files, hiddenFile{Dev: id.dev, Ino: id.ino, Path: origin.path, Proof: origin.proof})
		delete(missing, id)
		if len(missing) == 0 {
			return fs.SkipAll
		}
		return nil
	})
	slices.SortFunc(record.Files, func(a, b hiddenFile) int { return strings.Compare(a.Path, b.Path) })
	return err
}

// hiddenDirs lists the repository-relative folders the mount plan covers with a decoy.
func hiddenDirs(workdir string, mounts []Mount) []string {
	var dirs []string
	for _, m := range mounts {
		if rel, ok := strings.CutPrefix(m.Target, workdir+"/"); ok && m.Kind == DirDecoy {
			dirs = append(dirs, rel)
		}
	}
	return dirs
}

func underHiddenDir(rel string, dirs []string) bool {
	for _, dir := range dirs {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

func writeHiddenFiles(repo string, record hiddenFilesRecord) error {
	path, err := hiddenFilesPath(repo)
	if err != nil {
		return err
	}
	record.RootDev, _ = rootDevice(repo)
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
