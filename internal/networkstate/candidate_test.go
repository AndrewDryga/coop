package networkstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func candidateFixture() CandidateSpec {
	return CandidateSpec{Runtime: RuntimeBinding{HostFamily: "darwin", Endpoint: "unix:///fixture.sock", DaemonID: "fixture-daemon",
		OS: "linux", Architecture: "arm64", ServerVersion: "29.4.0", KernelVersion: "7.0.14-fixture", SecurityOptions: []string{"name=seccomp,profile=builtin", "name=cgroupns"}},
		ClientImage: "sha256:" + strings.Repeat("a", 64), GatewayImage: "sha256:" + strings.Repeat("b", 64),
		ClientDefinition: strings.Repeat("c", 64), ClientClosure: strings.Repeat("d", 64), GatewaySource: strings.Repeat("e", 64),
		Libc: "glibc", NodeBase: "node:24-slim@sha256:" + strings.Repeat("f", 64), GoBase: "golang:1.26.6-bookworm@sha256:" + strings.Repeat("0", 64)}
}

func TestNetworkCandidateIsImmutableOwnerBoundConstruction(t *testing.T) {
	s := openStore(t)
	input := candidateFixture()
	record, err := s.RecordCandidate(input)
	if err != nil || record.ID == "" {
		t.Fatal("publish", err)
	}
	// Sorting/copying must not mutate the caller or leave shared slices.
	if input.Runtime.SecurityOptions[0] != "name=seccomp,profile=builtin" {
		t.Fatal("caller runtime observation was mutated")
	}
	input.Runtime.SecurityOptions[0] = "changed"
	got, err := s.Candidate(record.ID)
	if err != nil || !equalJSON(record, got) {
		t.Fatal("read changed construction", err)
	}
	again := candidateFixture()
	slices.Reverse(again.Runtime.SecurityOptions)
	again.Runtime.SecurityOptions = append(again.Runtime.SecurityOptions, again.Runtime.SecurityOptions[0])
	repeated, err := s.RecordCandidate(again)
	if err != nil || repeated.ID != record.ID {
		t.Fatal("equivalent observation lost immutable identity", err)
	}
	foreign := openStore(t)
	other, err := foreign.RecordCandidate(candidateFixture())
	if err != nil || other.ID == record.ID {
		t.Fatal("candidate references are not owner-bound", err)
	}
	data, _ := json.Marshal(record)
	if err := foreign.publish("candidate-"+record.ID+".json", data, false); err != nil {
		t.Fatal(err)
	}
	if got, err := foreign.Candidate(record.ID); err == nil || got.ID != "" {
		t.Fatal("imported foreign candidate became authority")
	}
	var shape map[string]any
	if err := json.Unmarshal(data, &shape); err != nil || shape["qualified"] != nil || shape["passed"] != nil {
		t.Fatal("construction claims qualification", err)
	}
}

func TestNetworkCandidateRejectsInvalidIdentityWithoutPublishing(t *testing.T) {
	cases := map[string]func(*CandidateSpec){
		"client tag":     func(s *CandidateSpec) { s.ClientImage = "coop-clients:tag" },
		"helper tag":     func(s *CandidateSpec) { s.GatewayImage = "coop-network:tag" },
		"same image":     func(s *CandidateSpec) { s.GatewayImage = s.ClientImage },
		"definition":     func(s *CandidateSpec) { s.ClientDefinition = strings.Repeat("C", 64) },
		"closure":        func(s *CandidateSpec) { s.ClientClosure = "" },
		"helper source":  func(s *CandidateSpec) { s.GatewaySource = "latest" },
		"base tag":       func(s *CandidateSpec) { s.NodeBase = "node:24-slim" },
		"base control":   func(s *CandidateSpec) { s.GoBase = "evil\n@" + s.ClientImage },
		"libc":           func(s *CandidateSpec) { s.Libc = "musl" },
		"host":           func(s *CandidateSpec) { s.Runtime.HostFamily = "windows" },
		"remote":         func(s *CandidateSpec) { s.Runtime.Endpoint = "tcp://daemon:2375" },
		"daemon":         func(s *CandidateSpec) { s.Runtime.DaemonID = "" },
		"platform":       func(s *CandidateSpec) { s.Runtime.OS = "windows" },
		"arch":           func(s *CandidateSpec) { s.Runtime.Architecture = "x86_64" },
		"server":         func(s *CandidateSpec) { s.Runtime.ServerVersion = "\x1b[0m" },
		"kernel":         func(s *CandidateSpec) { s.Runtime.KernelVersion = "" },
		"rootless":       func(s *CandidateSpec) { s.Runtime.SecurityOptions = []string{"name=rootless"} },
		"userns":         func(s *CandidateSpec) { s.Runtime.SecurityOptions = []string{"name=userns"} },
		"security bound": func(s *CandidateSpec) { s.Runtime.SecurityOptions = make([]string, 33) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			spec := candidateFixture()
			change(&spec)
			if got, err := s.RecordCandidate(spec); err == nil || got.ID != "" {
				t.Fatal("invalid candidate published", err)
			}
			files, err := os.ReadDir(s.Path())
			if err != nil || len(files) != 1 || files[0].Name() != "owner.key" {
				t.Fatal("invalid construction left a record", err)
			}
		})
	}
}

