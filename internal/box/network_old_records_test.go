package box

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plantKeyedWithdrawal files the withdrawal builds up to v11.3.0 wrote under the keyed project id
// and returns its file name.
func plantKeyedWithdrawal(t *testing.T, root, repo string) string {
	t.Helper()
	key, err := os.ReadFile(filepath.Join(root, "owner.key"))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("project-v1\x00" + repo))
	id := hex.EncodeToString(mac.Sum(nil))
	data, err := json.Marshal(map[string]any{"version": 1, "project_id": id})
	if err != nil {
		t.Fatal(err)
	}
	name := "withdrawn-" + id + ".json"
	if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// A withdrawal an older build filed is a barrier this build no longer reads. The launch of its
// project is refused by name through the real admission, a project it cannot belong to is not,
// and `coop net forget`'s capability removes the file and leaves the project withdrawn.
func TestAdmitNetworkRefusesAProjectWithAnOlderBuildsWithdrawal(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	cfg.Egress = ""
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rememberFilteredPosture(t, root, repo, nil)
	rememberFilteredPosture(t, root, other, nil)
	name := plantKeyedWithdrawal(t, root, repo)

	_, err = admitFixture(t, cfg, repo, NetworkAdmission{})
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "coop net forget --project "+repo) {
		t.Fatalf("admission of a project with an older build's withdrawal = %v; want a refusal that names %s and the fix", err, name)
	}
	if _, err := ProjectNetworkAccess(context.Background(), cfg, repo); err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("the posture view of that project = %v; want the same refusal", err)
	}
	if _, err := admitFixture(t, cfg, other, NetworkAdmission{}); err == nil || !strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
		t.Fatalf("an unrelated project's admission = %v; want it to reach the runtime preflight", err)
	}
	if _, err := os.Stat(filepath.Join(root, name)); err != nil {
		t.Fatalf("the refusal changed the old file: %v", err)
	}

	review, err := ReviewProjectNetworkForget(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer review.Close()
	if got := review.OldFiles(); len(got) != 1 || got[0] != name {
		t.Fatalf("forget lists %v; want [%s]", got, name)
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
		t.Fatalf("the old withdrawal survived the forget: %v", err)
	}
	_, err = admitFixture(t, cfg, repo, NetworkAdmission{})
	if err == nil || !strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("admission after the forget = %v; want the withdrawal's refusal", err)
	}
}
