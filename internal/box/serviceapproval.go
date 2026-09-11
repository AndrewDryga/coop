package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/project"
)

// ServiceStateRootEnv overrides where coop keeps the host-owned state behind sibling services —
// approvals and the decoy files sidecars mount. Honored only by test binaries, which otherwise get
// a directory under the system temp dir, so a test never reads or writes the developer's real state.
const ServiceStateRootEnv = "COOP_SERVICE_STATE_ROOT"

// ServiceApproval records a human's decision that the sibling services defined by one exact
// compose file may read the secret-looking files it binds — a generated dev TLS key for Keycloak,
// say — instead of the empty decoys serviceShadowOverride would hand them.
//
// The record is keyed by the compose file's CONTENT digest, not its path or workspace: the human
// vouched for that text, and an agent editing the file (the one way a box can reach these binds)
// voids the approval by construction. It lives on the host under ~/.local/state, where no box can
// write, which is what makes it an approval rather than a hint.
type ServiceApproval struct {
	File       string    `json:"file"`      // repo-relative compose path at approval time
	Workspace  string    `json:"workspace"` // where it was approved (display only)
	Paths      []string  `json:"paths"`     // the secret-looking bind sources the approver saw
	ApprovedAt time.Time `json:"approved_at"`
	ApprovedBy string    `json:"approved_by"`
}

// serviceStateRoot is <state>/<name>: host-owned, outside every repo and every box, which is what
// makes an approval an approval and keeps a mounted decoy from vanishing under a running sidecar.
func serviceStateRoot(name string) (string, error) {
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		root := os.Getenv(ServiceStateRootEnv)
		if root == "" {
			root = filepath.Join(os.TempDir(), "coop-test-service-state")
		}
		return filepath.Join(root, name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "coop", name), nil
}

func serviceApprovalRoot() (string, error) { return serviceStateRoot("service-approvals") }

func composeDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ApprovedServiceSecrets reports whether this exact compose content was approved on this machine.
// Any read failure reads as "not approved": the decoys stay, which is the safe direction.
func ApprovedServiceSecrets(data []byte) (ServiceApproval, bool) {
	root, err := serviceApprovalRoot()
	if err != nil {
		return ServiceApproval{}, false
	}
	raw, err := os.ReadFile(filepath.Join(root, composeDigest(data)+".json"))
	if err != nil {
		return ServiceApproval{}, false
	}
	var approval ServiceApproval
	if err := json.Unmarshal(raw, &approval); err != nil {
		return ServiceApproval{}, false
	}
	return approval, true
}

// ServiceSecretReview is what `coop up` puts in front of a human: the secret-looking files one
// compose file binds into its services, which stay decoys until Approve is called.
type ServiceSecretReview struct {
	File  string       // repo-relative compose path
	Files []ReviewFile // the secret-looking bind sources still hidden, sorted by path

	workspace string
	data      []byte
}

// ReviewFile is one hidden file, with the two things that tell a human whether to expect it: does
// the repo say its services need this file, and is it new since the last time they said yes.
type ReviewFile struct {
	Path      string
	Requested bool // .agent/project.yaml lists it under services.require_real_files
	New       bool // this workspace approved this compose file before, and this path was not in it
}

// Paths is the review's files, in order — what Approve records and what the caller prints.
func (r *ServiceSecretReview) Paths() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Files))
	for _, f := range r.Files {
		out = append(out, f.Path)
	}
	return out
}

// Reason is the one-line explanation printed beside a hidden file at the prompt.
func (f ReviewFile) Reason() string {
	reason := "nothing in the repo asks for it"
	if f.Requested {
		reason = project.File + " asks for it"
	}
	if f.New {
		reason += "; new since you last approved this file"
	}
	return reason
}

