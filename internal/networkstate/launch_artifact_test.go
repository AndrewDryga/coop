package networkstate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

var fixtureLaunchData = []byte(`{"fixture":"immutable helper configuration"}`)

func prepareFixtureLaunch(t *testing.T, s *Store, record Execution) Execution {
	t.Helper()
	ready, err := s.PrepareLaunchConfig(context.Background(), record.ID, record.Revision, fixtureLaunchData)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func fixtureEvidence(t *testing.T, s *Store) *Evidence {
	t.Helper()
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = evidence.Close() })
	return evidence
}

func confirmFixtureConsumersGone(t *testing.T, evidence *Evidence, record Execution) Execution {
	t.Helper()
	for _, resource := range record.Resources {
		if resource.Kind != "container" {
			continue
		}
		var err error
		record, err = evidence.ConfirmResourceGone(context.Background(), record.ID, record.Revision, record.DaemonID, resource.Role, resource.Name, resource.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func TestLaunchArtifactReadyImmutableAndSeparateFromAuthorityReads(t *testing.T) {
	s, record := executionFixture(t)
	ctx := context.Background()
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "controller"); err == nil {
		t.Fatal("container created before helper configuration was ready")
	}
	record = prepareFixtureLaunch(t, s, record)
	if record.LaunchConfig.State != "ready" || record.Revision != 3 {
		t.Fatal("missing intent and ready publications")
	}
	path, err := s.LaunchConfigPath(record.ID)
	if err != nil || path != filepath.Join(s.Path(), record.LaunchConfig.Name) {
		t.Fatal("launch path did not bind the ready artifact", err)
	}
	if _, err := s.read(record.LaunchConfig.Name, maxPrivateRecordBytes); err == nil {
		t.Fatal("helper-readable artifact weakened generic private authority reads")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != 0o444 {
		t.Fatal("helper file is not immutable/readable", err)
	}
	before := record.Revision
	record = prepareFixtureLaunch(t, s, record)
	if record.Revision != before {
		t.Fatal("identical prepare rewrote custody")
	}
	if _, err := s.PrepareLaunchConfig(ctx, record.ID, record.Revision, []byte(`{"different":true}`)); err == nil {
		t.Fatal("configuration was rebound")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareLaunchConfig(ctx, record.ID, record.Revision, fixtureLaunchData); err == nil {
		t.Fatal("missing ready artifact was silently regenerated")
	}
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "controller"); err == nil {
		t.Fatal("container creation used missing ready configuration")
	}
}

func TestLaunchArtifactPublicationFaultsRequireRereadAndDurability(t *testing.T) {
	for _, phase := range []string{"intent", "file", "ready"} {
		t.Run(phase, func(t *testing.T) {
			s, record := executionFixture(t)
			fault := errors.New("injected directory durability failure")
			calls := 0
			wanted := map[string]int{"intent": 1, "file": 2, "ready": 3}[phase]
			s.syncDir = func(dir *os.File) error {
				calls++
				if calls == wanted {
					return fault
				}
				return dir.Sync()
			}
			got, err := s.PrepareLaunchConfig(context.Background(), record.ID, record.Revision, fixtureLaunchData)
			if !errors.Is(err, fault) || got.ID != record.ID {
				t.Fatal("ambiguous publication lost identity", err)
			}
			s.syncDir = nil
			persisted, err := s.Execution(record.ID)
			if err != nil || persisted.LaunchConfig.Digest != launchDigest(fixtureLaunchData) {
				t.Fatal("fault erased intended content custody", err)
			}
			if _, err := s.PrepareLaunchConfig(context.Background(), record.ID, record.Revision, fixtureLaunchData); !errors.Is(err, ErrExecutionConflict) {
				t.Fatal("blind retry reused stale revision", err)
			}
			ready := prepareFixtureLaunch(t, s, persisted)
			if ready.LaunchConfig.State != "ready" {
				t.Fatal("exact retry failed to reconcile intent")
			}
			if _, err := s.LaunchConfigPath(record.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLaunchArtifactRejectsReplacementAndPreservesForeignFiles(t *testing.T) {
	for _, shape := range []string{"symlink", "fifo", "content", "mode", "hardlink"} {
		t.Run(shape, func(t *testing.T) {
			s, record := executionFixture(t)
			record = prepareFixtureLaunch(t, s, record)
			evidence := fixtureEvidence(t, s)
			record = confirmFixtureConsumersGone(t, evidence, record)
			path := filepath.Join(s.Path(), record.LaunchConfig.Name)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			var err error
			switch shape {
			case "symlink":
				err = os.Symlink(filepath.Join(s.Path(), "owner.key"), path)
			case "fifo":
				err = syscall.Mkfifo(path, 0o444)
			case "content":
				err = os.WriteFile(path, []byte(strings.Repeat("x", len(fixtureLaunchData))), 0o444)
			case "mode":
				err = os.WriteFile(path, fixtureLaunchData, 0o600)
			case "hardlink":
				source := filepath.Join(s.Path(), "fixture-other")
				err = os.WriteFile(source, fixtureLaunchData, 0o444)
				if err == nil {
					err = os.Link(source, path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.LaunchConfigPath(record.ID); err == nil {
				t.Fatal("launch used a substituted artifact")
			}
			if _, err := evidence.RemoveLaunchConfig(context.Background(), record.ID, record.Revision); err == nil {
				t.Fatal("cleanup removed a substituted artifact")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("foreign artifact was not preserved", err)
			}
		})
	}
}

func TestLaunchArtifactCleanupRequiresGoneConsumersAndSurvivesKeyLoss(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	ctx := context.Background()
	evidence := fixtureEvidence(t, s)
	if _, err := evidence.RemoveLaunchConfig(ctx, record.ID, record.Revision); err == nil {
		t.Fatal("cleanup raced possible container consumers")
	}
	var err error
	record, err = s.BeginResourceCreation(ctx, record.ID, record.Revision, "controller")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.RemoveLaunchConfig(ctx, record.ID, record.Revision); err == nil {
		t.Fatal("uncertain creation permitted config cleanup")
	}
	record, err = s.SealExecution(ctx, record.ID, record.Revision, "launch_failed")
	if err != nil {
		t.Fatal(err)
	}
	receipt := *record.Receipt
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	record = confirmFixtureConsumersGone(t, evidence, record)
	record, err = evidence.RemoveLaunchConfig(ctx, record.ID, record.Revision)
	if err != nil || record.LaunchConfig.State != "gone" || !reflect.DeepEqual(receipt, *record.Receipt) {
		t.Fatal("keyless cleanup failed or rewrote receipt", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), record.LaunchConfig.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned config was not removed", err)
	}
	before := record.Revision
	record, err = evidence.RemoveLaunchConfig(ctx, record.ID, record.Revision)
	if err != nil || record.Revision != before {
		t.Fatal("cleanup replay changed receipt/custody", err)
	}
	if _, err := s.PrepareLaunchConfig(ctx, record.ID, record.Revision, fixtureLaunchData); err == nil {
		t.Fatal("cleaned generation reopened")
	}
}

func TestLaunchArtifactCleanupFsyncFaultRetainsDurableReconciliation(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	evidence := fixtureEvidence(t, s)
	record = confirmFixtureConsumersGone(t, evidence, record)
	fault := errors.New("injected cleanup sync failure")
	evidence.files.syncDir = func(*os.File) error { return fault }
	if _, err := evidence.RemoveLaunchConfig(context.Background(), record.ID, record.Revision); !errors.Is(err, fault) {
		t.Fatal("cleanup hid durability error", err)
	}
	persisted, err := evidence.Execution(record.ID)
	if err != nil || persisted.LaunchConfig.State != "gone" {
		t.Fatal("ambiguous cleanup erased identity", err)
	}
	if _, err := evidence.RemoveLaunchConfig(context.Background(), record.ID, persisted.Revision); !errors.Is(err, fault) {
		t.Fatal("already-gone replay skipped durability", err)
	}
	evidence.files.syncDir = nil
	if _, err := evidence.RemoveLaunchConfig(context.Background(), record.ID, persisted.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchArtifactAbruptExitAfterPublicationKeepsOneOwnedLink(t *testing.T) {
	if root := os.Getenv("COOP_TEST_NETWORK_ARTIFACT_CRASH_ROOT"); root != "" {
		s, err := Open(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		inputsID, err := s.RecordInputs(LaunchInputs{})
		if err != nil {
			t.Fatal(err)
		}
		record, err := executionTrial(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: os.Getenv("COOP_TEST_NETWORK_ARTIFACT_PROJECT"), InputsID: inputsID,
			PolicyFingerprint: os.Getenv("COOP_TEST_NETWORK_ARTIFACT_POLICY"), Runtime: "docker", DaemonID: "fixture-daemon",
			Endpoint: "unix:///fixture.sock", GatewayImage: "sha256:" + strings.Repeat("a", 64), ClientImage: "sha256:" + strings.Repeat("b", 64)}, "enforcement", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stdout.WriteString(record.ID + "\n"); err != nil {
			t.Fatal(err)
		}
		calls := 0
		s.syncDir = func(dir *os.File) error {
			if err := dir.Sync(); err != nil {
				return err
			}
			calls++
			if calls == 2 {
				os.Exit(43) // abrupt death: deliberately bypass every deferred unlink
			}
			return nil
		}
		_, _ = s.PrepareLaunchConfig(context.Background(), record.ID, record.Revision, fixtureLaunchData)
		t.Fatal("fixture failed to reach the durable file publication barrier")
	}
	s, record := executionFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLaunchArtifactAbruptExitAfterPublicationKeepsOneOwnedLink$")
	cmd.Env = append(os.Environ(), "COOP_TEST_NETWORK_ARTIFACT_CRASH_ROOT="+s.Path(),
		"COOP_TEST_NETWORK_ARTIFACT_PROJECT="+record.Project, "COOP_TEST_NETWORK_ARTIFACT_POLICY="+record.Snapshot.PolicyFingerprint)
	output, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 43 {
		t.Fatalf("fixture did not exit at publication barrier: %v", err)
	}
	id := strings.TrimSpace(string(output))
	interrupted, err := s.Execution(id)
	if err != nil || interrupted.LaunchConfig.State != "creating" {
		t.Fatal("crash did not retain the creating intent", err)
	}
	if _, _, err := s.readLaunchArtifact(interrupted.LaunchConfig); err != nil {
		t.Fatal("legitimate abrupt publication looked like foreign hardlink custody", err)
	}
	evidence := fixtureEvidence(t, s)
	interrupted = confirmFixtureConsumersGone(t, evidence, interrupted)
	if _, err := evidence.RemoveLaunchConfig(context.Background(), id, interrupted.Revision); err != nil {
		t.Fatal("keyless recovery could not clean abrupt publication", err)
	}
}

func TestLaunchArtifactExclusivePublicationNeverReplacesOccupiedDestination(t *testing.T) {
	s := openStore(t)
	if err := s.publishMode("fixture-launch.json", []byte("original"), false, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := s.publishMode("fixture-launch.json", []byte("replacement"), false, 0o444); !errors.Is(err, os.ErrExist) {
		t.Fatal("exclusive publication replaced occupied destination", err)
	}
	data, err := os.ReadFile(filepath.Join(s.Path(), "fixture-launch.json"))
	if err != nil || string(data) != "original" {
		t.Fatal("collision damaged existing artifact", err)
	}
}
