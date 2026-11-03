package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

// filesystemProbePrefix starts the name of every probe directory an admission makes at a root.
const filesystemProbePrefix = ".coop-fs-probe-"

type filesystemProbe struct {
	root, relative string
	profile        fsfilter.Profile
	record         filesystemProbeRecord
}

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
		data, err := safefile.ReadRegular(dir, name, int64(len(fsfilter.ProbeContent)))
		if err != nil || len(data) != 0 && string(data) != fsfilter.ProbeContent {
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

func newFilesystemProbe(root string, record func(filesystemProbeRecord) error) (*filesystemProbe, error) {
	dir, err := os.MkdirTemp(root, filesystemProbePrefix)
	if err != nil {
		return nil, err
	}
	p := &filesystemProbe{root: root, relative: filepath.Base(dir)}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	id, ok := identityOf(info)
	if !ok {
		return nil, errors.New("filesystem probe identity unavailable")
	}
	p.record = filesystemProbeRecord{Root: root, Leaf: p.relative, Dev: id.dev, Ino: id.ino}
	fail := func(err error) (*filesystemProbe, error) { return nil, errors.Join(err, p.close()) }
	if err := record(p.record); err != nil {
		return fail(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return fail(err)
	}
	for _, name := range []string{".env", "Café", "public", "warm-alias"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fsfilter.ProbeContent), 0644); err != nil {
			return fail(err)
		}
		if err := os.Chmod(filepath.Join(dir, name), 0644); err != nil {
			return fail(err)
		}
	}
	base, err := os.Stat(filepath.Join(dir, "Café"))
	if err != nil {
		return fail(err)
	}
	for name, destination := range map[string]*bool{"CAFÉ": &p.profile.Fold, "Cafe\u0301": &p.profile.Normalize} {
		alias, err := os.Stat(filepath.Join(dir, name))
		if err == nil {
			*destination = os.SameFile(base, alias)
		} else if !os.IsNotExist(err) {
			return fail(err)
		}
	}
	return p, nil
}

func (p *filesystemProbe) add(policy *fsfilter.Policy) {
	for _, name := range []string{".env", "Café"} {
		policy.Entries = append(policy.Entries, fsfilter.Entry{Path: p.relative + "/" + name, Flags: fsfilter.Secret})
	}
}

// After the loader retains the original inode, add an unlisted hardlink and
// replace the protected name. The probe now proves both retained-object denial
// across independent binds and path denial for a newly created inode.
func (p *filesystemProbe) replace(index int, probe *fsfilter.ShareProbe) error {
	dir := filepath.Join(p.root, p.relative)
	if err := os.Link(filepath.Join(dir, ".env"), filepath.Join(dir, "object-alias")); err != nil {
		return err
	}
	// Replace a pathname already opened in the guest, not just a fresh alias.
	if err := os.Link(filepath.Join(dir, ".env"), filepath.Join(dir, "replacement")); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "warm-alias")); err != nil {
		return err
	}
	temporary, err := os.OpenFile(filepath.Join(dir, "replacement"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.WriteString(fsfilter.ProbeContent)
	// Ordinary permissions must allow the probe's read, so only enforcement
	// can explain its denial after the protected pathname is replaced.
	if err := errors.Join(writeErr, temporary.Chmod(0644), temporary.Close()); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), filepath.Join(dir, ".env")); err != nil {
		return err
	}
	prefix := fmt.Sprintf("/sources/%d/%s/", index+1, p.relative)
	// Check the warmed pathname before another alias can refresh guest metadata.
	probe.Denied = append(probe.Denied, prefix+"warm-alias", prefix+".env", prefix+"object-alias", prefix+"Café")
	if p.profile.Fold {
		probe.Denied = append(probe.Denied, prefix+".ENV", prefix+"CAFÉ")
	}
	if p.profile.Normalize {
		probe.Denied = append(probe.Denied, prefix+"Cafe\u0301")
	}
	if p.profile.Fold && p.profile.Normalize {
		probe.Denied = append(probe.Denied, prefix+"CAFE\u0301")
	}
	probe.Readable = append(probe.Readable, prefix+"public")
	return nil
}

func (p *filesystemProbe) close() error { return p.record.close() }

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
