package networkstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestLaunchInputsFreezePairWithoutPublicPayloads(t *testing.T) {
	s := openStore(t)
	input := LaunchInputs{Environment: []byte("KEY=fixture-secret\n"), MCP: []byte(`{"mcpServers":{}}`), Selection: []byte(`{"account":"private-selection"}`), Native: []byte("private-native-configuration")}
	id, err := s.RecordInputs(input)
	if err != nil || id == "" {
		t.Fatal(err)
	}
	got, err := s.Inputs(id)
	if err != nil || !bytes.Equal(got.Environment, input.Environment) || !bytes.Equal(got.MCP, input.MCP) || !bytes.Equal(got.Selection, input.Selection) || !bytes.Equal(got.Native, input.Native) {
		t.Fatal("stored pair changed", err)
	}
	got.Environment[0] = 'X'
	again, err := s.Inputs(id)
	if err != nil || !bytes.Equal(again.Environment, input.Environment) {
		t.Fatal("caller mutated retained pair", err)
	}
	repeated, err := s.RecordInputs(input)
	if err != nil || repeated != id {
		t.Fatal("idempotent publication changed identity", err)
	}
	foreign, err := openStore(t).RecordInputs(input)
	if err != nil || foreign == id {
		t.Fatal("input reference is not owner-bound", err)
	}
	boundary, err := s.RecordInputs(LaunchInputs{Environment: []byte("ab"), MCP: []byte("c")})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.RecordInputs(LaunchInputs{Environment: []byte("a"), MCP: []byte("bc")})
	if err != nil || boundary == other {
		t.Fatal("unframed pair identity", err)
	}
	data, err := json.Marshal(input)
	if err != nil || string(data) != "{}" {
		t.Fatal("payload can enter JSON projection", err)
	}
	metadata, err := os.ReadFile(filepath.Join(s.Path(), "inputs-"+id+".json"))
	if err != nil || bytes.Contains(metadata, []byte("fixture-secret")) || bytes.Contains(metadata, []byte("mcpServers")) || bytes.Contains(metadata, []byte("private-native")) {
		t.Fatal("metadata contains payload", err)
	}
	for _, size := range []int{0, MaxInputBytes} {
		input := LaunchInputs{Environment: bytes.Repeat([]byte("e"), size), MCP: bytes.Repeat([]byte("m"), size)}
		id, err := s.RecordInputs(input)
		if err != nil {
			t.Fatal("valid input bound", err)
		}
		got, err := s.Inputs(id)
		if err != nil || !bytes.Equal(got.Environment, input.Environment) || !bytes.Equal(got.MCP, input.MCP) {
			t.Fatal("bounded input round trip", err)
		}
	}
}

func TestLaunchInputsNativeFramingBoundAndIdempotence(t *testing.T) {
	s := openStore(t)
	// Base64-framed native JSON may exceed the environment/MCP file bound.
	input := LaunchInputs{Native: bytes.Repeat([]byte("n"), MaxNativeBytes)}
	if err := s.publish("ordinary-record", input.Native, false); err == nil {
		t.Fatal("native framing widened unrelated record limits")
	}
	id, err := s.RecordInputs(input)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := s.Inputs(id)
		if err != nil || !bytes.Equal(got.Native, input.Native) {
			t.Fatal("maximum native payload did not round trip", err)
		}
		repeated, err := s.RecordInputs(input)
		if err != nil || repeated != id {
			t.Fatal("maximum native payload did not republish idempotently", err)
		}
	}
}

