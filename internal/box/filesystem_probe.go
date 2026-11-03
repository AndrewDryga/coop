package box

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

// Coop versions before 2026-11-03 checked each launch's share with a probe folder at every
// project root. Their custody records still list those folders, so retiring such a record
// removes one that a crash left behind; nothing writes new probes.
const (
	filesystemProbePrefix  = ".coop-fs-probe-"
	filesystemProbeContent = "coop filesystem admission fixture\n"
)

type filesystemProbeRecord struct {
	Root, Leaf string
	Dev, Ino   uint64
}

func (p filesystemProbeRecord) valid() bool {
	return filepath.IsAbs(p.Root) && filepath.Base(p.Leaf) == p.Leaf && strings.HasPrefix(p.Leaf, filesystemProbePrefix) && p.Dev != 0 && p.Ino != 0
}

func (p filesystemProbeRecord) close() error {
	if !p.valid() {
		return errors.New("invalid filesystem probe identity")
	}
	parent, err := safefile.OpenRoot(p.Root)
	if os.IsNotExist(err) {
		return nil // the project folder is gone, and the probe folder inside it with it
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	dir, err := safefile.OpenDir(parent, p.Leaf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	id, ok := identityOf(info)
	if !ok || !info.IsDir() || id.ino != p.Ino {
		return errors.New("filesystem probe directory was replaced; retained for review")
	}
	// Device numbers can change across a host reboot; neither one nor an inode
	// alone authorizes recursive deletion. Retire only our bounded inert fixture
	// through the pinned directory, preserving unexpected content for review.
	allowed := []string{".env", "Cafe\u0301", "public", "object-alias", "warm-alias", "replacement"}
	names, err := dir.Readdirnames(len(allowed) + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) > len(allowed) {
		return errors.New("filesystem probe has unexpected entries; retained for review")
	}
	for _, name := range names {
		if !slices.Contains(allowed, fsfilter.NormalizeName(name, fsfilter.Profile{Normalize: true})) {
			return errors.New("filesystem probe has unexpected entries; retained for review")
		}
		data, err := safefile.ReadRegular(dir, name, int64(len(filesystemProbeContent)))
		if err != nil || len(data) != 0 && string(data) != filesystemProbeContent {
			return errors.Join(errors.New("filesystem probe contents changed; retained for review"), err)
		}
	}
	for _, name := range names {
		if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	current, err := safefile.OpenDir(parent, p.Leaf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer current.Close()
	now, err := current.Stat()
	if err != nil || !os.SameFile(info, now) {
		return errors.Join(errors.New("filesystem probe directory changed during cleanup"), err)
	}
	return unix.Unlinkat(int(parent.Fd()), p.Leaf, unix.AT_REMOVEDIR)
}

// filesystemVolumeProfile reports which other spellings of a name open the same file on root's
// volume: another letter case (Fold) or another Unicode form (Normalize). Both belong to the whole
// volume, so the test file goes in Coop's private folder when it shares root's device and nothing
// is written in the project. A root on another device gets both: a policy may then deny spellings
// the volume would not resolve, but never misses one it would.
func filesystemVolumeProfile(root, private string) (fsfilter.Profile, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return fsfilter.Profile{}, err
	}
	privateInfo, err := os.Stat(private)
	if err != nil {
		return fsfilter.Profile{}, err
	}
	rootID, rootOK := identityOf(rootInfo)
	privateID, privateOK := identityOf(privateInfo)
	if !rootOK || !privateOK {
		return fsfilter.Profile{}, errors.New("filesystem identity unavailable")
	}
	if rootID.dev != privateID.dev {
		return fsfilter.Profile{Fold: true, Normalize: true}, nil
	}
	name := filepath.Join(private, "Caf\u00e9")
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fsfilter.Profile{}, err
	}
	defer os.Remove(name)
	base, err := file.Stat()
	if err := errors.Join(err, file.Close()); err != nil {
		return fsfilter.Profile{}, err
	}
	// Folding is tried on the ASCII letters alone and on the accented one, so a volume that folds
	// only one kind still counts as folding.
	var profile fsfilter.Profile
	for _, alias := range []struct {
		name       string
		equivalent *bool
	}{{"CAF\u00c9", &profile.Fold}, {"cAF\u00e9", &profile.Fold}, {"Cafe\u0301", &profile.Normalize}} {
		info, err := os.Stat(filepath.Join(private, alias.name))
		if err == nil {
			*alias.equivalent = *alias.equivalent || os.SameFile(base, info)
		} else if !os.IsNotExist(err) {
			return fsfilter.Profile{}, err
		}
	}
	return profile, nil
}

func nonoverlappingFilesystemRoots(roots []string) error {
	for i, root := range roots {
		for _, other := range roots[:i] {
			for _, pair := range [][2]string{{root, other}, {other, root}} {
				rel, err := filepath.Rel(pair[0], pair[1])
				if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return errors.New("overlapping filesystem roots need one inherited policy")
				}
			}
		}
	}
	return nil
}
