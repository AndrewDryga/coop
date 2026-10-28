package box

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestServicePolicyConcurrentPublicationIsComplete(t *testing.T) {
	pinHome(t)
	data := []byte(strings.Repeat("logs/\n", 32768))
	start := make(chan struct{})
	failures := make(chan error, 24)
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			path, err := durableServicePolicySnapshot(data)
			if err != nil {
				failures <- err
				return
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, data) {
				failures <- fmt.Errorf("read published snapshot: %v, bytes=%d", err, len(got))
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	root, err := serviceStateRoot("policies")
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 {
		t.Fatalf("publication retained temp files: %v %v", files, err)
	}
	info, err := files[0].Info()
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("snapshot mode: %v %v", info, err)
	}
}

func TestServicePolicyRefusesChangedExistingSnapshot(t *testing.T) {
	pinHome(t)
	data := []byte("private/\n")
	root, err := serviceStateRoot("policies")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, fmt.Sprintf("%x.coopignore", sha256.Sum256(data)))
	if err = os.WriteFile(path, []byte("changed"), 0400); err != nil {
		t.Fatal(err)
	}
	if _, err = durableServicePolicySnapshot(data); err == nil {
		t.Fatal("replaced mismatching snapshot")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "changed" {
		t.Fatalf("existing snapshot changed: %q %v", got, err)
	}
}