func TestNetworkCandidateReadRejectsTamperAndFilesystemTraps(t *testing.T) {
	for _, kind := range []string{"content", "unknown-field", "noncanonical", "symlink", "fifo", "oversized", "public", "key-loss", "key-change"} {
		t.Run(kind, func(t *testing.T) {
			s := openStore(t)
			record, err := s.RecordCandidate(candidateFixture())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Path(), "candidate-"+record.ID+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "content":
				record.Spec.GatewayImage = "sha256:" + strings.Repeat("9", 64)
				data, _ = json.Marshal(record)
			case "unknown-field":
				data = append([]byte(`{"qualified":true,`), data[1:]...)
			case "noncanonical":
				slices.Reverse(record.Spec.Runtime.SecurityOptions)
				data, _ = json.Marshal(record)
			case "symlink":
				target := filepath.Join(s.Path(), "other.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				data = []byte(strings.Repeat(" ", maxCandidateBytes+1))
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "key-loss":
				if err := os.Remove(filepath.Join(s.Path(), "owner.key")); err != nil {
					t.Fatal(err)
				}
			case "key-change":
				if err := os.WriteFile(filepath.Join(s.Path(), "owner.key"), []byte(strings.Repeat("z", 32)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if slices.Contains([]string{"content", "unknown-field", "noncanonical", "oversized"}, kind) {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := s.Candidate(record.ID); err == nil || got.ID != "" {
				t.Fatal("unsafe stored candidate was accepted", kind, err)
			}
		})
	}
}

func TestNetworkCandidatePublicationMustBeDurable(t *testing.T) {
	s := openStore(t)
	failure := errors.New("fixture directory-sync failure")
	s.syncDir = func(*os.File) error { return failure }
	if got, err := s.RecordCandidate(candidateFixture()); !errors.Is(err, failure) || got.ID != "" {
		t.Fatal("ambiguous publication returned usable candidate", err)
	}
	spec, _ := canonicalCandidate(candidateFixture())
	id := s.candidateID(spec)
	if got, err := s.Candidate(id); !errors.Is(err, failure) || got.ID != "" {
		t.Fatal("visible bytes bypassed failed durability", err)
	}
	if got, err := s.RecordCandidate(candidateFixture()); !errors.Is(err, failure) || got.ID != "" {
		t.Fatal("equal publication bypassed failed durability", err)
	}
	s.syncDir = nil
	got, err := s.RecordCandidate(candidateFixture())
	if err != nil || got.ID != id {
		t.Fatal("publication could not be confirmed", err)
	}
	loaded, err := s.Candidate(id)
	if err != nil || !equalJSON(got, loaded) {
		t.Fatal("confirmed candidate changed", err)
	}
}

func TestNetworkCandidateRechecksCustodyAfterDirectorySync(t *testing.T) {
	for _, operation := range []string{"read", "publish"} {
		for _, fault := range []string{"root-replaced", "root-public", "key-replaced"} {
			t.Run(operation+"/"+fault, func(t *testing.T) {
				s := openStore(t)
				record, err := s.RecordCandidate(candidateFixture())
				if err != nil {
					t.Fatal(err)
				}
				fired := false
				s.syncDir = func(dir *os.File) error {
					if !fired {
						fired = true
						switch fault {
						case "root-replaced":
							if err := os.Rename(s.Path(), s.Path()+"-original"); err != nil {
								return err
							}
							if err := os.Mkdir(s.Path(), 0700); err != nil {
								return err
							}
						case "root-public":
							if err := os.Chmod(s.Path(), 0755); err != nil {
								return err
							}
						case "key-replaced":
							if err := os.WriteFile(filepath.Join(s.Path(), "owner.key"), []byte(strings.Repeat("q", 32)), 0600); err != nil {
								return err
							}
						}
					}
					return dir.Sync()
				}
				var got Candidate
				if operation == "read" {
					got, err = s.Candidate(record.ID)
				} else {
					got, err = s.RecordCandidate(candidateFixture())
				}
				if !fired || err == nil || got.ID != "" {
					t.Fatal("changed custody returned usable candidate", fired, got.ID, err)
				}
			})
		}
	}
}

func TestNetworkCandidateConcurrentPublicationAndExactRuntime(t *testing.T) {
	s := openStore(t)
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for range 16 {
		wg.Go(func() {
			got, err := s.RecordCandidate(candidateFixture())
			if err != nil {
				t.Error(err)
			}
			ids <- got.ID
		})
	}
	wg.Wait()
	close(ids)
	first := <-ids
	for id := range ids {
		if id == "" || id != first {
			t.Fatal("concurrent construction split identity")
		}
	}
	original := candidateFixture().Runtime
	reordered := candidateFixture().Runtime
	slices.Reverse(reordered.SecurityOptions)
	if !EqualRuntimeBinding(original, reordered) {
		t.Fatal("security option ordering changed runtime")
	}
	changes := []func(*RuntimeBinding){
		func(r *RuntimeBinding) { r.HostFamily = "linux" },
		func(r *RuntimeBinding) { r.Endpoint = "unix:///other.sock" },
		func(r *RuntimeBinding) { r.DaemonID = "other-daemon" },
		func(r *RuntimeBinding) { r.Architecture = "amd64" },
		func(r *RuntimeBinding) { r.ServerVersion = "29.4.1" },
		func(r *RuntimeBinding) { r.KernelVersion = "7.0.15-fixture" },
		func(r *RuntimeBinding) { r.SecurityOptions = nil },
	}
	for _, change := range changes {
		changed := candidateFixture().Runtime
		change(&changed)
		if EqualRuntimeBinding(original, changed) {
			t.Fatal("unqualified runtime change accepted", changed)
		}
	}
}
