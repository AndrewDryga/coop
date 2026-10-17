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
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

const (
	accountRenewalName = "renewal.json"
	accountRetryName   = "retry.json"
)

var errAccountRenewalUncertain = errors.New("an interrupted credential renewal could not be finished; run coop login")

// accountRetry is the last renewal exchange that failed for a passing reason, At in Unix milliseconds.
// Every run on the account waits Delay milliseconds after it, so the runs take turns asking the
// provider instead of each holding the account's lock through an exchange that may hang. It names the
// record it was written for, so any publication ends it.
type accountRetry struct {
	Epoch    uint64 `json:"epoch"`
	Revision uint64 `json:"revision"`
	At       int64  `json:"at"`
	Delay    int64  `json:"delay"`
}

// The intent precedes provider I/O. Received is the provider's 200 answer, kept before anything
// parses it, and ReceivedAt (Unix milliseconds) when it arrived; settleRenewal finishes from them.
type accountRenewal struct {
	Version    int    `json:"version"`
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Account    string `json:"account"`
	Epoch      uint64 `json:"epoch"`
	Revision   uint64 `json:"revision"`
	Received   []byte `json:"received,omitempty"`
	ReceivedAt int64  `json:"received_at,omitempty"`
}

// renewAccountAuthority renews the account under its lock. needed decides whether to renew now; it is
// given the time before which no exchange should follow one that failed for a passing reason, zero
// when there was none.
func renewAccountAuthority(ctx context.Context, cfg *config.Config, spec accountAuthoritySpec, account string,
	needed func(current *accountAuthority, retryAt time.Time) (bool, error),
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
		var retryAt time.Time
		if retry := root.retry(current, time.Now()); retry != nil {
			retryAt = time.UnixMilli(retry.At + retry.Delay)
		}
		shouldRenew, err := needed(cloneAccountAuthority(current), retryAt)
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
		next.Received, next.ReceivedAt = bytes.Clone(received), time.Now().UnixMilli()
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
		// Nothing to publish: settle whatever the attempt left, as a later read would, so a
		// failed exchange never leaves the account waiting for a sign-in it does not need.
		settled, err := root.settleRenewal(lock, spec, account, current)
		err = errors.Join(err, readinessErr)
		if settled == current && renewalRetryable(err) {
			root.waitAfterFailure(lock, current)
		}
		return settled, settled != current, err
	}
	if next.Epoch != current.Epoch || next.Revoked {
		return current, false, errors.Join(errAccountRenewalUncertain, errors.New("renewal changed the account's generation"), readinessErr)
	}
	// Publish even when the answer could not be kept: the issued grant exists only in memory
	// then, and publishing is what keeps it.
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

// retry returns the wait a failed exchange left for current, or nil. The wait only paces renewals, so
// one that cannot be read, or that Coop would not have written, paces nothing rather than hold one
// off. Coop writes waits of at most a minute, so one that ends more than a minute after now was
// written before the host clock stepped back (NTP, a resumed VM) and would hold renewals off far
// longer than its own wait.
func (root *accountAuthorityRoot) retry(current *accountAuthority, now time.Time) *accountRetry {
	data, err := safefile.ReadRegular(root.dir(), accountRetryName, 4096)
	var retry accountRetry
	if err != nil || json.Unmarshal(data, &retry) != nil || retry.Epoch != current.Epoch || retry.Revision != current.Revision ||
		retry.Delay <= 0 || retry.Delay > time.Minute.Milliseconds() || time.UnixMilli(retry.At+retry.Delay).After(now.Add(time.Minute)) {
		return nil
	}
	return &retry
}

// waitAfterFailure records that an exchange for current just failed for a passing reason: the next may
// follow 5 s later, a wait that doubles up to a minute while the failures go on. Like retry, it only
// paces: a wait that cannot be written leaves each run to its own backoff.
func (root *accountAuthorityRoot) waitAfterFailure(lock *os.File, current *accountAuthority) {
	now := time.Now()
	delay := 5 * time.Second
	if previous := root.retry(current, now); previous != nil {
		delay = min(2*time.Duration(previous.Delay)*time.Millisecond, time.Minute)
	}
	data, err := json.Marshal(accountRetry{Epoch: current.Epoch, Revision: current.Revision, At: now.UnixMilli(), Delay: delay.Milliseconds()})
	if err == nil {
		_ = writeAccountPrivate(root, lock, accountRetryName, data)
	}
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

// settleRenewal finishes what an earlier renewal of this account left in its journal and returns the
// record callers must use from then on; handing back the record read before it would replay a grant
// the provider already spent. A kept answer completes without contacting the provider. A journal
// with no answer is cleared so a later renewal can try again: the grant it sent may be spent, but
// then it is lost either way, and a retry only asks. A kept answer that cannot be used refuses until
// a fresh host sign-in, which archives it.
func (root *accountAuthorityRoot) settleRenewal(lock *os.File, spec accountAuthoritySpec, account string, current *accountAuthority) (*accountAuthority, error) {
	pending, err := root.readRenewal()
	if err != nil || pending == nil {
		return current, err
	}
	if current == nil || current.Provider != pending.Provider || current.Account != pending.Account || pending.Epoch > current.Epoch {
		return current, errAccountRenewalUncertain
	}
	switch {
	case pending.Epoch < current.Epoch:
		// A host sign-in or removal superseded it and could not archive it at the time.
		return current, root.archiveRenewal(lock)
	case current.Renewal == pending.ID && current.Revision == pending.Revision+1:
		// Publication landed before the journal was cleared; the exact receipt proves which won.
		return current, root.clearRenewal(lock, *pending)
	case current.Revoked || current.Revision != pending.Revision:
		return current, errAccountRenewalUncertain
	case len(pending.Received) == 0 || pending.ReceivedAt == 0 && keptErrorAnswer(pending.Received):
		// Nothing kept, or an error answer an earlier version kept (it kept every answer): no grant
		// was issued, so the old one may be sent again.
		return current, root.clearRenewal(lock, *pending)
	case spec.Complete == nil:
		return current, errAccountRenewalUncertain
	}
	// An answer kept before arrival times were recorded dates its grant as long expired, so the next
	// renewal spends the grant it carries instead of trusting its access token.
	at := time.Unix(0, 0)
	if pending.ReceivedAt > 0 {
		at = time.UnixMilli(pending.ReceivedAt)
	}
	next := cloneAccountAuthority(current)
	files, err := spec.Complete(next.Artifacts, pending.Received, at)
	if files == nil {
		return current, errors.Join(errAccountRenewalUncertain, err)
	}
	next.Artifacts, next.Renewal = files, pending.ID
	published, effect, err := root.publish(lock, spec, account, current, next, nil)
	if err != nil && !effect {
		// The kept answer is still the only copy of the grant; a host sign-in may supersede it.
		return current, errors.Join(errAccountRenewalUncertain, err)
	}
	if err != nil {
		return published, err
	}
	return published, root.clearRenewal(lock, *pending)
}

// keptErrorAnswer reports a kept answer that is an error document rather than issued credentials.
func keptErrorAnswer(received []byte) bool {
	var answer map[string]json.RawMessage
	if json.Unmarshal(received, &answer) != nil {
		return false
	}
	_, refused := answer["error"]
	_, issued := answer["access_token"]
	return refused && !issued
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
