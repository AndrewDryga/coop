package cli

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/ui"
	"github.com/AndrewDryga/coop/internal/workerconnector"
)

func TestSessionCheckFlagsTakeOneStateRoot(t *testing.T) {
	if state, err := parseSessionCheckFlags([]string{"--state", "/srv/state"}); err != nil || state != "/srv/state" {
		t.Fatalf("check flags = %q, %v", state, err)
	}
	if state, err := parseSessionCheckFlags(nil); err != nil || state != "" {
		t.Fatalf("no flags = %q, %v; want the default state root", state, err)
	}
	for _, args := range [][]string{
		{"--state"},
		{"--state", "--json"},
		{"--state", "/a", "--state", "/b"},
		{"--json"},
		{"/srv/state"},
	} {
		if _, err := parseSessionCheckFlags(args); err == nil || !strings.Contains(err.Error(), "sessions check") {
			t.Errorf("check flags accepted %v: %v", args, err)
		}
	}
}

// Each verdict has its own documented exit status, so an upgrade script can tell "swap",
// "back up, then swap" and "keep the old build" apart, and none of them writes to the data.
func TestRunSessionCheckReportsEachVerdictWithoutWriting(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, state string) (cleanup func())
		code    int
		want    []string
	}{
		{"no data yet", func(t *testing.T, state string) func() {
			// A service that started holds the lock before its first store.
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "lock"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}, 0, []string{"✓ No session data yet", "  This build will create it in <state>."}},
		{"no data directory", func(*testing.T, string) func() { return nil }, 1, []string{
			"✗ Could not check session data",
			"      There is no session data at <state>.",
		}},
		{"a directory no service used", func(t *testing.T, state string) func() {
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			return nil
		}, 1, []string{
			"✗ Could not check session data",
			"      There is no session data at <state>.",
			"  Check the --state path. A worker that has never run has nothing to check.",
		}},
		{"current", func(t *testing.T, state string) func() {
			createSessionStore(t, state)
			return nil
		}, 0, []string{
			"✓ This build can use the session data as it is",
			"  Data            <state>",
			fmt.Sprintf("  Session schema  %d", session.SchemaVersion),
		}},
		{"converts", func(t *testing.T, state string) func() {
			createSessionStore(t, state)
			setSessionSchema(t, state, session.SchemaVersion-1)
			return nil
		}, sessionCheckConverts, []string{
			"⚠ This build will convert the session data",
			fmt.Sprintf("  Session schema  %d → %d", session.SchemaVersion-1, session.SchemaVersion),
			"  Coop builds with an older session schema can't open it afterwards.",
			"  To keep a way back, back up the data directory while the service is stopped.",
		}},
		{"too new", func(t *testing.T, state string) func() {
			createSessionStore(t, state)
			setSessionSchema(t, state, session.SchemaVersion+1)
			return nil
		}, sessionCheckRefused, []string{
			"✗ This build can't use the session data",
			fmt.Sprintf("      Session schema %d is newer than this coop build supports (%d to %d).",
				session.SchemaVersion+1, session.OldestSchemaVersion, session.SchemaVersion),
			"  Use a newer coop build, such as the one that wrote this data.",
		}},
		{"worker records from a newer build", func(t *testing.T, state string) func() {
			createSessionStore(t, state)
			writeConnectorRecord(t, state, "commands", "newer.json", `{"version":2}`)
			return nil
		}, sessionCheckRefused, []string{
			"✗ This build can't use the session data",
			"      Found 1 worker command receipt from a newer coop build (this build reads version 1).",
			"  Use a newer coop build, such as the one that wrote this data.",
		}},
		{"damaged worker record", func(t *testing.T, state string) func() {
			createSessionStore(t, state)
			writeConnectorRecord(t, state, "event-streams", "torn.json", `{"version":1}`)
			return nil
		}, 1, []string{
			"✗ Could not check session data",
			"      This build can't read 1 worker record:",
			"      torn.json: worker event stream is malformed",
		}},
		{"service running", func(t *testing.T, state string) func() {
			store, err := session.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			return func() { _ = store.Close() }
		}, 1, []string{
			"✗ Could not check session data",
			"      The session service is using this data directory.",
			"  Stop the service, then run the command again.",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			if cleanup := tc.prepare(t, state); cleanup != nil {
				defer cleanup()
			}
			before := stateRootSnapshot(t, state)
			var lines []string
			ui.SetLiveSink(func(line string) { lines = append(lines, line) })
			code, err := runSessionCheck(state)
			ui.SetLiveSink(nil)
			if after := stateRootSnapshot(t, state); !maps.Equal(before, after) {
				t.Fatalf("sessions check changed the state root:\n before: %v\n  after: %v", before, after)
			}
			output := strings.Join(lines, "\n")
			if err != nil {
				output += usageBlock(t, err)
			}
			if code != tc.code {
				t.Fatalf("exit status = %d, want %d; output:\n%s", code, tc.code, output)
			}
			for _, want := range tc.want {
				if want = strings.ReplaceAll(want, "<state>", state); !strings.Contains(output, want) {
					t.Errorf("output lacks %q:\n%s", want, output)
				}
			}
		})
	}
}

// Nothing a released build writes is too old yet, so this renders the verdict a later build
// returns once a waypoint is set: the refusal names the converting schema and exits 4.
func TestSessionCheckRefusesATooOldStoreWithItsWaypoint(t *testing.T) {
	refusal := errors.New("session schema 24 is older than this coop build supports (25 to 26); " +
		"open it once with a coop build that uses session schema 25, then upgrade")
	code, err := reportSessionCheck("/srv/state", session.StoreCheck{Status: session.StoreTooOld, Version: 24, Refusal: refusal},
		[]workerconnector.RecordCheck{{Noun: "worker command receipt", Version: 1, Current: 3}})
	if code != sessionCheckRefused || err == nil {
		t.Fatalf("too-old store = (%d, %v), want exit %d with a refusal", code, err, sessionCheckRefused)
	}
	got := usageBlock(t, err)
	want := "      Session schema 24 is older than this coop build supports (25 to 26); " +
		"open it once with a coop build that uses session schema 25, then upgrade."
	if !strings.Contains(got, "✗ This build can't use the session data") || !strings.Contains(got, want) ||
		strings.Contains(got, "newer coop build") {
		t.Fatalf("too-old refusal:\n%s", got)
	}
}

func createSessionStore(t *testing.T, state string) {
	t.Helper()
	store, err := session.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// setSessionSchema relabels a stopped store's schema, the one fact the check decides by.
func setSessionSchema(t *testing.T, state string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeConnectorRecord(t *testing.T, state, kind, name, body string) {
	t.Helper()
	dir := filepath.Join(connectorJournalDir(state), kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stateRootSnapshot records each entry under the state root, the root included: type and
// permissions, size, modification time and contents. A file created and removed again still shows,
// as a changed directory modification time.
func stateRootSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return map[string]string{".": "absent"}
	}
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		entry := fmt.Sprintf("%v %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += fmt.Sprintf(" %x", sha256.Sum256(data))
		}
		rel, err := filepath.Rel(root, path)
		snapshot[rel] = entry
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
