package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/egress"
)

// Retention removes only the policy snapshots a launch admission created, and
// marks each one when it does (admitted-<fingerprint>.json). Nothing else is
// ever removed: a controller job's snapshot is named by its session row for as
// long as that row exists, rows are never deleted, and they live in session
// stores under any --state, which this store cannot see; a snapshot written
// before the marker existed may be such a session's too. One whose own rule says
// coop net setup's check admitted it cannot be, so the record of the check's run
// marks it as that record goes, dated to when the run ended. The marker's
// modification time is when the snapshot was last admitted or loaded, which
// covers an admission still on its way to its run and a loop or editor session
// that loads it again for each box.

// admittedVersion is the format of an admitted marker.
const admittedVersion = 1

func snapshotRecord(fingerprint string) string { return "snapshot-" + fingerprint + ".json" }

func admittedRecord(fingerprint string) string { return "admitted-" + fingerprint + ".json" }

// SetupOrigin is the origin coop net setup gives the one rule its check
// admits. Retention recognizes by it the check snapshots earlier versions wrote
// without a marker, so it must not change.
func SetupOrigin() egress.Origin { return egress.Origin{Kind: "operator", Name: "net-setup"} }

// fromSetupCheck reports whether a snapshot is a setup check's policy: coop net
// setup granted every rule in it, and it carries no provider, since the check
// runs no agent. No launch or session admits a rule with that origin.
func fromSetupCheck(snapshot egress.Snapshot) bool {
	if len(snapshot.Grants) == 0 || len(snapshot.Dependencies) != 0 {
		return false
	}
	for _, grant := range snapshot.Grants {
		if len(grant.Origins) != 1 || grant.Origins[0] != SetupOrigin() {
			return false
		}
	}
	return true
}

type admittedSnapshot struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

// lockSnapshots orders what puts a removable snapshot to use, admitting or
// loading it, against retention's last look before it removes one: the snapshot
// is either marked in use before that look or found gone after it, never removed
// in between. Both sides hold it for a few file operations. RemoveExpired takes
// it inside a run's lock to mark a setup check's snapshot, so whoever holds it
// must never take a run's lock: the two orders would deadlock.
func (s *Store) lockSnapshots(ctx context.Context, operation func() error) error {
	ctx, cancel := context.WithTimeout(ctx, ExecutionLockTimeout)
	defer cancel()
	return s.lockFile(ctx, "snapshot-lock", operation)
}

// markAdmitted makes a snapshot an admission just created removable once it is
// unused. The caller holds lockSnapshots.
func (s *Store) markAdmitted(fingerprint string) error {
	data, err := json.Marshal(admittedSnapshot{Version: admittedVersion, Fingerprint: fingerprint})
	if err != nil {
		return err
	}
	if err := s.publish(admittedRecord(fingerprint), data, false); !errors.Is(err, os.ErrExist) {
		return err
	}
	// A marker left by a removal that stopped halfway: it marks this snapshot now.
	if err := s.touchAdmitted(fingerprint); err != nil {
		return err
	}
	return s.confirmPublication()
}

// touchAdmitted restarts a removable snapshot's retention window. The caller
// holds lockSnapshots.
func (s *Store) touchAdmitted(fingerprint string) error {
	return s.setAdmittedTime(fingerprint, time.Now())
}

// setAdmittedTime records when a removable snapshot was last used. It sets the
// time on the marker's own name, never through a link, and reports a missing
// marker as os.ErrNotExist. The caller holds lockSnapshots.
func (s *Store) setAdmittedTime(fingerprint string, used time.Time) error {
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	at := unix.NsecToTimespec(used.UnixNano())
	return unix.UtimesNanoAt(int(dir.Fd()), admittedRecord(fingerprint), []unix.Timespec{at, at}, unix.AT_SYMLINK_NOFOLLOW)
}

