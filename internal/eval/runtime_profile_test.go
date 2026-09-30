package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func runtimeProfileSuite(t *testing.T) *Suite {
	t.Helper()
	s := stagedAgentSuite(t)
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "Dockerfile"), []byte("FROM trusted-base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Cases[0].Runtime = &CaseRuntime{Profile: profile, Workdir: "/app", AgentTimeout: Duration(time.Minute), VerifierTimeout: Duration(time.Minute)}
	return s
}

func TestRuntimeProfileYAMLAndRunMetadata(t *testing.T) {
	source := runtimeProfileSuite(t)
	body, err := yaml.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.Path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := StageSuite(context.Background(), t.TempDir(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	r := staged.Cases[0].Runtime
	r.ImageID, r.Platform = "sha256:"+strings.Repeat("a", 64), "linux/arm64"
	rec := NewRunRecord(&Plan{Suite: staged, Repeat: 1}, []FrozenConfig{{Kind: ConfigTarget, Label: "codex"}}, time.Now())
	store, err := CreateRun(t.TempDir(), rec)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := LoadRun(filepath.Dir(store.Dir()), store.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(retained.Runtimes) != 1 {
		t.Fatal("runtime missing from manifest")
	}
	m := retained.Runtimes[0]
	if m.ProfileDigest != string(r.ProfileDigest) || m.ImageID != r.ImageID || m.Platform != r.Platform ||
		m.AgentTimeoutMS != 60000 || m.VerifierTimeoutMS != 60000 || m.Protocol != ProfileProtocol ||
		m.CPUs != 1 || m.MemoryBytes != 2<<30 || m.PIDs != 128 || m.DeclaredStorageBytes != 10<<30 ||
		m.StorageEnforced || m.StorageMeasured || m.CandidateNetwork != "provider-only-filtered" || m.VerifierNetwork != "none" {
		t.Fatalf("wrong frozen runtime: %+v", m)
	}
	if err := store.Seal(RunSummary{}); err != nil {
		t.Fatal(err)
	}
	other := rec
	other.ID, other.Workload = "different-workload", "different"
	next, err := CreateRun(filepath.Dir(store.Dir()), other)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Seal(RunSummary{}); err != nil {
		t.Fatal(err)
	}
	cmp, err := Compare(filepath.Dir(store.Dir()), store.ID(), next.ID())
	if err != nil || cmp.Mismatch == "" || len(cmp.Base.Runtimes) != 1 || len(cmp.New.Runtimes) != 1 {
		t.Fatalf("runtime disclosure lost on mismatch: %+v, %v", cmp, err)
	}
	var old RunRecord
	if err := json.Unmarshal([]byte(`{"schema":1,"cases":["ordinary"]}`), &old); err != nil || len(old.Runtimes) != 0 {
		t.Fatal("old manifest no longer readable")
	}
	body = []byte(strings.Replace(string(body), "workdir: /app", "workdir: /app\n        image_id: forged", 1))
	if err := os.WriteFile(source.Path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(source.Path); err == nil || !strings.Contains(err.Error(), "field image_id not found") {
		t.Fatalf("authored resolved image was not refused by name: %v", err)
	}
}

func TestRuntimeProfileRejectsReplacementWithIdenticalBytes(t *testing.T) {
	source := runtimeProfileSuite(t)
	staged, err := StageSuite(context.Background(), t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	r := staged.Cases[0].Runtime
	if err := os.Rename(r.Profile, r.Profile+"-original"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(r.Profile + "-original") })
	if err := os.Mkdir(r.Profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Profile, "Dockerfile"), []byte("FROM trusted-base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyProfile(context.Background()); err == nil {
		t.Fatal("identical bytes replaced source authority")
	}
}

func TestRuntimeProfileSharesStagingBudgetWithOrdinaryInputs(t *testing.T) {
	source := runtimeProfileSuite(t)
	input, err := os.OpenRoot(filepath.Join(source.Dir, "files"))
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	// The two-byte ordinary input and profile each fit alone, but together exceed by one byte.
	remaining, entries := int64(len("FROM trusted-base\n")+2-1), stageMaxEntries
	if _, err := stageOpenedTree(context.Background(), input, filepath.Join(t.TempDir(), "input"), &remaining, &entries); err != nil {
		t.Fatal(err)
	}
	staged := *source
	staged.Dir, staged.Cases = t.TempDir(), append([]Case(nil), source.Cases...)
	if err := stageRuntimeProfiles(context.Background(), t.TempDir(), source, &staged, &remaining, &entries); err == nil {
		t.Fatal("profile ignored bytes already spent staging inputs")
	}
}

func TestRuntimeProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Suite)
		want string
	}{
		{"relative", func(s *Suite) { s.Cases[0].Runtime.Profile = "../profile" }, "absolute external"},
		{"workdir", func(s *Suite) { s.Cases[0].Runtime.Workdir = "/workspace" }, "/app"},
		{"missing phase", func(s *Suite) { s.Cases[0].Runtime.AgentTimeout = 0 }, "positive agent_timeout"},
		{"oversized phases", func(s *Suite) { s.Cases[0].Runtime.VerifierTimeout = s.Cases[0].Timeout }, "fit within"},
		{"suite overlap", func(s *Suite) { s.Cases[0].Runtime.Profile = s.Dir }, "overlaps the suite"},
		{"loop", func(s *Suite) { s.Runner = RunnerLoop; s.LoopConfig = "./files/input.txt" }, "not loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := runtimeProfileSuite(t)
			tc.edit(s)
			if err := s.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestRuntimeProfileRetentionIdentityAndDrift(t *testing.T) {
	source := runtimeProfileSuite(t)
	second := source.Cases[0]
	second.ID = "second"
	secondRuntime := *second.Runtime
	secondRuntime.AgentTimeout = Duration(2 * time.Minute)
	second.Runtime = &secondRuntime
	source.Cases = append(source.Cases, second)
	staged, err := StageSuite(context.Background(), t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	r := staged.Cases[0].Runtime
	if r == source.Cases[0].Runtime || source.Cases[0].Runtime.ProfileDigest != "" {
		t.Fatal("staging mutated source runtime")
	}
	if r.ProfileDigest == "" || r.RetainedProfile != staged.Cases[1].Runtime.RetainedProfile || staged.Cases[1].Runtime.AgentTimeout != secondRuntime.AgentTimeout {
		t.Fatal("shared profile was not deduplicated or replaced per-case budgets")
	}
	profiles, err := os.ReadDir(filepath.Join(staged.Dir, "profiles"))
	if err != nil || len(profiles) != 1 {
		t.Fatal("shared profile was copied more than once")
	}
	if err := r.VerifyProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := WorkloadFingerprint(staged)
	r.ImageID, r.Platform = "sha256:resolved", "linux/arm64"
	if WorkloadFingerprint(staged) == original {
		t.Fatal("resolved image/platform did not enter workload identity")
	}
	withImage := WorkloadFingerprint(staged)
	r.Profile += "-different-host-location"
	if WorkloadFingerprint(staged) != withImage {
		t.Fatal("source pathname entered semantic workload identity")
	}
	r.Profile = source.Cases[0].Runtime.Profile
	if err := os.WriteFile(filepath.Join(r.Profile, "Dockerfile"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyProfile(context.Background()); err == nil {
		t.Fatal("changed original profile was accepted")
	}
	retained, err := os.ReadFile(filepath.Join(staged.Dir, r.RetainedProfile, "Dockerfile"))
	if err != nil || string(retained) != "FROM trusted-base\n" {
		t.Fatal("retained profile changed with original")
	}
	if err := os.RemoveAll(r.Profile); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyProfile(context.Background()); err == nil {
		t.Fatal("retained copy authorized an absent original")
	}
}

func TestRuntimeProfileRefusesDirtyOrOverlappingTrees(t *testing.T) {
	for _, kind := range []string{"git", "nested git", "symlink", "oversized", "eval state"} {
		t.Run(kind, func(t *testing.T) {
			s := runtimeProfileSuite(t)
			profile := s.Cases[0].Runtime.Profile
			state := t.TempDir()
			var err error
			switch kind {
			case "git":
				err = os.Mkdir(filepath.Join(profile, ".git"), 0o700)
			case "nested git":
				err = os.MkdirAll(filepath.Join(profile, "nested", ".git"), 0o700)
			case "symlink":
				err = os.Symlink("Dockerfile", filepath.Join(profile, "link"))
			case "oversized":
				err = os.Truncate(filepath.Join(profile, "Dockerfile"), SnapshotLimit+1)
			case "eval state":
				state = profile
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := StageSuite(context.Background(), state, s); err == nil {
				t.Fatal("unsafe profile tree was accepted")
			}
		})
	}
}
