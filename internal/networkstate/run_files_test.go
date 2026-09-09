package networkstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prepareFixtureRunFiles(t *testing.T, s *Store, record Execution) (Execution, string) {
	t.Helper()
	record, err := s.PrepareRunFiles(context.Background(), record.ID, record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.RunFilesPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record, path
}

func TestRunFilesCompositionClosesBeforeAgentCreation(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	record, path := prepareFixtureRunFiles(t, s, record)
	if record.RunFiles.State != "preparing" || record.RunFiles.Inode == 0 || filepath.Dir(path) != s.Path() {
		t.Fatal("generated-file directory lacks private identity custody")
	}
	if err := os.WriteFile(filepath.Join(path, "fixture"), []byte("synthetic config"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent"); err == nil {
		t.Fatal("agent creation raced incomplete composition")
	}
	var err error
	record, err = s.FinishRunFiles(ctx, record.ID, record.Revision)
	if err != nil || record.RunFiles.State != "ready" {
		t.Fatal(err)
	}
	if _, err := s.RunFilesPath(record.ID); err == nil {
		t.Fatal("ready directory reopened to host generators")
	}
	if _, err := s.PrepareRunFiles(ctx, record.ID, record.Revision); err == nil {
		t.Fatal("ready directory reentered preparing")
	}
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent"); err != nil {
		t.Fatal("completed composition did not authorize agent intent", err)
	}
}

func TestRunFilesCreatingCrashReconcilesOnlyEmptyOwnedDirectory(t *testing.T) {
	for _, nonempty := range []bool{false, true} {
		s, record := executionFixture(t)
		var err error
		record, err = s.mutateExecution(context.Background(), record.ID, record.Revision, func(value *Execution) (bool, error) {
			value.RunFiles.State = "creating"
			return true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.root.Mkdir(record.RunFiles.Name, 0o700); err != nil {
			t.Fatal(err)
		}
		if nonempty {
			if err := os.WriteFile(filepath.Join(s.Path(), record.RunFiles.Name, "foreign"), []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.PrepareRunFiles(context.Background(), record.ID, record.Revision)
		if nonempty && err == nil || !nonempty && (err != nil || got.RunFiles.State != "preparing") {
			t.Fatalf("creating crash nonempty=%t: state=%s err=%v", nonempty, got.RunFiles.State, err)
		}
	}
}

func TestRunFilesCleanupRequiresAgentAbsenceAndCannotFollowOutwardLinks(t *testing.T) {
	s, record := executionFixture(t)
	record, path := prepareFixtureRunFiles(t, s, record)
	outside := t.TempDir()
	canary := filepath.Join(outside, "preserved")
	if err := os.WriteFile(canary, []byte("not run-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, "mutable-copy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "mutable-copy", "changed-by-agent"), []byte("scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := fixtureEvidence(t, s)
	ctx := context.Background()
	if _, err := evidence.CleanupRunFiles(ctx, record.ID, record.Revision); err == nil {
		t.Fatal("possible live agent lost its generated files")
	}
	record = confirmFixtureConsumersGone(t, evidence, record)
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	var err error
	record, err = evidence.CleanupRunFiles(ctx, record.ID, record.Revision)
	if err != nil || record.RunFiles.State != "gone" {
		t.Fatal("keyless cleanup failed", err)
	}
	if data, err := os.ReadFile(canary); err != nil || string(data) != "not run-owned" {
		t.Fatal("cleanup followed an outward symlink", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("run-owned directory was not removed", err)
	}
	if _, err := evidence.CleanupRunFiles(ctx, record.ID, record.Revision); err != nil {
		t.Fatal("idempotent cleanup failed", err)
	}
}

func TestRunFilesReplacedRootIsRetainedAndNeverRebound(t *testing.T) {
	s, record := executionFixture(t)
	record, path := prepareFixtureRunFiles(t, s, record)
	original := path + "-original"
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunFilesPath(record.ID); err == nil {
		t.Fatal("replacement directory received generator authority")
	}
	if _, err := s.PrepareRunFiles(context.Background(), record.ID, record.Revision); err == nil {
		t.Fatal("replacement directory was rebound")
	}
	evidence := fixtureEvidence(t, s)
	record = confirmFixtureConsumersGone(t, evidence, record)
	if _, err := evidence.CleanupRunFiles(context.Background(), record.ID, record.Revision); err == nil {
		t.Fatal("cleanup removed a replacement root")
	}
	for _, preserved := range []string{path, original} {
		if _, err := os.Stat(preserved); err != nil {
			t.Fatal("uncertain root not preserved", err)
		}
	}
}

func TestRunFilesCleanupRetryKeepsIrreversibleCustody(t *testing.T) {
	s, record := executionFixture(t)
	record, path := prepareFixtureRunFiles(t, s, record)
	if err := os.WriteFile(filepath.Join(path, "fixture"), []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := fixtureEvidence(t, s)
	record = confirmFixtureConsumersGone(t, evidence, record)
	fault := errors.New("injected cleaning-intent fsync failure")
	evidence.files.syncDir = func(*os.File) error { return fault }
	if _, err := evidence.CleanupRunFiles(context.Background(), record.ID, record.Revision); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uncertain cleaning publication deleted files", err)
	}
	current, err := s.Execution(record.ID)
	if err != nil || current.RunFiles.State != "cleaning" {
		t.Fatal("missing irreversible cleaning intent", err)
	}
	if _, err := s.RunFilesPath(record.ID); err == nil {
		t.Fatal("cleaning root reopened to generators")
	}
	evidence.files.syncDir = nil
	if _, err := evidence.CleanupRunFiles(context.Background(), record.ID, current.Revision); err != nil {
		t.Fatal("cleanup retry failed", err)
	}
}

func TestRunFilesPlannedCleanupNeverAdoptsOccupiedDirectory(t *testing.T) {
	s, record := executionFixture(t)
	path := filepath.Join(s.Path(), record.RunFiles.Name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	evidence := fixtureEvidence(t, s)
	record = confirmFixtureConsumersGone(t, evidence, record)
	if _, err := evidence.CleanupRunFiles(context.Background(), record.ID, record.Revision); err == nil {
		t.Fatal("cleanup adopted a directory without a creating intention")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("foreign empty directory was removed", err)
	}
}

func TestRunFilesReadyMustRetainIdentityBeforeCreateAndStart(t *testing.T) {
	for _, phase := range []string{"create", "start"} {
		for _, replacement := range []bool{false, true} {
			s, record := executionFixture(t)
			record = prepareFixtureLaunch(t, s, record)
			record, path := prepareFixtureRunFiles(t, s, record)
			ctx := context.Background()
			var err error
			record, err = s.FinishRunFiles(ctx, record.ID, record.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "start" {
				record, err = s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent")
				if err != nil {
					t.Fatal(err)
				}
				record, err = s.RecordResourceCreated(ctx, record.ID, record.Revision, "agent", strings.Repeat("b", 64))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(path, path+"-original"); err != nil {
				t.Fatal(err)
			}
			if replacement {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "create" {
				_, err = s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent")
			} else {
				_, err = s.BeginResourceStart(ctx, record.ID, record.Revision, "agent")
			}
			if err == nil {
				t.Fatalf("%s accepted missing/replaced ready directory (replacement=%t)", phase, replacement)
			}
		}
	}
}
