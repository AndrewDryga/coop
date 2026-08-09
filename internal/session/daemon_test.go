package session

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// verifyWALMode has to fail loudly, not silently, when a connection cannot actually enter WAL
// mode: every Store method depends on WAL's concurrent-reader guarantees, and SQLite's own
// behavior here is to leave journal_mode unchanged rather than error — so this file's contract
// only holds if Open notices the mismatch itself. A connection that asserts the file is
// immutable is a real, portable way to make SQLite do exactly that (verified empirically
// against this package's driver: journal_mode stays "delete", with no error from the pragma).
func TestVerifyWALModeFailsLoudlyOnANonWALConnection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.sqlite")
	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	seed, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	immutable := &url.URL{Scheme: "file", Path: path, RawQuery: "immutable=1"}
	nonWAL, err := sql.Open("sqlite", immutable.String())
	if err != nil {
		t.Fatal(err)
	}
	defer nonWAL.Close()
	if err := verifyWALMode(nonWAL); err == nil ||
		!strings.Contains(err.Error(), "enable WAL") || !strings.Contains(err.Error(), "got") {
		t.Fatalf("non-WAL verification error = %v, want an \"enable WAL: got ...\" error", err)
	}

	// The positive case is exercised end to end by every other test that calls Open; assert it
	// directly here too so this test fully specifies the function on its own.
	writable, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if err := verifyWALMode(writable); err != nil {
		t.Fatalf("plain writable connection should enter WAL mode: %v", err)
	}
}

// One state root, one daemon: every guarantee this package makes about the operations journal
// assumes a single writer, so the kernel flock in openStateLock is the authority that enforces
// it. flock conflicts across separate open file descriptions, so a second Open is refused even
// from inside this process — which is what makes the invariant testable without spawning one.
// The release half matters just as much: a lock that outlived Close would lock an operator out
// of their own state root until the process exited.
func TestOpenRefusesASecondDaemonOnTheSameStateRoot(t *testing.T) {
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	second, err := Open(root)
	if err == nil {
		_ = second.Close()
		t.Fatal("second Open on a held state root succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "another session daemon owns this state root") {
		t.Fatalf("second Open error = %v, want it to name the owning daemon", err)
	}

	// A different root is unaffected — the lock is per state root, not process-wide.
	other, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open of an unrelated state root: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("Open after the owner closed: %v — the lock outlived its Store", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

// The default generator itself — the one real callers get, as opposed to the counting
// stand-in every other test in this package injects via WithIDGenerator — is otherwise never
// exercised in this suite. Prove it produces the shape the schema and callers depend on.
func TestRandomIDProducesAPrefixedUniqueID(t *testing.T) {
	first := randomID("op")
	second := randomID("op")
	if !strings.HasPrefix(first, "op_") || len(first) != len("op_")+32 {
		t.Fatalf("randomID shape = %q, want \"op_\" + 32 hex chars", first)
	}
	if first == second {
		t.Fatalf("randomID produced the same id twice: %q", first)
	}
}

// randomID's panic on crypto/rand failure is documented (see the comment on randomID) to be
// unreachable dead code on every platform coop ships for, and empirically impossible to
// exercise in a test at all: crypto/rand.Read itself aborts the whole process via an
// unrecoverable runtime fatal error rather than returning normally, so there is no way to make
// it return a non-nil error without crashing the test binary. What IS both real and testable is
// the store.id seam (the same func var randomID plugs into by default): if a caller-supplied
// generator ever panics for any reason, the store's transaction handling must not leave a
// half-written reservation behind, and must remain usable afterward. That is the actual safety
// property worth locking down here.
func TestFailingIDGeneratorPanicsWithoutCorruptingState(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	// Let the operation reservation itself succeed normally — a real row is inserted
	// mid-transaction — and panic only once CreateSession asks for the session's own id, so
	// the rollback this test checks for has something real to undo, not an empty no-op.
	store.id = func(prefix string) string {
		if prefix == "ses" {
			panic("entropy exhausted")
		}
		return prefix + "-ok"
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected the failing id generator to panic")
			}
		}()
		_, _ = store.CreateSession(ctx, "panic-key", CreateSessionRequest{Target: "target"})
	}()

	// The deferred tx.Rollback() must still run during the panic's stack unwind: the
	// operation row reserveOperationTx already inserted must not survive it, even though it
	// was written and visible within this same transaction before the panic hit.
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM operations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("operations table after a panicking id generator: count=%d err=%v", count, err)
	}

	// And the store keeps working normally afterward: the panic doesn't wedge the connection,
	// leave a stale transaction open, or poison the idempotency key it was reserving.
	store.id = func(prefix string) string { return prefix + "-recovered" }
	sess, err := store.CreateSession(ctx, "panic-key", CreateSessionRequest{Target: "target"})
	if err != nil || sess.ID != "ses-recovered" {
		t.Fatalf("store unusable after a panicking id generator: %+v, %v", sess, err)
	}
}