func TestLaunchInputsRejectInvalidAndTamperedRecords(t *testing.T) {
	for _, kind := range []string{"env", "project", "review", "final", "mcp", "selection", "native", "truncated", "missing", "metadata", "unknown", "symlink", "fifo", "public", "oversize", "key-loss", "key-change"} {
		t.Run(kind, func(t *testing.T) {
			s := openStore(t)
			id, err := s.RecordInputs(LaunchInputs{Environment: []byte("KEY=original\n"), MCP: []byte("original")})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Path(), "inputs-"+id+".env")
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "env":
				write(path, []byte("KEY=changed!\n"))
			case "mcp":
				write(filepath.Join(s.Path(), "inputs-"+id+".mcp"), []byte("changed!"))
			case "selection":
				write(filepath.Join(s.Path(), "inputs-"+id+".selection"), []byte("changed!"))
			case "native":
				write(filepath.Join(s.Path(), "inputs-"+id+".native"), []byte("changed!"))
			case "project", "review", "final":
				write(filepath.Join(s.Path(), "inputs-"+id+"."+kind+"-env"), []byte("INJECTED=secret\n"))
			case "truncated":
				write(path, []byte(""))
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "metadata", "unknown":
				path = filepath.Join(s.Path(), "inputs-"+id+".json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "unknown" {
					data = append([]byte(`{"ignored":true,`), data[1:]...)
				} else {
					data = bytes.Replace(data, []byte(`"version":4`), []byte(`"version":1`), 1)
				}
				write(path, data)
			case "symlink", "fifo":
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					err = os.Symlink(path+"-original", path)
				} else {
					err = syscall.Mkfifo(path, 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				write(path, bytes.Repeat([]byte("x"), MaxInputBytes+1))
			case "key-loss":
				if err := os.Remove(filepath.Join(s.Path(), "owner.key")); err != nil {
					t.Fatal(err)
				}
			case "key-change":
				write(filepath.Join(s.Path(), "owner.key"), bytes.Repeat([]byte("z"), 32))
			}
			got, err := s.Inputs(id)
			if err == nil || len(got.Environment) != 0 || len(got.MCP) != 0 {
				t.Fatal("unsafe frozen inputs returned", err)
			}
		})
	}
	s := openStore(t)
	for _, id := range []string{"", "../owner.key", strings.Repeat("a", 64), strings.Repeat("A", 64)} {
		if _, err := s.Inputs(id); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
	for _, input := range []LaunchInputs{{Native: make([]byte, MaxNativeBytes+1)}, {Selection: make([]byte, MaxSelectionBytes+1)}, {Environment: make([]byte, MaxInputBytes+1)}, {MCP: make([]byte, MaxInputBytes+1)}, {ProjectEnvironment: make([]byte, MaxInputBytes), Environment: []byte("x")}, {ReviewEnvironment: make([]byte, MaxInputBytes), ProjectEnvironment: []byte("x")}} {
		if id, err := s.RecordInputs(input); err == nil || id != "" {
			t.Fatal("oversize inputs published")
		}
	}
	entries, err := os.ReadDir(s.Path())
	if err != nil || len(entries) != 1 {
		t.Fatal("invalid inputs left state", err)
	}
}

func TestLaunchInputsDurabilityAndConcurrentPublication(t *testing.T) {
	s := openStore(t)
	input := LaunchInputs{Environment: []byte("K=fixture\n")}
	failure := errors.New("fixture sync failure")
	s.syncDir = func(*os.File) error { return failure }
	if id, err := s.RecordInputs(input); id != "" || !errors.Is(err, failure) {
		t.Fatal("partial publication returned authority", err)
	}
	id := s.inputsID(input)
	if _, err := s.Inputs(id); err == nil {
		t.Fatal("partial pair accepted")
	}
	s.syncDir = nil
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := s.RecordInputs(input)
			if err != nil || got != id {
				t.Errorf("concurrent publish: %v", err)
			}
		})
	}
	wg.Wait()
	s.syncDir = func(*os.File) error { return failure }
	if id, err := s.RecordInputs(input); id != "" || !errors.Is(err, failure) {
		t.Fatal("equal bytes bypassed durability", err)
	}
	if _, err := s.Inputs(id); !errors.Is(err, failure) {
		t.Fatal("read bypassed durability", err)
	}
	s.syncDir = func(*os.File) error { return os.Chmod(s.Path(), 0755) }
	if got, err := s.Inputs(id); err == nil || got.Environment != nil {
		t.Fatal("post-sync custody loss accepted", err)
	}
}

func TestExecutionFrozenInputsAreRequiredForNewWorkNotRecovery(t *testing.T) {
	s, record := executionFixture(t)
	if !lowerHex(record.InputsID, 64) {
		t.Fatal("execution lost input custody")
	}
	for _, suffix := range []string{".project-env", ".env", ".review-env", ".mcp", ".selection", ".native", ".json"} {
		if err := os.Remove(filepath.Join(s.Path(), "inputs-"+record.InputsID+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	spec := ExecutionSpec{Project: record.Project, PolicyFingerprint: record.Snapshot.PolicyFingerprint, Runtime: record.Runtime, DaemonID: record.DaemonID, Endpoint: record.Endpoint, GatewayImage: record.GatewayImage, InputsID: record.InputsID}
	if got, err := s.CreateExecution(context.Background(), spec); err == nil || got.ID != "" {
		t.Fatal("missing inputs authorized new work", err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	got, err := evidence.Execution(record.ID)
	if err != nil || got.InputsID != record.InputsID {
		t.Fatal("input loss hid resource custody", err)
	}
	// Old execution records predate frozen inputs. They remain readable for
	// exact cleanup, but their empty reference cannot authorize a new launch.
	record.InputsID = ""
	record.Version, record.Purpose = 1, ""
	record.CandidateID, record.ClientImage, record.QualificationID = "", "", ""
	record.QualificationContract = ""
	record.TrialGroup, record.TrialCase, record.TrialClient = "", "", nil
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publish("execution-"+record.ID+".json", data, true); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Execution(record.ID); err != nil {
		t.Fatal("legacy cleanup record hidden", err)
	}
	spec.InputsID = ""
	if got, err := s.CreateExecution(context.Background(), spec); err == nil || got.ID != "" {
		t.Fatal("legacy empty inputs authorized new work", err)
	}
}
