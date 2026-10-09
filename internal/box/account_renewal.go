package box

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

const accountRenewalName = "renewal.json"

var errAccountRenewalUncertain = errors.New("account renewal needs recovery or a fresh host sign-in; automatic retry refused")

// The intent precedes provider I/O. A crash with no response is still uncertain: the
// provider might have consumed the old refresh grant before the connection broke.
type accountRenewal struct {
	Version  int    `json:"version"`
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Epoch    uint64 `json:"epoch"`
	Revision uint64 `json:"revision"`
	Received []byte `json:"received,omitempty"`
}

func renewAccountAuthority(ctx context.Context, cfg *config.Config, spec accountAuthoritySpec, account string,
	needed func(*accountAuthority) (bool, error),
	renew func(*accountAuthority, func([]byte) error) (*accountAuthority, error)) (*accountAuthority, bool, error) {
	root, lock, current, err := lockAccountAuthority(ctx, cfg, spec, account)
	if err != nil {
		return nil, false, err
	}
	defer root.close()
	defer lock.Close()
	if current == nil || current.Revoked {
		return current, false, errors.New("account needs host sign-in")
	}
	if err := ctx.Err(); err != nil {
		return current, false, err
	}
	if needed != nil {
		shouldRenew, err := needed(cloneAccountAuthority(current))
		if err != nil || !shouldRenew {
			return current, false, err
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return current, false, err
	}
	pending := accountRenewal{Version: 1, ID: hex.EncodeToString(nonce[:]), Provider: current.Provider,
		Account: current.Account, Epoch: current.Epoch, Revision: current.Revision}
	if err := root.writeRenewal(lock, nil, pending); err != nil {
		return current, false, err
	}
	retain := func(received []byte) error {
		if len(received) == 0 || len(received) > 1<<20 {
			return errors.New("renewal response exceeds recovery bounds")
		}
		next := pending
		next.Received = bytes.Clone(received)
		if err := root.writeRenewal(lock, &pending, next); err != nil {
			return err
		}
		pending = next
		return nil
	}
	if err := ctx.Err(); err != nil {
		return current, false, errors.Join(err, root.clearRenewal(lock, pending))
	}
	next, readinessErr := renew(cloneAccountAuthority(current), retain)
	if next == nil {
		return current, false, errors.Join(errAccountRenewalUncertain, readinessErr)
	}
	// A received response is required even when the adapter cannot use its grant. Never
	// acknowledge canonical publication based only on an in-memory rotation.
	if len(pending.Received) == 0 || next.Epoch != current.Epoch || next.Revoked {
		return current, false, errors.Join(errAccountRenewalUncertain, errors.New("renewal returned without response custody"), readinessErr)
	}
	next.Renewal = pending.ID
	published, effect, err := root.publish(lock, spec, account, current, next, nil)
	if err != nil {
		return published, effect, errors.Join(err, readinessErr)
	}
	if err := root.clearRenewal(lock, pending); err != nil {
		return published, true, errors.Join(err, readinessErr)
	}
	return published, true, readinessErr
}

func (root *accountAuthorityRoot) readRenewal() (*accountRenewal, error) {
	file, err := safefile.OpenRegular(root.dir(), accountRenewalName)
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
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errAccountRenewalUncertain
	}
	var pending accountRenewal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&pending) != nil || decoder.Decode(new(any)) != io.EOF || pending.Version != 1 ||
		pending.ID == "" || pending.Epoch == 0 || pending.Revision == 0 || len(pending.Received) > 1<<20 {
		return nil, errAccountRenewalUncertain
	}
	return &pending, nil
}

func (root *accountAuthorityRoot) recoverRenewal(lock *os.File, current *accountAuthority) error {
	pending, err := root.readRenewal()
	if err != nil || pending == nil {
		return err
	}
	// Publication may have succeeded before process death or the final directory sync
	// response. The exact canonical receipt proves which rotation won; age never does.
	if current != nil && !current.Revoked && current.Renewal == pending.ID && current.Provider == pending.Provider &&
		current.Account == pending.Account && current.Epoch == pending.Epoch && current.Revision > pending.Revision &&
		current.Revision-pending.Revision == 1 {
		return root.clearRenewal(lock, *pending)
	}
	return errAccountRenewalUncertain
}

