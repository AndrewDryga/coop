package networkstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func rule(name string) egress.Rule {
	return egress.Rule{To: egress.Destination{Domain: name}, Protocol: "tls", Ports: []int{443}}
}
func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "network"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestExistingOwnerKeyStillRequiresDurablePublication(t *testing.T) {
	s := openStore(t)
	s.key = nil
	failure := errors.New("synthetic directory sync failure")
	s.syncDir = func(*os.File) error { return failure }
	if err := s.loadKey(true); !errors.Is(err, failure) || len(s.key) != 0 {
		t.Fatal("failed existing-key confirmation returned authority", err)
	}
	s.syncDir = nil
	if err := s.loadKey(true); err != nil || len(s.key) != 32 {
		t.Fatal("key confirmation did not recover", err)
	}
}

func TestApprovalRequestsNeverGrantAndPostureSurvivesRemoval(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("a.example.com"), rule("b.example.com")}
	if _, err := s.CheckRequests(project, requests, nil); err == nil {
		t.Fatal("unapproved requests gained authority")
	}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	for _, current := range [][]egress.Rule{requests, requests[:1], nil, requests} {
		approval, err := s.CheckRequests(project, current, nil)
		if err != nil || approval == nil || approval.Posture != egress.Filtered {
			t.Fatalf("narrow/reintroduce lost approved posture: %#v %v", approval, err)
		}
	}
	if _, err := s.CheckRequests(project, append(requests, rule("extra.example.com")), nil); err == nil {
		t.Fatal("new destination reused approval")
	}
	if _, err := s.CheckRequests(t.TempDir(), requests, nil); err == nil {
		t.Fatal("another project reused approval")
	}
	if err := approve(s, project, egress.Open, requests, nil); err == nil {
		t.Fatal("open posture ignored rules")
	}
	if err := approve(s, project, egress.Open, nil, nil); err != nil {
		t.Fatal(err)
	}
	approval, err := s.CheckRequests(project, nil, nil)
	if err != nil || approval.Posture != egress.Open {
		t.Fatal("explicit host posture reset failed")
	}
	if approval, err := s.CheckRequests(project, requests, nil); err == nil || approval != nil {
		t.Fatal("refused requests under open posture returned usable approval")
	}
}

func TestApprovalPermitsNarrowerRulesWithoutWidening(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	approved := rule("*.example.com")
	approved.Ports = []int{443, 8443}
	if err := approve(s, project, egress.Filtered, []egress.Rule{approved}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckRequests(project, []egress.Rule{rule("a.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"example.com", "a.b.example.com", "a.example.com.attacker.net", "*.a.example.com"} {
		if _, err := s.CheckRequests(project, []egress.Rule{rule(name)}, nil); err == nil {
			t.Fatalf("wildcard approval widened to %s", name)
		}
	}
	if _, err := s.CheckRequests(project, []egress.Rule{rule("*.example.com")}, nil); err != nil {
		t.Fatal("same wildcard narrowed to TLS443 was rejected:", err)
	}
}

func TestCaptureChecksRequestsAndBindsProjectWithoutExportingKey(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("api.example.com")}
	mode := egress.Filtered
	if got, err := s.Admit(project, Admission{InvocationMode: &mode, Requests: requests}); err == nil || got.Fingerprint != "" {
		t.Fatal("capture compiled an unapproved request")
	}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Admit(project, Admission{InvocationMode: &mode, Requests: requests})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSnapshot(t.TempDir(), snapshot.Fingerprint); err == nil {
		t.Fatal("reused another project's captured authority")
	}
	if err := approve(s, project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	old, err := s.LoadSnapshot(project, snapshot.Fingerprint)
	if err != nil || !old.Domain("api.example.com", 443).Allowed {
		t.Fatal("narrowing new-run posture rewrote an existing session capture")
	}
	foreign := openStore(t)
	foreignID, err := foreign.projectID(project)
	if err != nil {
		t.Fatal(err)
	}
	foreignSnapshot, err := egress.Compile(foreignID, egress.Filtered, nil, nil, false, foreign.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.saveSnapshot(foreignSnapshot); err == nil {
		t.Fatal("accepted capture signed by another owner key")
	}
}

func TestMountIdentityRejectsCaseAliasesWhereFilesystemSupportsThem(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "MountSource")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "mountsource")
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		t.Skip("case-sensitive filesystem; Linux bind alias is in runtime qualification")
	}
	if s, err := Open(filepath.Join(project, "network"), []string{alias}); err == nil {
		_ = s.Close()
		t.Fatal("case alias exposed the private authority root")
	}
}

func TestMountIdentityRejectsCaseAliasDescendants(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "Network")
	s, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	alias := filepath.Join(parent, "network")
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		t.Skip("case-sensitive filesystem")
	}
	for _, path := range []string{filepath.Join(alias, "owner.key"), filepath.Join(alias, "future", "child")} {
		if err := CheckPathExposure(root, []string{path}); err == nil {
			t.Fatal("case-aliased authority descendant accepted", path)
		}
		if err := s.CheckExposure([]string{path}); err == nil {
			t.Fatal("final exposure accepted alias", path)
		}
	}
}

