package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestSourceLFSUsesOnlyTheGrantedRepositoryEndpointAndRetainsCompleteData(t *testing.T) {
	ctx := context.Background()
	data := bytes.Repeat([]byte("complete large payload\x00\n"), 300_000)
	oid := fmt.Sprintf("%x", sha256.Sum256(data))
	var unauthorized atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unauthorized.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer evil.Close()
	var downloads atomic.Int32
	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			t.Error("GitHub credential was sent to a non-GitHub data endpoint")
		}
		switch request.URL.Path {
		case "/repository/info/lfs/objects/batch":
			var body struct {
				Operation string `json:"operation"`
				Objects   []struct {
					OID  string `json:"oid"`
					Size int64  `json:"size"`
				} `json:"objects"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Operation != "download" ||
				len(body.Objects) != 1 || body.Objects[0].OID != oid || body.Objects[0].Size != int64(len(data)) {
				t.Errorf("unexpected LFS batch: %+v, %v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": []any{map[string]any{
				"oid": oid, "size": len(data), "actions": map[string]any{"download": map[string]any{"href": endpoint + "/payload"}},
			}}})
		case "/payload":
			downloads.Add(1)
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	endpoint = server.URL
	repository, git := gitrepo.New(t)
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(data))
	empty := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize 0\n", sha256.Sum256(nil))
	for path, body := range map[string]string{
		"large.bin": pointer, "empty.bin": empty, ".gitattributes": "*.bin filter=lfs -text\n",
		".lfsconfig": "[lfs]\nurl = " + evil.URL + "/stolen\nfetchexclude = *\nskipdownloaderrors = true\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "LFS source")
	source := testJobSource()
	commit, err := sourceGitValue(ctx, repository, "", "file", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sourceGitValue(ctx, repository, "", "file", "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	source.Binding.SelectedCommit, source.Binding.DefaultCommit, source.Binding.BaseCommit = commit, commit, commit
	source.Binding.AdmittedTree = tree
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	stager := &privateJobSourceStager{transport: &jobSourceGrantFixture{}, stateRoot: state,
		remoteForTest: repository, lfsEndpointForTest: endpoint + "/repository/info/lfs"}
	for range 2 {
		if err := stager.Stage(ctx, "job:lfs", source); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := source.StagingKey()
	checkout := filepath.Join(state, "job-sources", key, "repository")
	if actual, err := os.ReadFile(filepath.Join(checkout, "large.bin")); err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("source retained a pointer or incomplete payload: %d bytes, %v", len(actual), err)
	}
	if actual, err := os.ReadFile(filepath.Join(checkout, "empty.bin")); err != nil || len(actual) != 0 {
		t.Fatalf("empty object was not materialized: %q, %v", actual, err)
	}
	if unauthorized.Load() != 0 || downloads.Load() != 1 {
		t.Fatalf("unexpected transfers: unauthorized=%d downloads=%d", unauthorized.Load(), downloads.Load())
	}
	config, err := os.ReadFile(filepath.Join(checkout, ".git", "config"))
	if err != nil || strings.Contains(string(config), "Authorization") || strings.Contains(string(config), "secret-token") {
		t.Fatalf("source stored a credential: %q, %v", config, err)
	}
	object := filepath.Join(checkout, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	if err := stager.Stage(ctx, "job:lfs", source); !errors.Is(err, ErrJobSourceIntegrity) {
		t.Fatalf("warm source lost LFS object custody: %v", err)
	}
}

func TestSourceLFSFiltersCleanPayloadsBeforeApplyingStatusOutputBounds(t *testing.T) {
	stager, source, _, git := sourceRefreshFixture(t)
	repository := stager.remoteForTest
	empty := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize 0\n", sha256.Sum256(nil))
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs -text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := range 64 {
		if err := os.WriteFile(filepath.Join(repository, fmt.Sprintf("asset-%03d.bin", i)), []byte(empty), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "many LFS assets")
	commit, err := sourceGitValue(context.Background(), repository, "", "file", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sourceGitValue(context.Background(), repository, "", "file", "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	source.Binding.SelectedCommit, source.Binding.DefaultCommit, source.Binding.BaseCommit = commit, commit, commit
	source.Binding.AdmittedTree = tree
	for range 2 {
		if err := stager.Stage(context.Background(), "job:many-lfs", source); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := source.StagingKey()
	checkout := filepath.Join(stager.stateRoot, "job-sources", key, "repository")
	raw, err := sourceGitCommand(context.Background(), checkout, "", "status", "--porcelain=v2", "-z").Output()
	if err != nil || len(raw) <= 4096 {
		t.Fatalf("fixture did not exceed the pre-normalization status bound: %d bytes, %v", len(raw), err)
	}
}
