package workerproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

func validJobSpec() JobSpec {
	return JobSpec{
		Version: 2, JobRef: "job:one",
		Source: &JobSource{
			RepositoryRef: "repo:one", GitHubRepository: "example/repository", GitHubRepositoryID: 17,
			Binding: session.SourceBinding{
				Version: 1, Kind: session.SourceDefault, Requested: session.SourceSelector{Kind: session.SourceDefault},
				RemoteIdentity: "origin", DefaultRef: "refs/heads/main",
				DefaultCommit: strings.Repeat("a", 40), SelectedCommit: strings.Repeat("a", 40),
				SelectedRef: ptrString("refs/heads/main"), BaseCommit: strings.Repeat("a", 40),
				AdmittedTree: strings.Repeat("c", 40), ResolvedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
			}, Submodules: []JobSubmodule{},
		},
		Companions: []JobCompanion{}, Targets: []string{"codex:gpt-6-sol@default"},
		Mode: "normal", RepositoryReadOnly: true,
		Egress:      JobEgress{Mode: "none", Rules: []JobRule{}},
		Environment: map[string]string{"CI": "1"},
		Check:       JobCheck{Argv: []string{"make", "test"}, Environment: map[string]string{"DATABASE_URL": "postgres://test"}},
		Resources:   JobResources{CPUMillis: 2000, MemoryBytes: 4 << 30, PIDs: 512},
		Limits: JobLimits{
			MaxTurns: 100, MaxQueuedTurns: 20, MaxQueuedBytes: 1 << 20,
			TurnTimeoutMS: 3_600_000, MaxPatchBytes: 1 << 20,
		},
	}
}

func ptrString(value string) *string { return &value }

func TestJobExactCommitRequiresExplicitNullSelectedRef(t *testing.T) {
	spec := validJobSpec()
	spec.Source.Binding.Kind = session.SourceCommit
	spec.Source.Binding.Requested = session.SourceSelector{Kind: session.SourceCommit, SHA: strings.Repeat("b", 40)}
	spec.Source.Binding.SelectedCommit = strings.Repeat("b", 40)
	spec.Source.Binding.SelectedRef = nil
	document, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeJobSpec(document); err != nil {
		t.Fatalf("exact commit with explicit null selected_ref: %v", err)
	}
	missing := strings.Replace(string(document), `"selected_ref":null,`, "", 1)
	if _, err := DecodeJobSpec([]byte(missing)); err == nil {
		t.Fatal("accepted an omitted selected_ref")
	}
}

func TestJobSpecRoundTripAndDigest(t *testing.T) {
	spec := validJobSpec()
	// Pin the cross-language JSON escape boundary as well as object-key ordering.
	spec.Targets = []string{"codex:gpt-6&sol@default"}
	document, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeJobSpec(document)
	if err != nil {
		t.Fatal(err)
	}
	first, err := decoded.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var reordered map[string]any
	if err := json.Unmarshal(document, &reordered); err != nil {
		t.Fatal(err)
	}
	other, err := json.Marshal(reordered)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := DecodeJobSpec(other)
	if err != nil {
		t.Fatal(err)
	}
	second, err := replayed.Digest()
	if err != nil || first != second {
		t.Fatalf("job digest changed with JSON field order: %s %s %v", first, second, err)
	}
	const rykerDigest = "f99e0c853235d59ca54d9e3d5eec95d2c70b1ef01e1f5fd7af1a02fef1e79b83"
	if first != rykerDigest {
		t.Fatalf("job digest = %s, want shared Ryker vector %s", first, rykerDigest)
	}
}