// markSetupCheck makes removable the snapshot a setup check's run ran under,
// when an earlier coop wrote it without a marker, and dates the marker to when
// that run ended: nothing used the snapshot after it. RemoveExpired calls this
// under the run's lock, before it removes the run's record, the last thing that
// names the snapshot. A snapshot already marked, gone, a link, or not proved a
// setup check's by its own rules is left as it is. An error means it could not
// look, and keeps the record.
func (s *Store) markSetupCheck(ctx context.Context, fingerprint string, ended time.Time) error {
	if !lowerHex(fingerprint, 64) {
		return nil
	}
	// A snapshot this coop's setup admitted is marked already.
	if _, err := s.root.Lstat(admittedRecord(fingerprint)); err == nil {
		return nil
	}
	return s.lockSnapshots(ctx, func() error {
		_, err := s.root.Lstat(admittedRecord(fingerprint))
		if err == nil {
			return nil // marked since the look above
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// read opens without following links and accepts only a private regular
		// file. A snapshot that is gone needs no mark, and a link, which read
		// refuses with ELOOP, proves nothing, so the record goes and nothing is
		// marked. Any other error, a file that is not private or a failed read
		// such as one out of descriptors, keeps the record for the next pass.
		data, err := s.read(snapshotRecord(fingerprint), maxPrivateRecordBytes)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ELOOP) {
			return nil
		}
		if err != nil {
			return err
		}
		var snapshot egress.Snapshot
		if strictJSON(data, &snapshot) != nil || snapshot.ValidateRecorded() != nil || snapshot.Fingerprint != fingerprint || !fromSetupCheck(snapshot) {
			return nil
		}
		if err := s.markAdmitted(fingerprint); err != nil {
			return err
		}
		return s.setAdmittedTime(fingerprint, ended)
	})
}

// RemoveUnusedSnapshots deletes the snapshots retention may let go: an admission
// created it, no run in inUse names it, and nothing admitted or loaded it within
// ExecutionRetention before now. inUse must hold the policy of every retained
// run, so a caller that could not read them all must not call this, and a nil
// inUse, a census nobody took, removes nothing. A directory it cannot list
// whole removes nothing either. It reports how many it removed; one it could not
// judge or remove stays for the next pass.
func (e *Evidence) RemoveUnusedSnapshots(ctx context.Context, inUse map[string]bool, now time.Time) (int, error) {
	if inUse == nil {
		return 0, errors.New("no census of the snapshots retained runs use")
	}
	dir, err := e.files.root.Open(".")
	if err != nil {
		return 0, err
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures []error
	for _, name := range names {
		fingerprint, ok := admittedFingerprint(name)
		if !ok || inUse[fingerprint] {
			continue
		}
		// Most markers are young; only an old one is worth the lock.
		if marker, err := e.files.root.Lstat(name); err != nil || now.Sub(marker.ModTime()) <= ExecutionRetention {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		gone, err := e.files.removeUnusedSnapshot(ctx, fingerprint, now)
		if gone {
			removed++
		}
		failures = append(failures, err)
	}
	return removed, errors.Join(failures...)
}

// admittedFingerprint is the fingerprint an admitted-<fingerprint>.json name carries.
func admittedFingerprint(name string) (string, bool) {
	fingerprint, found := strings.CutPrefix(name, "admitted-")
	if !found {
		return "", false
	}
	fingerprint, found = strings.CutSuffix(fingerprint, ".json")
	return fingerprint, found && lowerHex(fingerprint, 64)
}

// removeUnusedSnapshot looks once more under the snapshot lock, where nothing can
// put it to use. A marker used inside the window, either name that is a link or
// not a private regular file, and a snapshot that does not read as the recorded
// snapshot its name promises keep both. The snapshot goes before its marker, so
// a snapshot found without one was never marked, and a load needs no lock to
// trust that it stays: only an earlier coop's setup check snapshot is marked
// later, once the one run that loads it has ended. A removal that stops halfway
// leaves a marker the next pass clears.
func (s *Store) removeUnusedSnapshot(ctx context.Context, fingerprint string, now time.Time) (bool, error) {
	removed := false
	err := s.lockSnapshots(ctx, func() error {
		marker, err := s.root.Lstat(admittedRecord(fingerprint))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if privateInfo(marker, false) != nil || now.Sub(marker.ModTime()) <= ExecutionRetention {
			return nil
		}
		name := snapshotRecord(fingerprint)
		// read opens without following links and accepts only a private regular file.
		data, err := s.read(name, maxPrivateRecordBytes)
		if errors.Is(err, os.ErrNotExist) {
			return s.root.Remove(admittedRecord(fingerprint)) // it marks nothing
		}
		if err != nil {
			return err
		}
		var snapshot egress.Snapshot
		if strictJSON(data, &snapshot) != nil || snapshot.ValidateRecorded() != nil || snapshot.Fingerprint != fingerprint {
			return nil
		}
		// os.Root unlinks the names themselves. No directory sync: a removal a
		// crash undoes is found unused again by the next pass.
		if err := s.root.Remove(name); err != nil {
			return err
		}
		removed = true
		return s.root.Remove(admittedRecord(fingerprint))
	})
	return removed, err
}
