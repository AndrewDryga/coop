package box

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

const accountAuthorityLimit = 12 << 20

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

// Adapters provide the complete credential-only inventory and identity validation.
// An access-only projector is deliberately not an importer of canonical authority.
type accountAuthoritySpec struct {
	Provider string
	Files    map[string]int64
	Inspect  func(map[string][]byte) (selection, principal string, err error)
}

type accountDirectoryPin struct {
	parent *os.File
	name   string
	dir    *os.File
}

type accountAuthorityRoot struct {
	path string
	base *os.File
	pins []accountDirectoryPin
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

// The credential envelope and native-home owner record use the same host-only custody:
// descriptors pin each component, and no guest ever mounts their parent directory.
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
	base, err := safefile.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	root := &accountAuthorityRoot{path: path, base: base}
	if err := privateAccountDirectory(base); err != nil {
		root.close()
		return nil, err
	}
	parent := base
	for _, name := range names {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			root.close()
			return nil, fmt.Errorf("create canonical credential directory: %w", err)
		}
		dir, err := safefile.OpenDir(parent, name)
		if err != nil {
			root.close()
			return nil, fmt.Errorf("open canonical credential directory: %w", err)
		}
		root.pins = append(root.pins, accountDirectoryPin{parent: parent, name: name, dir: dir})
		if err := privateAccountDirectory(dir); err != nil {
			root.close()
			return nil, err
		}
		parent = dir
	}
	return root, nil
}

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

func (r *accountAuthorityRoot) close() {
	for i := len(r.pins) - 1; i >= 0; i-- {
		_ = r.pins[i].dir.Close()
	}
	_ = r.base.Close()
}

func (r *accountAuthorityRoot) dir() *os.File { return r.pins[len(r.pins)-1].dir }

func sameAccountInode(a, b *os.File) bool {
	x, err := a.Stat()
	if err != nil {
		return false
	}
	y, err := b.Stat()
	return err == nil && os.SameFile(x, y)
}

// Directory descriptors confine writes; checking every named pin also prevents reporting
// a successful publication into a detached directory after its name was replaced.
func (r *accountAuthorityRoot) checkNamed() error {
	base, err := safefile.OpenRoot(r.path)
	if err != nil {
		return err
	}
	valid := sameAccountInode(base, r.base)
	_ = base.Close()
	if !valid {
		return errors.New("canonical credential root changed")
	}
	for _, pin := range r.pins {
		current, err := safefile.OpenDir(pin.parent, pin.name)
		if err != nil {
			return fmt.Errorf("reopen private directory %s: %w", pin.name, err)
		}
		valid := sameAccountInode(current, pin.dir)
		_ = current.Close()
		if !valid {
			return errors.New("canonical credential directory changed")
		}
	}
	return nil
}

func (r *accountAuthorityRoot) checkLock(lock *os.File) error {
	if err := r.checkNamed(); err != nil {
		return err
	}
	current, err := safefile.OpenRegular(r.dir(), ".authority.lock")
	if err != nil {
		return fmt.Errorf("reopen canonical lock: %w", err)
	}
	defer current.Close()
	if err := privateAccountFile(current); err != nil {
		return err
	}
	if !sameAccountInode(lock, current) {
		return errors.New("canonical credential lock changed")
	}
	return nil
}

func (r *accountAuthorityRoot) lock(ctx context.Context) (*os.File, error) {
	// Elect one creator, then open the permanent inode without another create attempt.
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(int(r.dir().Fd()), ".authority.lock", flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(int(r.dir().Fd()), ".authority.lock", flags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open canonical lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), ".authority.lock")
	if err := privateAccountFile(lock); err != nil {
		_ = lock.Close()
		return nil, err
	}
	until := time.Now().Add(30 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(until) {
		until = deadline
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = lock.Close()
			return nil, err
		}
		if !time.Now().Before(until) {
			_ = lock.Close()
			return nil, errors.New("canonical credential transaction is busy — retry")
		}
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if err := r.checkLock(lock); err != nil {
				_ = lock.Close()
				return nil, err
			}
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = lock.Close()
			return nil, err
		}
		timer := time.NewTimer(min(20*time.Millisecond, time.Until(until)))
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *accountAuthorityRoot) read(spec accountAuthoritySpec, account string) (*accountAuthority, error) {
	file, err := safefile.OpenRegular(r.dir(), "authority.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := privateAccountFile(file); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > accountAuthorityLimit {
		return nil, errors.New("canonical credential exceeds its bound")
	}
	data, err := io.ReadAll(io.LimitReader(file, accountAuthorityLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > accountAuthorityLimit {
		return nil, errors.New("canonical credential exceeds its bound")
	}
	var record accountAuthority
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, errors.New("invalid canonical credential document")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing canonical credential data")
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
	if err := root.recoverRenewal(lock, current); err != nil {
		_ = lock.Close()
		root.close()
		return nil, nil, nil, err
	}
	return root, lock, current, nil
}

// Explicit sign-in/removal may supersede an uncertain rotation. Ordinary readers and
// renewers must use lockAccountAuthority instead, which refuses pending uncertainty.
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

func (root *accountAuthorityRoot) publish(lock *os.File, spec accountAuthoritySpec, account string,
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
	if err != nil || len(data) >= accountAuthorityLimit {
		return current, false, errors.New("canonical credential serialization failed")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return current, false, err
	}
	name := ".authority-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(root.dir().Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return current, false, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer unix.Unlinkat(int(root.dir().Fd()), name, 0)
	_, writeErr := file.Write(append(data, '\n'))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return current, false, errors.Join(writeErr, closeErr)
	}
	if err := root.checkLock(lock); err != nil {
		return current, false, err
	}
	latest, err := root.read(spec, account)
	if err != nil {
		return current, false, err
	}
	originalData, err := json.Marshal(current)
	if err != nil {
		return current, false, err
	}
	latestData, err := json.Marshal(latest)
	if err != nil {
		return current, false, err
	}
	if !bytes.Equal(originalData, latestData) {
		return current, false, errors.New("canonical credential changed during transaction — preserve and recover")
	}
	if err := unix.Renameat(int(root.dir().Fd()), name, int(root.dir().Fd()), "authority.json"); err != nil {
		return current, false, err
	}
	// This error is post-publication: return the new authority and effect truth even when
	// durability or the named-root postcondition is unconfirmed. Never blindly repeat refresh.
	if err := root.dir().Sync(); err != nil {
		return next, true, fmt.Errorf("canonical credential published; directory sync failed: %w", err)
	}
	if err := root.checkLock(lock); err != nil {
		return next, true, fmt.Errorf("canonical credential published into changed custody: %w", err)
	}
	return next, true, readinessErr
}