func sameAccountRenewal(a, b *accountRenewal) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (root *accountAuthorityRoot) writeRenewal(lock *os.File, before *accountRenewal, next accountRenewal) error {
	data, err := json.Marshal(next)
	if err != nil || len(data) > 2<<20 {
		return errors.New("renewal recovery serialization failed")
	}
	name := accountRenewalName
	if before != nil {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		name = ".renewal-" + hex.EncodeToString(nonce[:])
	}
	fd, err := unix.Openat(int(root.dir().Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	if before != nil {
		defer unix.Unlinkat(int(root.dir().Fd()), name, 0)
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	if err := root.checkLock(lock); err != nil {
		return err
	}
	if before != nil {
		actual, err := root.readRenewal()
		if err != nil || !sameAccountRenewal(before, actual) {
			return errors.Join(errAccountRenewalUncertain, err)
		}
		if err := unix.Renameat(int(root.dir().Fd()), name, int(root.dir().Fd()), accountRenewalName); err != nil {
			return err
		}
	}
	if err := root.dir().Sync(); err != nil {
		return fmt.Errorf("renewal recovery durability unconfirmed: %w", err)
	}
	return root.checkLock(lock)
}

func (root *accountAuthorityRoot) clearRenewal(lock *os.File, pending accountRenewal) error {
	if err := root.checkLock(lock); err != nil {
		return err
	}
	actual, err := root.readRenewal()
	if err != nil || !sameAccountRenewal(&pending, actual) {
		return errors.Join(errAccountRenewalUncertain, err)
	}
	if err := unix.Unlinkat(int(root.dir().Fd()), accountRenewalName, 0); err != nil {
		return err
	}
	return errors.Join(root.dir().Sync(), root.checkLock(lock))
}

// replaceAccountAuthority is reserved for an explicit completed host sign-in or
// confirmed removal. It never retries a pending grant: a fresh epoch supersedes it,
// and the received bytes remain in private, non-serving recovery storage.
func replaceAccountAuthority(ctx context.Context, cfg *config.Config, spec accountAuthoritySpec, account string,
	replacement *accountAuthority) (*accountAuthority, bool, error) {
	root, lock, current, err := lockAccountAuthorityState(ctx, cfg, spec, account)
	if err != nil {
		return nil, false, err
	}
	defer root.close()
	defer lock.Close()
	if replacement == nil {
		return current, false, errors.New("missing account replacement")
	}
	next := cloneAccountAuthority(replacement)
	next.Epoch, next.Renewal = 1, ""
	if current != nil {
		next.Epoch = current.Epoch + 1
	}
	if err := ctx.Err(); err != nil {
		return current, false, err
	}
	next, effect, err := root.publish(lock, spec, account, current, next, nil)
	if err != nil {
		return next, effect, err
	}
	if err := root.archiveRenewal(lock); err != nil {
		return next, true, fmt.Errorf("account replaced; renewal recovery archival incomplete: %w", err)
	}
	return next, true, nil
}

func (root *accountAuthorityRoot) archiveRenewal(lock *os.File) error {
	file, err := safefile.OpenRegular(root.dir(), accountRenewalName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := privateAccountFile(file); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.Join(errAccountRenewalUncertain, err)
	}
	name := fmt.Sprintf("recovery-%x.json", sha256.Sum256(data))
	saved, readErr := safefile.ReadRegular(root.dir(), name, 2<<20)
	if errors.Is(readErr, os.ErrNotExist) {
		if err := writeAccountPrivate(root, lock, name, data); err != nil {
			return err
		}
	} else if readErr != nil || !bytes.Equal(saved, data) {
		return errors.Join(errAccountRenewalUncertain, readErr)
	}
	if err := root.checkLock(lock); err != nil {
		return err
	}
	current, err := safefile.OpenRegular(root.dir(), accountRenewalName)
	if err != nil {
		return err
	}
	same := sameAccountInode(file, current)
	_ = current.Close()
	if !same {
		return errAccountRenewalUncertain
	}
	if err := unix.Unlinkat(int(root.dir().Fd()), accountRenewalName, 0); err != nil {
		return err
	}
	return errors.Join(root.dir().Sync(), root.checkLock(lock))
}