// ReviewServiceSecrets returns the review a human must see before workspace's compose file (an
// absolute path inside it) hands services any secret-looking file, or nil when nothing is hidden
// or every hidden file is already approved for this exact content. A compose file the host would
// refuse to run is an error here too, so `coop up` reports the violation before asking anything.
func ReviewServiceSecrets(workspace, file string) (*ServiceSecretReview, error) {
	data, err := readValidatedCompose(file, workspace, false)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	_, hidden, err := serviceShadowPlan(workspace, abs, data)
	if err != nil {
		return nil, err
	}
	if len(hidden) == 0 {
		return nil, nil
	}
	if approval, ok := ApprovedServiceSecrets(data); ok {
		remaining := make([]string, 0, len(hidden))
		approved := make(map[string]bool, len(approval.Paths))
		for _, p := range approval.Paths {
			approved[p] = true
		}
		for _, p := range hidden {
			if !approved[p] {
				remaining = append(remaining, p)
			}
		}
		hidden = remaining
		if len(hidden) == 0 {
			return nil, nil
		}
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil {
		rel = abs
	}
	// The repo's own request list: committed, agent-writable, and worth exactly nothing on its own —
	// it only labels the prompt, so a file the repo never asked for stands out from the one its
	// services genuinely need.
	proj, err := project.Load(workspace)
	if err != nil {
		return nil, err
	}
	requested := make(map[string]bool, len(proj.Services.RequireRealFiles))
	for _, p := range proj.Services.RequireRealFiles {
		requested[p] = true
	}
	// "New" only means something once this workspace has approved this compose file at least once.
	previous, hadPrevious := lastApprovalFor(workspace, filepath.ToSlash(rel))
	seen := make(map[string]bool, len(previous))
	for _, p := range previous {
		seen[p] = true
	}
	files := make([]ReviewFile, 0, len(hidden))
	for _, p := range hidden {
		files = append(files, ReviewFile{Path: p, Requested: requested[p], New: hadPrevious && !seen[p]})
	}
	// The order is the file's own, so a reader can follow the prompt against their Compose file.
	// What deserves a second look is carried by the LABELS under each path — "Not requested in
	// .agent/project.yaml", "Added since your last approval" — which a reordering could not make
	// louder and a reader cannot miss in a list this short.
	return &ServiceSecretReview{File: filepath.ToSlash(rel), Files: files, workspace: workspace, data: data}, nil
}

// lastApprovalFor returns the paths approved most recently for this workspace and compose path,
// across every version of that file. Approvals are keyed by content, so an edited compose file
// starts from nothing; this is what still lets the prompt say which of the paths are new.
func lastApprovalFor(workspace, file string) ([]string, bool) {
	root, err := serviceApprovalRoot()
	if err != nil {
		return nil, false
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, false
	}
	var newest ServiceApproval
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		var approval ServiceApproval
		if json.Unmarshal(raw, &approval) != nil || approval.Workspace != workspace || approval.File != file {
			continue
		}
		if !found || approval.ApprovedAt.After(newest.ApprovedAt) {
			newest, found = approval, true
		}
	}
	return newest.Paths, found
}

// Approve records the human's decision for the reviewed content. It is written atomically, so a
// reader never sees a half-written approval.
func (r *ServiceSecretReview) Approve() error {
	if r == nil {
		return errors.New("nothing to approve")
	}
	root, err := serviceApprovalRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	// Everything hidden for this content is approved together: the review already excludes anything an
	// earlier approval of the same content covered, so re-approving adds the new files to it.
	paths := r.Paths()
	if prior, ok := ApprovedServiceSecrets(r.data); ok {
		paths = append(append([]string(nil), prior.Paths...), paths...)
		sort.Strings(paths)
	}
	approval := ServiceApproval{File: r.File, Workspace: r.workspace, Paths: paths, ApprovedAt: time.Now().UTC(), ApprovedBy: approverName()}
	raw, err := json.MarshalIndent(approval, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(root, composeDigest(r.data)+".json")
	tmp, err := os.CreateTemp(root, "approval-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), final)
}

func approverName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}