func TestFirstKeyPublicationRechecksProspectiveCaseAlias(t *testing.T) {
	parent := t.TempDir()
	probe := filepath.Join(parent, "CaseProbe")
	if err := os.Mkdir(probe, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "caseprobe")); os.IsNotExist(err) {
		t.Skip("case-sensitive filesystem")
	}
	root := filepath.Join(parent, "Network")
	if s, err := Open(root, []string{filepath.Join(parent, "network", "future")}); err == nil {
		_ = s.Close()
		t.Fatal("first key published beneath prospective case-alias mount")
	}
	// Creating an empty directory is harmless; the first authority-bearing
	// publication must wait until the new inode passes exposure checks.
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("refused first publication left private authority", entries)
	}
}

func TestBundleMaintenanceAndFrozenSnapshots(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	bundle := egress.Bundle{Provider: "model", Client: egress.ClientCLI, Version: "1", Backend: "direct", AuthMode: "key", Core: []egress.Rule{rule("api.example.com")}, Features: map[string][]egress.Rule{"cloud-mcp": {rule("mcp.example.com")}}}
	requests := []egress.Rule{{To: egress.Destination{Provider: "model", Features: []string{"cloud-mcp"}}}}
	if err := approve(s, project, egress.Filtered, requests, []egress.Bundle{bundle}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Admit(project, Admission{Bundles: []egress.Bundle{bundle}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.saveSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.saveSnapshot(snapshot); err != nil {
		t.Fatal("idempotent capture:", err)
	}
	bundle.Core = append(bundle.Core, rule("new.example.com"))
	if err := s.checkBundles([]egress.Bundle{bundle}); err == nil {
		t.Fatal("same-version content drift accepted")
	}
	bundle.Version = "2"
	if _, err := s.CheckRequests(project, requests, []egress.Bundle{bundle}); err != nil {
		t.Fatal("new core release unnecessarily required feature reapproval:", err)
	}
	old, err := s.LoadSnapshot(project, snapshot.Fingerprint)
	if err != nil || old.Domain("new.example.com", 443).Allowed {
		t.Fatal("captured authority re-expanded")
	}
	bundle.Version = "3"
	bundle.Features["cloud-mcp"] = append(bundle.Features["cloud-mcp"], rule("new-mcp.example.com"))
	if _, err := s.CheckRequests(project, requests, []egress.Bundle{bundle}); err == nil {
		t.Fatal("new optional access did not require review")
	}
}

func TestPrivateStoreDeniesMountOverlapAndSpecialFiles(t *testing.T) {
	project := t.TempDir()
	if s, err := Open(filepath.Join(project, "network"), []string{project}); err == nil {
		_ = s.Close()
		t.Fatal("authority allowed inside agent mount")
	}
	link := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(filepath.Join(project, "network"), []string{link}); err == nil {
		_ = s.Close()
		t.Fatal("symlinked mount overlap ignored")
	}
	root := filepath.Join(t.TempDir(), "public")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(root, nil); err == nil {
		_ = s.Close()
		t.Fatal("nonprivate state accepted")
	}
	s := openStore(t)
	if duplicate, err := Open(s.Path(), []string{filepath.Join(s.Path(), "owner.key")}); err == nil {
		_ = duplicate.Close()
		t.Fatal("individual authority record allowed as an agent mount")
	}
	if err := os.Symlink("owner.key", filepath.Join(s.Path(), "key-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read("key-link", 32); err == nil {
		t.Fatal("followed a record symlink")
	}
	if err := syscall.Mkfifo(filepath.Join(s.Path(), "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read("fifo", 32); err == nil {
		t.Fatal("accepted FIFO as a record")
	}
	if err := os.WriteFile(filepath.Join(s.Path(), "large"), bytes.Repeat([]byte{1}, 33), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read("large", 32); err == nil {
		t.Fatal("record byte bound ignored")
	}
	if _, err := s.LoadSnapshot(project, "../owner.key"); err == nil {
		t.Fatal("snapshot traversal accepted")
	}
}

func TestConcurrentInitializationAndKeyLoss(t *testing.T) {
	root := filepath.Join(t.TempDir(), "network")
	var wg sync.WaitGroup
	keys := make(chan []byte, 16)
	failures := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			s, err := Open(root, nil)
			if err != nil {
				failures <- err
				return
			}
			keys <- bytes.Clone(s.key)
			_ = s.Close()
		})
	}
	wg.Wait()
	close(keys)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var first []byte
	for key := range keys {
		if first == nil {
			first = key
		}
		if !bytes.Equal(first, key) {
			t.Fatal("concurrent callers used different owner keys")
		}
	}
	s, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := approve(s, t.TempDir(), egress.Filtered, nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := os.Remove(filepath.Join(root, "owner.key")); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(root, nil); err == nil {
		_ = s.Close()
		t.Fatal("lost owner key regenerated over durable posture")
	} else if !strings.Contains(err.Error(), "key missing") {
		t.Fatal(err)
	}
}
