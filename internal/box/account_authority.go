package box

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/randid"
	"golang.org/x/sys/unix"
)

// errAccountAuthorityBusy is another process holding the account's lock for longer than this one waits.
var errAccountAuthorityBusy = errors.New("canonical credential transaction is busy — retry")

type accountAuthority struct {
	Version   int               `json:"version"`
	Provider  string            `json:"provider"`
	Account   string            `json:"account"`
	Epoch     uint64            `json:"epoch"`
	Revision  uint64            `json:"revision"`
	Revoked   bool              `json:"revoked"`
	Renewal   string            `json:"renewal,omitempty"`
	Migration string            `json:"migration,omitempty"`
	Selection string            `json:"selection,omitempty"`
	Principal string            `json:"principal,omitempty"`
	Artifacts map[string][]byte `json:"artifacts,omitempty"`
}

// Adapters provide the complete credential-only inventory and identity validation, and Complete
// finishes an interrupted renewal from the provider's kept answer (settleRenewal).
// An access-only projector is deliberately not an importer of canonical authority.
type accountAuthoritySpec struct {
	Provider string
	Files    map[string]int64
	Inspect  func(map[string][]byte) (selection, principal string, err error)
	Complete func(files map[string][]byte, received []byte, at time.Time) (map[string][]byte, error)
}

// accountAuthorityRoot is one account's custody folder under ConfigDir: its credential envelope,
// renewal journal and lock, or a native home's owner record beside the home it publishes. No box
// mounts the folder (a native home's box mounts only its home child) and only coop writes it, so
// its records are read plainly and its lock is a plain flock. The one thing that deletes custody
// folders, `coop credentials rm`, does so only under lockAccountHomes, when no launch is inside
// one. The descriptor serves the links and renames that move files between it and a native home.
type accountAuthorityRoot struct {
	path string
	fd   *os.File
}

func accountNameValid(name string) bool {
	return name != "" && name != "." && name != ".." && name[0] != '-' &&
		filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00\r\n") && !filepath.IsAbs(name)
}

func openAccountAuthorityRoot(cfg *config.Config, spec accountAuthoritySpec, account string) (*accountAuthorityRoot, error) {
	if cfg == nil || !filepath.IsAbs(cfg.ConfigDir) || !accountNameValid(spec.Provider) || !accountNameValid(account) ||
		len(spec.Files) == 0 || len(spec.Files) > 8 || spec.Inspect == nil {
		return nil, errors.New("invalid canonical credential selection")
	}
	for name, limit := range spec.Files {
		if !accountNameValid(name) || limit <= 0 || limit > 1<<20 {
			return nil, errors.New("invalid canonical credential declaration")
		}
	}
	return openPrivateAccountTree(cfg.ConfigDir, spec.Provider, "credentials", account)
}

// openPrivateAccountTree opens path/names... as custody, creating each folder owner-only. The
// names come from provider and account settings, so each must be one plain folder name. Each is
// also refused as a link: ConfigDir/<provider> was a box mount before 2026-07, so a box of that
// time could have left a link in a name custody uses now.
func openPrivateAccountTree(path string, names ...string) (*accountAuthorityRoot, error) {
	if !filepath.IsAbs(path) || len(names) == 0 {
		return nil, errors.New("invalid private account directory")
	}
	for _, name := range names {
		if !accountNameValid(name) {
			return nil, errors.New("invalid private account directory component")
		}
	}
	if err := config.EnsurePrivateDir(path); err != nil {
		return nil, err
	}
	dir := path
	for _, name := range names {
		dir = filepath.Join(dir, name)
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create canonical credential directory: %w", err)
		}
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(fmt.Errorf("canonical credential path %s must be a real directory", dir), err)
		}
	}
	fd, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open canonical credential directory: %w", err)
	}
	return &accountAuthorityRoot{path: dir, fd: fd}, nil
}

// privateAccountDirectory refuses a folder others can read. It guards what a box can reach: a
// native home's box can chmod the home it mounts, and a legacy profile folder was a box mount.
func privateAccountDirectory(dir *os.File) error {
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("canonical credential directory must be owner-private")
	}
	return nil
}

// privateAccountFile is privateAccountDirectory's check for a legacy writer lock, which lives in a
// folder a box could reach.
func privateAccountFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return errors.New("canonical credential file must be owner-private with one link")
	}
	return nil
}

func (r *accountAuthorityRoot) close() { _ = r.fd.Close() }

func (r *accountAuthorityRoot) dir() *os.File { return r.fd }

func sameAccountInode(a, b *os.File) bool {
	x, err := a.Stat()
	if err != nil {
		return false
	}
	y, err := b.Stat()
	return err == nil && os.SameFile(x, y)
}

