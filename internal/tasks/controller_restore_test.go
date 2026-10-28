package tasks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func restoredControllerFixture(t *testing.T) (string, string, ControllerTaskDraft, ControllerTaskBinding) {
	t.Helper()
	original := t.TempDir()
	draft := ControllerTaskDraft{OfferRef: "record:task_offer:restored", Title: "Restore approved work", Prompt: "Continue the approved implementation.", SuccessChecks: []string{"focused checks pass"}}
	instance, err := EnsureControllerTask(original, draft)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ControllerTaskDraftSHA256(draft)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "workspace")
	if err := os.CopyFS(restored, os.DirFS(original)); err != nil {
		t.Fatal(err)
	}
	want := ControllerTaskBinding{QueueID: instance.Ref.QueueID, TaskID: instance.Ref.TaskID, ID: instance.Ref.ID, OfferRef: draft.OfferRef, DraftSHA256: digest}
	got, err := ReadControllerTaskBinding(restored, instance.Ref.ID)
	if err != nil || got != want {
		t.Fatalf("restored binding=%+v err=%v", got, err)
	}
	return restored, filepath.Join(restored, TasksRoot, StateTodo, instance.Ref.ID), draft, want
}

func TestRestoredControllerTaskRejectsChangedApprovalAndIdentity(t *testing.T) {
	for _, file := range []string{controllerTaskRecordFile, ".coop-controller-draft.json"} {
		for _, corrupt := range []struct {
			name string
			body func([]byte) []byte
		}{
			{"malformed", func([]byte) []byte { return []byte("{") }},
			{"wrong-type", func([]byte) []byte { return []byte("[]") }},
			{"unknown-field", func(b []byte) []byte { return bytes.Replace(b, []byte("{"), []byte(`{"untrusted":true,`), 1) }},
			{"trailing-value", func(b []byte) []byte { return append(append([]byte(nil), b...), []byte("\n{}")...) }},
		} {
			t.Run(file+"/"+corrupt.name, func(t *testing.T) {
				workspace, dir, _, want := restoredControllerFixture(t)
				path := filepath.Join(dir, file)
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				bad := corrupt.body(body)
				if err := os.WriteFile(path, bad, 0600); err != nil {
					t.Fatal(err)
				}
				got, err := ReadControllerTaskBinding(workspace, want.ID)
				if err == nil || got != (ControllerTaskBinding{}) {
					t.Fatalf("accepted invalid document: %+v %v", got, err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, bad) {
					t.Fatal("read changed rejected evidence")
				}
			})
		}
	}
	for _, name := range []string{"version", "offer", "digest", "prompt", "invalid draft matching digest", "queue missing", "task missing", "record missing", "draft missing", "unknown task"} {
		t.Run(name, func(t *testing.T) {
			workspace, dir, draft, want := restoredControllerFixture(t)
			record := controllerTaskRecord{Version: 1, OfferRef: draft.OfferRef, DraftSHA256: want.DraftSHA256}
			var removed string
			requestID := want.ID
			switch name {
			case "version":
				record.Version = 2
			case "offer":
				record.OfferRef = "record:task_offer:another"
			case "digest":
				record.DraftSHA256 = strings.Repeat("a", 64)
			case "prompt":
				draft.Prompt = "Changed approved work."
			case "invalid draft matching digest":
				draft.SuccessChecks = nil
				data, err := json.Marshal(draft)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(data)
				record.DraftSHA256 = hex.EncodeToString(sum[:])
			case "queue missing":
				removed = filepath.Join(workspace, TasksRoot, QueueIdentityFile)
			case "task missing":
				removed = filepath.Join(dir, TaskIdentityFile)
			case "record missing":
				removed = filepath.Join(dir, controllerTaskRecordFile)
			case "draft missing":
				removed = filepath.Join(dir, ".coop-controller-draft.json")
			case "unknown task":
				requestID = "nonexistent-task"
			}
			for file, value := range map[string]any{controllerTaskRecordFile: record, ".coop-controller-draft.json": draft} {
				body, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, file), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if removed != "" {
				if err := os.Remove(removed); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ReadControllerTaskBinding(workspace, requestID)
			if err == nil || got != (ControllerTaskBinding{}) {
				t.Fatalf("accepted changed approval: %+v %v", got, err)
			}
			if removed != "" {
				if _, err := os.Stat(removed); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("read recreated identity: %v", err)
				}
			}
		})
	}
}
