package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

// The private journal retains originals outside native discovery paths. Publish
// only after retiring every serving grant; never roll back to these old bytes.
type nativeCutover struct {
	ID       string                `json:"id"`
	Provider string                `json:"provider"`
	Account  string                `json:"account"`
	Next     *accountAuthority     `json:"next"`
	Sources  []nativeCutoverSource `json:"sources"`
}
type nativeCutoverSource struct {
	Root, Name    string
	Device, Inode uint64
	Digest        string
	Data          []byte
	Retire        bool
}

func readCutoverSource(path, name string, limit int64, retire bool) (nativeCutoverSource, error) {
	value := nativeCutoverSource{Root: path, Name: name, Retire: retire}
	root, err := safefile.OpenRoot(path)
	if err != nil {
		return value, err
	}
	defer root.Close()
	f, err := safefile.OpenRegular(root, name)
	if err != nil {
		return value, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return value, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || before.Size() < 0 || before.Size() > limit {
		return value, errors.New("legacy credential is not a bounded single-link file")
	}
	value.Data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return value, err
	}
	after, err := f.Stat()
	if err != nil || int64(len(value.Data)) > limit || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return value, errors.New("legacy credential changed during read")
	}
	current, err := safefile.OpenRegular(root, name)
	if err != nil {
		return value, err
	}
	same := sameAccountInode(f, current)
	_ = current.Close()
	if !same {
		return value, errors.New("legacy credential was replaced during read")
	}
	value.Device, value.Inode = uint64(stat.Dev), stat.Ino
	value.Digest = fmt.Sprintf("%x", sha256.Sum256(value.Data))
	return value, nil
}

