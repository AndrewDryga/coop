package box

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestNativeHistoryOwnershipProofChangesRefusePublication(t *testing.T) {
	home, source, plan := nativeImportFixture(t)
	proof := []byte("original exact repository marker")
	name := ".project_root"
	if err := os.WriteFile(filepath.Join(source, name), proof, 0600); err != nil {
		t.Fatal(err)
	}
	witness := agents.NativeHistoryFile{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(proof)), Size: int64(len(proof))}
	plan.Dependencies = map[string][]agents.NativeHistoryFile{plan.Files[0].Path: {witness}}
	if err := os.WriteFile(filepath.Join(source, name), []byte("foreign repository marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := importNativeHistory(context.Background(), home, source, plan); err == nil {
		t.Fatal("unchanged companion accepted after ownership changed")
	}
	if _, err := os.Stat(filepath.Join(home, plan.Files[0].Path)); !os.IsNotExist(err) {
		t.Fatal("companion was published")
	}
	plan.Dependencies = nil
	if err := importNativeHistory(context.Background(), home, source, plan); err == nil {
		t.Fatal("pending receipt accepted a different proof set")
	}
}

func TestNativeHistoryCompletedReceiptDoesNotRevalidateOldProofs(t *testing.T) {
	home, source, plan := nativeImportFixture(t)
	proof := []byte("original marker")
	name := ".project_root"
	if err := os.WriteFile(filepath.Join(source, name), proof, 0600); err != nil {
		t.Fatal(err)
	}
	plan.Dependencies = map[string][]agents.NativeHistoryFile{plan.Files[0].Path: {{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(proof)), Size: int64(len(proof))}}}
	if err := importNativeHistory(context.Background(), home, source, plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, plan.Files[0].Path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, name), []byte("changed old marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := importNativeHistory(context.Background(), home, source, plan); err != nil {
		t.Fatal("completed import coupled to old writer", err)
	}
	if _, err := os.Stat(filepath.Join(home, plan.Files[0].Path)); !os.IsNotExist(err) {
		t.Fatal("native deletion resurrected")
	}
}
