package workerproto

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedWorkspaceCheckpointGoldenBindsPortableTaskAndWorkspaceIdentity(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "workspace-checkpoint-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := DecodeWorkspaceCheckpoint(document)
	if err != nil {
		t.Fatalf("DecodeWorkspaceCheckpoint: %v", err)
	}
	if checkpoint.SessionRef != "session-1" || checkpoint.PlacementGeneration != 1 ||
		checkpoint.Task.QueueID != strings.Repeat("4", 32) || checkpoint.Task.TaskID != strings.Repeat("5", 32) ||
		len(checkpoint.Task.Subtasks) != 2 || !checkpoint.Task.Subtasks[0] || checkpoint.Task.Subtasks[1] {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	if checkpoint.Bundle.MediaType != WorkspaceCheckpointBundleMediaType || checkpoint.Bundle.ByteSize != 16384 {
		t.Fatalf("bundle = %+v", checkpoint.Bundle)
	}
}

func TestWorkspaceCheckpointRejectsUnknownUnboundedOrInconsistentState(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "workspace-checkpoint-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(document, &fixture); err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(map[string]any){
		"unknown field": func(value map[string]any) { value["worker_path"] = "/private/workspace" },
		"wrong version": func(value map[string]any) { value["version"] = float64(2) },
		"oversized bundle": func(value map[string]any) {
			value["bundle"].(map[string]any)["byte_size"] = float64(MaxWorkspaceCheckpointBundleBytes + 1)
		},
		"too many subtasks": func(value map[string]any) {
			value["task"].(map[string]any)["subtasks"] = make([]bool, MaxWorkspaceCheckpointSubtasks+1)
		},
		"gate receipt missing": func(value map[string]any) {
			delete(value["gate"].(map[string]any), "receipt_ref")
		},
		"not-run gate has receipt": func(value map[string]any) {
			value["gate"] = map[string]any{"status": "not_run", "receipt_ref": "gate:unexpected"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			body, _ := json.Marshal(fixture)
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			mutate(value)
			invalid, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeWorkspaceCheckpoint(invalid); err == nil {
				t.Fatal("invalid checkpoint was accepted")
			}
		})
	}
}

func TestSharedWorkspaceCheckpointBundleManifestBindsEveryPortableByte(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "workspace-checkpoint-bundle-v1.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeWorkspaceCheckpointBundleManifest(document)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.CheckpointRef != "checkpoint:session-1:g1:1" || manifest.TrackedPatch.Entry != "workspace.patch" ||
		len(manifest.UntrackedFiles) != 1 || string(manifest.UntrackedFiles[0].PathBytes) != "notes/plan.md" ||
		manifest.TaskProjection.Ref.TaskID != strings.Repeat("5", 32) ||
		len(manifest.TaskProjection.Files) != 2 || manifest.GateReceipt == nil {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestWorkspaceCheckpointBundleManifestRejectsAmbiguousOrUnsafeEntries(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "workspace-checkpoint-bundle-v1.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal(document, &original); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"unknown field": func(value map[string]any) { value["extra"] = true },
		"duplicate entry": func(value map[string]any) {
			files := value["task_projection"].(map[string]any)["files"].([]any)
			files[0].(map[string]any)["entry"] = "untracked/000000"
		},
		"absolute path bytes": func(value map[string]any) {
			value["untracked_files"].([]any)[0].(map[string]any)["path_b64"] = base64.StdEncoding.EncodeToString([]byte("/tmp/escape"))
		},
		"parent path bytes": func(value map[string]any) {
			value["untracked_files"].([]any)[0].(map[string]any)["path_b64"] = base64.StdEncoding.EncodeToString([]byte("../escape"))
		},
		"oversized file": func(value map[string]any) {
			value["untracked_files"].([]any)[0].(map[string]any)["byte_size"] = float64(MaxWorkspaceCheckpointBundleBytes)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(original)
			var value map[string]any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			mutate(value)
			changed, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeWorkspaceCheckpointBundleManifest(changed); err == nil {
				t.Fatal("invalid bundle manifest was accepted")
			}
		})
	}
}

func TestWorkspaceCheckpointDescriptorAndManifestCannotBeCrossed(t *testing.T) {
	checkpointDocument, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "workspace-checkpoint-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifestDocument, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "workspace-checkpoint-bundle-v1.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := DecodeWorkspaceCheckpoint(checkpointDocument)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeWorkspaceCheckpointBundleManifest(manifestDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceCheckpointPair(checkpoint, manifest); err != nil {
		t.Fatalf("matching checkpoint pair: %v", err)
	}

	crossed := manifest
	crossed.CandidateTreeSHA256 = strings.Repeat("a", 64)
	if err := ValidateWorkspaceCheckpointPair(checkpoint, crossed); err == nil {
		t.Fatal("crossed candidate tree was accepted")
	}
	crossed = manifest
	crossed.TaskProjection.TaskID = strings.Repeat("b", 32)
	if err := ValidateWorkspaceCheckpointPair(checkpoint, crossed); err == nil {
		t.Fatal("crossed task identity was accepted")
	}
	crossed = manifest
	crossed.GateReceipt = nil
	if err := ValidateWorkspaceCheckpointPair(checkpoint, crossed); err == nil {
		t.Fatal("completed checkpoint without gate receipt bytes was accepted")
	}
}