func legacyAccountSources(cfg *config.Config, ag agents.Agent, account string) ([]nativeCutoverSource, error) {
	native := ag.NativeCredentials()
	home := cfg.AgentProfileDir(ag.Name(), account)
	var sources []nativeCutoverSource
	for _, artifact := range native.Artifacts {
		value, err := readCutoverSource(home, artifact.Name, artifact.Limit, slices.Contains(native.LegacyGrants, artifact.Name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sources = append(sources, value)
	}
	if host := ag.HostCredential(); host.Declared() {
		dir, err := hostCredentialDir(cfg, ag.Name(), account)
		if err != nil {
			return nil, err
		}
		value, err := readCutoverSource(dir, host.File, hostCredentialSizeLimit, true)
		if err == nil {
			sources = append(sources, value)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return sources, nil
}

func cutoverFiles(sources []nativeCutoverSource) map[string][]byte {
	files := map[string][]byte{}
	for _, source := range sources {
		files[source.Name] = bytes.Clone(source.Data)
	}
	return files
}

// Supported launch/maintenance paths hold this lease. Inventory additionally
// covers old and restartable containers which do not know about the lease.
type legacyAccountFence struct {
	home  string
	root  *os.File
	lease *credentialLease
	locks []*os.File
}

func (f *legacyAccountFence) close() {
	for _, lock := range f.locks {
		_ = lock.Close()
	}
	if f.root != nil {
		_ = f.root.Close()
	}
	f.lease.close()
}
func (f *legacyAccountFence) check() error {
	named, err := safefile.OpenRoot(f.home)
	if err != nil {
		return err
	}
	same := sameAccountInode(named, f.root)
	_ = named.Close()
	if !same {
		return errors.New("legacy credential directory changed")
	}
	for _, lock := range f.locks {
		current, err := safefile.OpenRegular(f.root, lock.Name())
		if err != nil {
			return err
		}
		same := sameAccountInode(lock, current)
		_ = current.Close()
		if !same {
			return errors.New("legacy credential writer lock changed")
		}
	}
	return nil
}

func legacyWriterLockOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func (f *legacyAccountFence) addWriter(name string) error {
	fd, err := unix.Openat(int(f.root.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), name)
	f.locks = append(f.locks, lock)
	return f.lockWriter(lock)
}

func (f *legacyAccountFence) lockWriter(lock *os.File) error {
	info, err := lock.Stat()
	if err != nil {
		return err
	}
	if !legacyWriterLockOwned(info) {
		return errors.New("legacy credential writer lock must be an owned single-link regular file")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("legacy native credential writer is busy; retry after it exits")
	}
	if err := f.check(); err != nil {
		return err
	}
	info, err = lock.Stat()
	if err != nil {
		return err
	}
	if !legacyWriterLockOwned(info) {
		return errors.New("legacy credential writer lock changed")
	}
	// Native clients may create public PID locks. Tighten the owned, exclusively fenced inode;
	// replacing it would let a native writer keep using a different lock from our cutover.
	if info.Mode().Perm() != 0o600 {
		if err := lock.Chmod(0o600); err != nil {
			return err
		}
	}
	return privateAccountFile(lock)
}

func fenceLegacyAccount(ctx context.Context, cfg *config.Config, rt runtime.Runtime, ag agents.Agent, account string) (*legacyAccountFence, error) {
	home := cfg.AgentProfileDir(ag.Name(), account)
	lease, err := credentialUseLease(ctx, cfg, home, true)
	if err != nil {
		return nil, err
	}
	fence := &legacyAccountFence{home: home, lease: lease}
	release := fence.close
	if err := checkCredentialContainers(ctx, rt, ag.Name(), lease.key, true); err != nil {
		release()
		return nil, err
	}
	root, err := safefile.OpenRoot(home)
	if err != nil {
		release()
		return nil, err
	}
	fence.root = root
	for _, name := range ag.NativeCredentials().LegacyLocks {
		if err := fence.addWriter(name); err != nil {
			release()
			return nil, err
		}
	}
	if err := checkLegacyMounts(ctx, cfg, rt, ag, account); err != nil {
		release()
		return nil, err
	}
	if err := fence.check(); err != nil {
		release()
		return nil, err
	}
	return fence, nil
}

func checkLegacyMounts(ctx context.Context, cfg *config.Config, rt runtime.Runtime, ag agents.Agent, account string) error {
	paths := []string{cfg.AgentProfileDir(ag.Name(), account)}
	if ag.HostCredential().Declared() {
		dir, err := hostCredentialDir(cfg, ag.Name(), account)
		if err != nil {
			return err
		}
		paths = append(paths, dir)
	}
	return checkLegacyMountPaths(ctx, rt, false, paths...)
}

func checkLegacyMountPaths(ctx context.Context, rt runtime.Runtime, writable bool, paths ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var sources []string
	var err error
	if writable {
		sources, err = rt.WritableBindSourcesByLabels(ctx, nil, true)
	} else {
		sources, err = rt.BindSourcesByLabels(ctx, nil)
	}
	if err != nil {
		return errors.New("legacy writer inventory is unavailable; originals retained unchanged")
	}
	within := func(child, parent string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	for _, path := range paths {
		canonical, err := filepath.EvalSymlinks(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, source := range sources {
			source, err = filepath.EvalSymlinks(source)
			if err != nil {
				return errors.New("legacy writer mount cannot be resolved")
			}
			if within(canonical, source) || within(source, canonical) {
				return errors.New("legacy credential home is mounted by a running or restartable container; finish that session before retrying")
			}
		}
	}
	return nil
}

// Cutover is not fresh sign-in. Tombstones and uncertain canonical authority
// never authorize reimport; only this exact retained receipt can resume.
func ensureNativeAccount(ctx context.Context, cfg *config.Config, rt runtime.Runtime, ag agents.Agent, account string) (*accountAuthority, bool, error) {
	cfg = cfg.NativeAuthorityConfig()
	record, exists, err := readNativeAccount(ctx, cfg, ag, account)
	if exists && record != nil {
		return record, true, err
	}
	_, pendingErr := os.Lstat(filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", account, "cutover.json"))
	_, bootstrapErr := os.Lstat(filepath.Join(cfg.ConfigDir, ag.Name(), "cutovers", account, "cutover.json"))
	if exists && pendingErr != nil && bootstrapErr != nil {
		return nil, true, err
	}
	if check := ag.NativeCredentials().LegacyCheck; check != nil {
		if err := check(cfg.AgentProfileDir(ag.Name(), account)); err != nil {
			return nil, exists, err
		}
	}
	sources, sourceErr := legacyAccountSources(cfg, ag, account)
	if sourceErr != nil {
		return nil, exists, sourceErr
	}
	if !exists {
		serving := false
		for _, source := range sources {
			serving = serving || source.Retire
		}
		if !serving {
			return nil, false, nil
		}
	}
	fence, err := fenceLegacyAccount(ctx, cfg, rt, ag, account)
	if err != nil {
		return nil, exists, err
	}
	defer fence.close()
	return migrateNativeAccount(ctx, cfg, ag, account, func() error {
		if err := fence.check(); err != nil {
			return err
		}
		return checkLegacyMounts(ctx, cfg, rt, ag, account)
	})
}

func migrateNativeAccount(ctx context.Context, cfg *config.Config, ag agents.Agent, account string, checkWriters func() error) (*accountAuthority, bool, error) {
	if checkWriters == nil {
		return nil, false, errors.New("credential cutover requires a writer fence")
	}
	if check := ag.NativeCredentials().LegacyCheck; check != nil {
		if err := check(cfg.AgentProfileDir(ag.Name(), account)); err != nil {
			return nil, false, err
		}
	}
	if err := checkWriters(); err != nil {
		return nil, false, err
	}
	sources, err := legacyAccountSources(cfg, ag, account)
	if err != nil {
		return nil, false, err
	}
	// A durable bootstrap receipt precedes creation of canonical authority. A
	// failed mkdir/write/normalization must not strand an intact legacy account
	// behind an indistinguishable, empty canonical directory.
	bootstrap, bootstrapLock, data, err := prepareNativeCutover(ctx, cfg, ag, account, sources)
	if err != nil {
		return nil, false, err
	}
	defer bootstrap.close()
	defer bootstrapLock.Close()
	spec := nativeAccountSpec(ag)
	root, lock, current, err := lockAccountAuthorityState(ctx, cfg, spec, account)
	if err != nil {
		return nil, true, err
	}
	defer root.close()
	defer lock.Close()
	if current != nil {
		return current, true, nil
	}
	var journal nativeCutover
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&journal) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, true, errors.New("invalid credential cutover receipt; preserve for recovery")
	}
	if err := validateNativeCutover(cfg, ag, account, journal); err != nil {
		return nil, true, err
	}
	if saved, readErr := safefile.ReadRegular(root.dir(), "cutover.json", accountAuthorityLimit); errors.Is(readErr, os.ErrNotExist) {
		if err := writeAccountPrivate(root, lock, "cutover.json", data); err != nil {
			return nil, true, err
		}
	} else if readErr != nil || !bytes.Equal(saved, data) {
		return nil, true, errors.New("canonical cutover receipt differs from bootstrap; preserve both for recovery")
	}
	if err := checkWriters(); err != nil {
		return nil, true, err
	}
	for _, source := range journal.Sources {
		if err := checkWriters(); err != nil {
			return nil, true, err
		}
		if err := retireCutoverSource(source); err != nil {
			return nil, true, err
		}
	}
	if err := checkWriters(); err != nil {
		return nil, true, err
	}
	next, _, err := root.publish(lock, spec, account, nil, journal.Next, nil)
	if err == nil {
		if err = bootstrap.checkLock(bootstrapLock); err == nil {
			err = unix.Unlinkat(int(bootstrap.dir().Fd()), "cutover.json", 0)
		}
		if err == nil {
			err = bootstrap.dir().Sync()
		}
	}
	return next, true, err
}

func prepareNativeCutover(ctx context.Context, cfg *config.Config, ag agents.Agent, account string, sources []nativeCutoverSource) (*accountAuthorityRoot, *os.File, []byte, error) {
	root, err := openPrivateAccountTree(cfg.ConfigDir, ag.Name(), "cutovers", account)
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := root.lock(ctx)
	if err != nil {
		root.close()
		return nil, nil, nil, err
	}
	fail := func(err error) (*accountAuthorityRoot, *os.File, []byte, error) {
		_ = lock.Close()
		root.close()
		return nil, nil, nil, err
	}
	data, err := safefile.ReadRegular(root.dir(), "cutover.json", accountAuthorityLimit)
	if err == nil {
		return root, lock, data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	// If a prior publish retired sources but cleanup removed the bootstrap,
	// recover only its exact canonical journal, never newly discovered grants.
	canonical, openErr := safefile.OpenRoot(filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", account))
	if openErr == nil {
		data, err = safefile.ReadRegular(canonical, "cutover.json", accountAuthorityLimit)
		_ = canonical.Close()
	} else {
		err = openErr
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if errors.Is(err, os.ErrNotExist) {
		next, err := normalizeNativeAccount(ag, cutoverFiles(sources))
		if err != nil {
			return fail(err)
		}
		journal := nativeCutover{ID: randomHex(16), Provider: ag.Name(), Account: account, Next: next, Sources: sources}
		journal.Next.Epoch = 1
		journal.Next.Migration = journal.ID
		if err := validateNativeCutover(cfg, ag, account, journal); err != nil {
			return fail(err)
		}
		data, err = json.Marshal(journal)
		if err != nil {
			return fail(err)
		}
	}
	if err := writeAccountPrivate(root, lock, "cutover.json", data); err != nil {
		return fail(err)
	}
	return root, lock, data, nil
}

func validateNativeCutover(cfg *config.Config, ag agents.Agent, account string, journal nativeCutover) error {
	id, err := hex.DecodeString(journal.ID)
	if err != nil || len(id) != 16 || journal.Provider != ag.Name() || journal.Account != account || journal.Next == nil || journal.Next.Epoch != 1 || journal.Next.Migration != journal.ID || len(journal.Sources) == 0 || len(journal.Sources) > 9 {
		return errors.New("invalid credential cutover binding")
	}
	allowed := map[string]bool{}
	home := cfg.AgentProfileDir(ag.Name(), account)
	for _, artifact := range ag.NativeCredentials().Artifacts {
		allowed[filepath.Join(home, artifact.Name)] = slices.Contains(ag.NativeCredentials().LegacyGrants, artifact.Name)
	}
	if host := ag.HostCredential(); host.Declared() {
		dir, err := hostCredentialDir(cfg, ag.Name(), account)
		if err != nil {
			return err
		}
		allowed[filepath.Join(dir, host.File)] = true
	}
	seen := map[string]bool{}
	for _, source := range journal.Sources {
		path := filepath.Join(source.Root, source.Name)
		retire, ok := allowed[path]
		if !ok || seen[path] || !accountNameValid(source.Name) || retire != source.Retire || source.Digest != fmt.Sprintf("%x", sha256.Sum256(source.Data)) {
			return errors.New("invalid credential cutover source")
		}
		seen[path] = true
	}
	normalized, err := normalizeNativeAccount(ag, cutoverFiles(journal.Sources))
	if err != nil {
		return err
	}
	a, _ := json.Marshal(normalized.Artifacts)
	b, _ := json.Marshal(journal.Next.Artifacts)
	if !bytes.Equal(a, b) || normalized.Principal != journal.Next.Principal || normalized.Selection != journal.Next.Selection {
		return errors.New("credential cutover authority changed")
	}
	if journal.Next.Revoked || journal.Next.Renewal != "" {
		return errors.New("invalid initial credential cutover authority")
	}
	candidate := cloneAccountAuthority(journal.Next)
	candidate.Version, candidate.Provider, candidate.Account, candidate.Revision = 1, ag.Name(), account, 1
	return validateAccountAuthority(nativeAccountSpec(ag), account, candidate)
}

func retireCutoverSource(source nativeCutoverSource) error {
	current, err := readCutoverSource(source.Root, source.Name, 1<<20, source.Retire)
	if errors.Is(err, os.ErrNotExist) && source.Retire {
		return nil
	}
	if err != nil {
		return err
	}
	// Device numbers can change across a reboot while this durable journal waits.
	// The rooted name, inode and exact grant digest are the retirement witness.
	if current.Inode != source.Inode || current.Digest != source.Digest {
		return errors.New("legacy credentials changed during cutover; both copies retained for recovery")
	}
	if !source.Retire {
		return nil
	}
	root, err := safefile.OpenRoot(source.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := safefile.OpenRegular(root, source.Name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino != source.Inode {
		return errors.New("legacy credential replaced before retirement")
	}
	if err := unix.Unlinkat(int(root.Fd()), source.Name, 0); err != nil {
		return err
	}
	return root.Sync()
}

// Callers hold the permanent authority lock. Atomic rename prevents a crash
// from leaving a partial receipt; original grants are retired only afterward.
func writeAccountPrivate(root *accountAuthorityRoot, lock *os.File, name string, data []byte) error {
	if !accountNameValid(name) || len(data) > accountAuthorityLimit {
		return errors.New("invalid private account publication")
	}
	temporary := ".private-" + randomHex(16)
	fd, err := unix.Openat(int(root.dir().Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), temporary)
	defer unix.Unlinkat(int(root.dir().Fd()), temporary, 0)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := root.checkLock(lock); err != nil {
		return err
	}
	if err := unix.Renameat(int(root.dir().Fd()), temporary, int(root.dir().Fd()), name); err != nil {
		return err
	}
	return errors.Join(root.dir().Sync(), root.checkLock(lock))
}