func TestJobSpecRejectsMissingAndDuplicateAuthority(t *testing.T) {
	document, err := json.Marshal(validJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"source", "environment", "check", "resources", "companions", "limits"} {
		t.Run("missing "+field, func(t *testing.T) {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(document, &object); err != nil {
				t.Fatal(err)
			}
			delete(object, field)
			bad, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeJobSpec(bad); err == nil {
				t.Fatal("accepted missing authority")
			}
		})
	}
	for name, bad := range map[string][]byte{
		"duplicate top-level": []byte(strings.Replace(string(document), `"version":2`, `"version":2,"version":2`, 1)),
		"duplicate nested":    []byte(strings.Replace(string(document), `"github_repository_id":17`, `"github_repository_id":17,"github_repository_id":17`, 1)),
		"case alias":          []byte(strings.Replace(string(document), `"version":2`, `"version":2,"Version":3`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeJobSpec(bad); err == nil {
				t.Fatal("accepted incomplete or ambiguous job authority")
			}
		})
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(document, &root); err != nil {
		t.Fatal(err)
	}
	var limits map[string]json.RawMessage
	if err := json.Unmarshal(root["limits"], &limits); err != nil {
		t.Fatal(err)
	}
	delete(limits, "max_patch_bytes")
	root["limits"], err = json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeJobSpec(bad); err == nil {
		t.Fatal("accepted a missing nested limit")
	}
	root["limits"], _ = json.Marshal(validJobSpec().Limits)
	for _, field := range []string{"argv", "environment"} {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(root["check"], &nested); err != nil {
			t.Fatal(err)
		}
		delete(nested, field)
		badCheck, _ := json.Marshal(nested)
		root["check"] = badCheck
		bad, _ := json.Marshal(root)
		if _, err := DecodeJobSpec(bad); err == nil {
			t.Fatalf("accepted check without %s", field)
		}
		root["check"], _ = json.Marshal(validJobSpec().Check)
	}
}

func TestJobSetupIsRequiredAndChangesDigest(t *testing.T) {
	base := validJobSpec()
	baseDigest, err := base.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*JobSpec){
		"work environment": func(job *JobSpec) { job.Environment["CI"] = "0" },
		"check argv":       func(job *JobSpec) { job.Check.Argv[1] = "lint" },
		"check environment": func(job *JobSpec) {
			job.Check.Environment["DATABASE_URL"] = "postgres://other"
		},
		"cpu":    func(job *JobSpec) { job.Resources.CPUMillis++ },
		"memory": func(job *JobSpec) { job.Resources.MemoryBytes++ },
		"pids":   func(job *JobSpec) { job.Resources.PIDs++ },
	} {
		t.Run(name, func(t *testing.T) {
			job := validJobSpec()
			change(&job)
			digest, err := job.Digest()
			if err != nil || digest == baseDigest {
				t.Fatalf("changed setup digest = %s, err=%v", digest, err)
			}
		})
	}
	for name, change := range map[string]func(*JobSpec){
		"retired version":              func(job *JobSpec) { job.Version = 1 },
		"missing work env":             func(job *JobSpec) { job.Environment = nil },
		"missing check argv":           func(job *JobSpec) { job.Check.Argv = nil },
		"reserved env":                 func(job *JobSpec) { job.Environment["COOP_PIDS"] = "0" },
		"invalid env key":              func(job *JobSpec) { job.Environment["BAD-KEY"] = "x" },
		"newline env":                  func(job *JobSpec) { job.Check.Environment["CI"] = "1\nEVIL=1" },
		"trimmed env":                  func(job *JobSpec) { job.Environment["CI"] = " leading" },
		"zero cpu":                     func(job *JobSpec) { job.Resources.CPUMillis = 0 },
		"below runtime cpu minimum":    func(job *JobSpec) { job.Resources.CPUMillis = 9 },
		"zero memory":                  func(job *JobSpec) { job.Resources.MemoryBytes = 0 },
		"below runtime memory minimum": func(job *JobSpec) { job.Resources.MemoryBytes = (6 << 20) - 1 },
		"zero pids":                    func(job *JobSpec) { job.Resources.PIDs = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			job := validJobSpec()
			change(&job)
			if err := job.Validate(); err == nil {
				t.Fatal("accepted invalid job setup")
			}
		})
	}
	minimumMemory := validJobSpec()
	minimumMemory.Resources.CPUMillis = 10
	minimumMemory.Resources.MemoryBytes = 6 << 20
	if err := minimumMemory.Validate(); err != nil {
		t.Fatalf("runtime minimum memory rejected: %v", err)
	}
}

func TestJobTimestampCanonicalIdentity(t *testing.T) {
	spec := validJobSpec()
	spec.Targets = []string{"codex:gpt-6&sol@default"}
	document, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	for timestamp, expected := range map[string]string{
		"2026-09-26T12:00:00Z":           "f99e0c853235d59ca54d9e3d5eec95d2c70b1ef01e1f5fd7af1a02fef1e79b83",
		"2026-09-26T12:00:00.12345Z":     "912849518cdb641b0d52583c78c1eb989f1d3aeeed5649f2b0714609d2cc1ba5",
		"2026-09-26T12:00:00.000001Z":    "f80344cd93c76c3fe1dde45a99b7749bb3719313f7017f0b78cfec95c46f92de",
		"2026-09-26T12:00:00.123456789Z": "2396bbf41ba21b17d8e7be0e3911496ef319b7adbf786baac381a3763bf043eb",
	} {
		raw := strings.Replace(string(document), "2026-09-26T12:00:00Z", timestamp, 1)
		job, err := DecodeJobSpec([]byte(raw))
		if err != nil {
			t.Fatalf("canonical timestamp %q: %v", timestamp, err)
		}
		digest, err := job.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if digest != expected {
			t.Errorf("timestamp %s digest = %s, want shared controller vector %s", timestamp, digest, expected)
		}
	}
	for _, timestamp := range []string{
		"2026-09-26T12:00:00.123450Z", "2026-09-26T12:00:00.120000Z",
		"2026-09-26T12:00:00.000000Z", "2026-09-26T12:00:00.1234567891Z",
		"2026-09-26T12:00:00+00:00", "2026-09-26T12:00:00,1Z", "2026-02-30T12:00:00Z",
	} {
		raw := strings.Replace(string(document), "2026-09-26T12:00:00Z", timestamp, 1)
		if _, err := DecodeJobSpec([]byte(raw)); err == nil {
			t.Fatalf("accepted noncanonical timestamp %q", timestamp)
		}
	}
}