// lock takes the custody's lock, waiting up to 30 seconds (or ctx's deadline) for another process
// to finish its transaction.
func (r *accountAuthorityRoot) lock(ctx context.Context) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(r.path, ".authority.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open canonical lock: %w", err)
	}
	if err := flockWait(ctx, lock, unix.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

// flockWait takes how on file, waiting up to 30 seconds (or ctx's deadline) for another process
// to let go.
func flockWait(ctx context.Context, file *os.File, how int) error {
	until := time.Now().Add(30 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(until) {
		until = deadline
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(until) {
			return errAccountAuthorityBusy
		}
		err := unix.Flock(int(file.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(min(20*time.Millisecond, time.Until(until)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// lockAccountHomes guards an account's native-homes folder against `coop credentials rm`:
// launches hold it shared while they work in one of its homes, and removal holds it exclusive while
// it deletes the folder, custody lock files and all. The lock sits beside the folder, where removal
// does not reach, so both sides contend for one inode.
func lockAccountHomes(ctx context.Context, folder string, exclusive bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(folder), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(folder), "."+filepath.Base(folder)+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open native homes lock: %w", err)
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if err := flockWait(ctx, lock, how); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

// LockAccountHomesForRemoval holds every native home of account so `coop credentials rm` can delete
// them: it waits for launches working in one to finish and keeps new ones out until release.
func LockAccountHomesForRemoval(ctx context.Context, cfg *config.Config, provider, account string) (func(), error) {
	return lockAccountHomes(ctx, HistoryAccountRoot(cfg, provider, account), true)
}

func (r *accountAuthorityRoot) read(spec accountAuthoritySpec, account string) (*accountAuthority, error) {
	var record accountAuthority
	ok, err := config.ReadRecord(filepath.Join(r.path, "authority.json"), &record)
	if err != nil || !ok {
		return nil, err
	}
	if err := validateAccountAuthority(spec, account, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func validateAccountAuthority(spec accountAuthoritySpec, account string, record *accountAuthority) error {
	if record.Version != 1 || record.Provider != spec.Provider || record.Account != account || record.Epoch == 0 || record.Revision == 0 {
		return errors.New("canonical credential selection/version mismatch")
	}
	for _, receipt := range []string{record.Renewal, record.Migration} {
		if receipt == "" {
			continue
		}
		decoded, err := hex.DecodeString(receipt)
		if err != nil || len(decoded) != 16 {
			return errors.New("invalid canonical renewal receipt")
		}
	}
	if record.Revoked {
		if len(record.Artifacts) != 0 || record.Selection != "" || record.Principal != "" {
			return errors.New("revoked credential retains serving authority")
		}
		return nil
	}
	if len(record.Artifacts) == 0 || len(record.Artifacts) > len(spec.Files) {
		return errors.New("invalid canonical credential inventory")
	}
	for name, data := range record.Artifacts {
		limit, ok := spec.Files[name]
		if !ok || len(data) == 0 || int64(len(data)) > limit {
			return errors.New("invalid canonical credential artifact")
		}
	}
	selection, principal, err := spec.Inspect(record.Artifacts)
	if err != nil {
		return err
	}
	if selection == "" || principal == "" || record.Selection != selection || record.Principal != principal {
		return errors.New("canonical credential principal/selection mismatch")
	}
	return nil
}

func lockAccountAuthority(ctx context.Context, cfg *config.Config, spec accountAuthoritySpec, account string) (*accountAuthorityRoot, *os.File, *accountAuthority, error) {
	root, lock, current, err := lockAccountAuthorityState(ctx, cfg, spec, account)
	if err != nil {
		return nil, nil, nil, err
	}
	current, err = root.settleRenewal(spec, account, current)
	if err != nil {
		_ = lock.Close()
		root.close()
		return nil, nil, nil, err
	}
	return root, lock, current, nil
}

// Explicit sign-in/removal may supersede an uncertain rotation. Ordinary readers and
// renewers must use lockAccountAuthority instead, which settles an earlier renewal first.
func lockAccountAuthorityState(ctx context.Context, cfg *config.Config, spec accountAuthoritySpec, account string) (*accountAuthorityRoot, *os.File, *accountAuthority, error) {
	root, err := openAccountAuthorityRoot(cfg, spec, account)
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := root.lock(ctx)
	if err != nil {
		root.close()
		return nil, nil, nil, err
	}
	current, err := root.read(spec, account)
	if err != nil {
		_ = lock.Close()
		root.close()
		return nil, nil, nil, err
	}
	return root, lock, current, nil
}

func cloneAccountAuthority(current *accountAuthority) *accountAuthority {
	// A refused callback cannot mutate the reported original snapshot through its maps.
	if current == nil {
		return nil
	}
	copy := *current
	copy.Artifacts = make(map[string][]byte, len(current.Artifacts))
	for name, data := range current.Artifacts {
		copy.Artifacts[name] = bytes.Clone(data)
	}
	return &copy
}

// publish replaces the envelope with next if its generation follows current coherently. Callers
// hold the custody lock, so current is what is on disk.
func (root *accountAuthorityRoot) publish(spec accountAuthoritySpec, account string,
	current, next *accountAuthority, readinessErr error) (*accountAuthority, bool, error) {
	if next == nil {
		return current, false, readinessErr
	}
	var epoch, revision uint64
	if current != nil {
		epoch, revision = current.Epoch, current.Revision
	}
	if revision == math.MaxUint64 || next.Epoch == 0 || next.Epoch < epoch || next.Epoch-epoch > 1 {
		return current, false, errors.New("canonical credential generation did not advance coherently")
	}
	if current != nil && next.Epoch == epoch &&
		(current.Revoked || next.Revoked || current.Principal != next.Principal || current.Selection != next.Selection) {
		return current, false, errors.New("canonical credential replacement/removal needs a fresh epoch")
	}
	next.Version, next.Provider, next.Account, next.Revision = 1, spec.Provider, account, revision+1
	if err := validateAccountAuthority(spec, account, next); err != nil {
		return current, false, err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return current, false, errors.New("canonical credential serialization failed")
	}
	if err := writeAccountPrivate(root, "authority.json", append(data, '\n')); err != nil {
		return current, false, err
	}
	return next, true, readinessErr
}

// writeAccountPrivate publishes one custody record whole: a temp file, fsynced, renamed over name.
// Callers hold the custody lock.
func writeAccountPrivate(root *accountAuthorityRoot, name string, data []byte) error {
	if !accountNameValid(name) {
		return errors.New("invalid private account publication")
	}
	temporary := filepath.Join(root.path, ".private-"+randid.Hex(16))
	f, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(root.path, name))
}
