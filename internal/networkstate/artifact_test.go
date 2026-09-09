package networkstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

var fixtureLaunchData = []byte(`{"fixture":"immutable helper configuration"}`)

func prepareFixtureLaunch(t *testing.T, s *Store, record Execution) Execution {
	t.Helper()
	ready, err := s.PrepareArtifacts(context.Background(), record.ID, record.Revision, fixtureLaunchData)
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

func TestArtifactsPrepareOneOwnedDirectoryBeforeAnyContainer(t *testing.T) {
	s, record := executionFixture(t)
	ctx := context.Background()
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "controller"); err == nil {
		t.Fatal("container created before its launch artifacts were prepared")
	}
	record = prepareFixtureLaunch(t, s, record)
	if record.Artifact.State != "prepared" || record.Artifact.Inode == 0 || record.Revision != 2 {
		t.Fatal("artifact directory lacks private identity custody")
	}
	config, err := s.LaunchConfigPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.RunFilesPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.Path(), record.Artifact.Name)
	if config != filepath.Join(root, "launch.json") || files != filepath.Join(root, "files") {
		t.Fatal("artifact paths escaped their private directory", config, files)
	}
	info, err := os.Lstat(config)
	if err != nil || info.Mode() != 0o444 || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Fatal("launch configuration is not a single immutable link", err)
	}
	if dir, err := os.Lstat(root); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatal("artifact directory is not owner-private", err)
	}
	if dir, err := os.Lstat(files); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatal("generated-file directory is not owner-private", err)
	}
	// A second call with the same bytes reconfirms durability; different bytes
	// can never rebind an execution already bound to its configuration.
	if _, err := s.PrepareArtifacts(context.Background(), record.ID, record.Revision, fixtureLaunchData); err != nil {
		t.Fatal("idempotent reconfirmation failed", err)
	}
	if _, err := s.PrepareArtifacts(context.Background(), record.ID, record.Revision, []byte(`{"other":true}`)); err == nil {
		t.Fatal("launch configuration was rebound")
	}
	if _, err := s.PrepareArtifacts(context.Background(), record.ID, record.Revision, nil); err == nil {
		t.Fatal("empty launch configuration accepted")
	}
}

func TestArtifactsPublicationFaultRequiresRereadAndDurability(t *testing.T) {
	s, record := executionFixture(t)
	failure := errors.New("fixture directory-sync failure")
	s.syncDir = func(*os.File) error { return failure }
	if _, err := s.PrepareArtifacts(context.Background(), record.ID, record.Revision, fixtureLaunchData); !errors.Is(err, failure) {
		t.Fatal("ambiguous publication returned usable artifacts", err)
	}
	if _, err := s.LaunchConfigPath(record.ID); err == nil {
		t.Fatal("visible bytes bypassed failed durability")
	}
	s.syncDir = nil
	current, err := s.Execution(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareArtifacts(context.Background(), current.ID, current.Revision, fixtureLaunchData); err != nil {
		t.Fatal("retry could not confirm publication", err)
	}
	if _, err := s.LaunchConfigPath(record.ID); err != nil {
		t.Fatal("confirmed artifacts stayed unreadable", err)
	}
}

func TestArtifactsRefuseAnOccupiedOrTamperedDestination(t *testing.T) {
	for _, kind := range []string{"occupied", "symlink", "replaced-content", "public-directory"} {
		t.Run(kind, func(t *testing.T) {
			s, record := executionFixture(t)
			root := filepath.Join(s.Path(), record.Artifact.Name)
			switch kind {
			case "occupied":
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "squatter"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), root); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "occupied" || kind == "symlink" {
				if _, err := s.PrepareArtifacts(context.Background(), record.ID, record.Revision, fixtureLaunchData); err == nil {
					t.Fatal("adopted an unproven artifact destination")
				}
				return
			}
			record = prepareFixtureLaunch(t, s, record)
			switch kind {
			case "replaced-content":
				config := filepath.Join(root, "launch.json")
				if err := os.Remove(config); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config, []byte(`{"fixture":"replaced!!!!!!!!!!!!!!!!!!!!!!"}`), 0o444); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.LaunchConfigPath(record.ID); err == nil {
				t.Fatal("tampered artifacts were accepted", kind)
			}
			if _, err := s.BeginResourceCreation(context.Background(), record.ID, record.Revision, "controller"); err == nil {
				t.Fatal("tampered artifacts authorized runtime work", kind)
			}
		})
	}
}

func TestArtifactCleanupRequiresGoneConsumersAndSurvivesKeyLoss(t *testing.T) {
	s, record := executionFixture(t)
	ctx := context.Background()
	record = prepareFixtureLaunch(t, s, record)
	files, err := s.RunFilesPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A workload can leave a subtree of its own behind; cleanup owns all of it.
	if err := os.MkdirAll(filepath.Join(files, "agent", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "agent", "nested", "mcp.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := fixtureEvidence(t, s)
	if _, err := evidence.CleanupArtifacts(ctx, record.ID, record.Revision); err == nil {
		t.Fatal("cleanup ran while container consumers might exist")
	}
	for _, role := range []string{"controller", "guard", "agent"} {
		if record, err = s.BeginResourceCreation(ctx, record.ID, record.Revision, role); err != nil {
			t.Fatal(err)
		}
		if record, err = s.RecordResourceCreated(ctx, record.ID, record.Revision, role, strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
	}
	record = confirmFixtureConsumersGone(t, evidence, record)
	if err := os.Remove(filepath.Join(s.Path(), "owner.key")); err != nil {
		t.Fatal(err)
	}
	record, err = evidence.CleanupArtifacts(ctx, record.ID, record.Revision)
	if err != nil || record.Artifact.State != "gone" {
		t.Fatal("key loss blocked exact-owned cleanup", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), record.Artifact.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("artifact subtree survived cleanup", err)
	}
	// Repeating cleanup is inert: the irreversible conclusion never reopens.
	if record, err = evidence.CleanupArtifacts(ctx, record.ID, record.Revision); err != nil || record.Artifact.State != "gone" {
		t.Fatal("cleanup retry lost custody", err)
	}
	if _, err := s.PrepareArtifacts(ctx, record.ID, record.Revision, fixtureLaunchData); err == nil {
		t.Fatal("cleaned generation was reopened")
	}
}

func TestArtifactCleanupRefusesAReplacedDirectory(t *testing.T) {
	s, record := executionFixture(t)
	ctx := context.Background()
	record = prepareFixtureLaunch(t, s, record)
	evidence := fixtureEvidence(t, s)
	root := filepath.Join(s.Path(), record.Artifact.Name)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.CleanupArtifacts(ctx, record.ID, record.Revision); err == nil {
		t.Fatal("cleanup deleted a directory it never created")
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatal("replacement directory was removed", err)
	}
}