func TestJobSpecRejectsUnknownAndOversizedAuthority(t *testing.T) {
	spec := validJobSpec()
	document, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"unknown top-level field": append(document[:len(document)-1], []byte(`,"host_path":"/tmp/repo"}`)...),
		"unknown nested field":    []byte(strings.Replace(string(document), `"github_repository_id":17`, `"github_repository_id":17,"url":"https://example.invalid/repo"`, 1)),
		"oversized":               []byte(strings.Repeat(" ", maxJobDocumentBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeJobSpec(bad); err == nil {
				t.Fatal("accepted invalid job authority")
			}
		})
	}
}

func TestJobSpecRejectsChangedSource(t *testing.T) {
	spec := validJobSpec()
	spec.Source.Binding.SelectedCommit = strings.Repeat("z", 40)
	if err := spec.Validate(); err == nil {
		t.Fatal("accepted non-commit source")
	}
}

// The readonly and bare modes are gone: a job naming either is refused by name, with the shape
// that replaces it, and never runs as normal. A read-only repository is mode normal with
// repository_read_only, which the shared vector job above already is.
func TestJobSpecRefusesRemovedModes(t *testing.T) {
	for _, mode := range []string{"readonly", "bare"} {
		spec := validJobSpec()
		spec.Mode = mode
		if mode == "bare" {
			spec.Source, spec.RepositoryReadOnly = nil, false
		}
		document, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		_, err = DecodeJobSpec(document)
		if err == nil || !strings.Contains(err.Error(), "job mode \""+mode+"\" was removed") ||
			!strings.Contains(err.Error(), "repository_read_only") {
			t.Fatalf("%s job = %v, want a refusal naming the removed mode and its replacement", mode, err)
		}
	}
	if spec := validJobSpec(); spec.Mode != "normal" || !spec.RepositoryReadOnly || spec.Validate() != nil {
		t.Fatal("a normal job with a read-only repository is refused")
	}
}

func TestNormalJobCanUseEmptyWorkspaceAndAuthorizedCompanions(t *testing.T) {
	spec := validJobSpec()
	companion := *spec.Source
	spec.Source = nil
	spec.Companions = []JobCompanion{{Name: "library", Source: companion}}
	document, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeJobSpec(document); err != nil {
		t.Fatal(err)
	}
}

func TestJobSubmoduleManifestPinsRecursiveRepositoryAuthority(t *testing.T) {
	job := validJobSpec()
	module := JobSubmodule{
		Path: "vendor/library", RepositoryRef: "repo:library", GitHubRepository: "example/library", GitHubRepositoryID: 18,
		Commit: strings.Repeat("d", 40), Tree: strings.Repeat("e", 40), Submodules: []JobSubmodule{},
	}
	nested := module
	nested.Path = "nested"
	module.Submodules = []JobSubmodule{nested}
	job.Source.Submodules = []JobSubmodule{module}
	document, _ := json.Marshal(job)
	if _, err := DecodeJobSpec(document); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "/absolute", "../escape", "a/../b", "a//b", ".git", "a/.GIT/b", "a\\b", "a\x00b"} {
		job.Source.Submodules[0].Path = path
		if err := job.Validate(); err == nil {
			t.Fatalf("accepted unsafe submodule path %q", path)
		}
	}
	job.Source.Submodules[0].Path = "vendor/library"
	job.Source.Submodules = append(job.Source.Submodules, job.Source.Submodules[0])
	if err := job.Validate(); err == nil {
		t.Fatal("accepted duplicate submodule path")
	}
}

func TestJobSpecEnforcesExistingSessionBounds(t *testing.T) {
	for name, change := range map[string]func(*JobSpec){
		"too many targets": func(spec *JobSpec) {
			spec.Targets = []string{"codex@one", "codex@two", "codex@three", "codex@four", "codex@five"}
		},
		"too many queued turns": func(spec *JobSpec) { spec.Limits.MaxQueuedTurns = 1001 },
		"oversized patch":       func(spec *JobSpec) { spec.Limits.MaxPatchBytes = (1 << 20) + 1 },
		"long warm lease":       func(spec *JobSpec) { spec.Limits.WarmIdleTimeoutMS = 3_600_001 },
		"open export": func(spec *JobSpec) {
			spec.Egress.ExportDestinations = true
		},
		"service grant without service authority": func(spec *JobSpec) {
			spec.Egress = JobEgress{Mode: "filtered", Rules: []JobRule{{To: JobDestination{Service: "database"}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := validJobSpec()
			change(&spec)
			if err := spec.Validate(); err == nil {
				t.Fatal("accepted job beyond Coop's existing session bounds")
			}
		})
	}
}
