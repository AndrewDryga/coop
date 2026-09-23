package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"gopkg.in/yaml.v3"
)

// ServiceStateRootEnv overrides where coop keeps the host-owned state behind sibling services —
// approvals and the decoy files sidecars mount. Honored only by test binaries, which otherwise get
// a directory under the system temp dir, so a test never reads or writes the developer's real state.
const ServiceStateRootEnv = "COOP_SERVICE_STATE_ROOT"

// ServiceApproval records a human's decision that the sibling services defined by one exact
// compose file may read the secret-looking files it binds — a generated dev TLS key for Keycloak,
// say — instead of the empty decoys serviceShadowOverride would hand them.
//
// The record is keyed by the private repository anchor, repository-relative Compose path and content
// digest. Approval of one repository's secret never grants the same Compose text in another.
// It lives on the host under ~/.local/state, where no box can write.
type ServiceApproval struct {
	Version       int                   `json:"version"`
	Anchor        string                `json:"anchor"`
	Digest        string                `json:"digest"`
	File          string                `json:"file"`              // repo-relative compose path at approval time
	Workspace     string                `json:"workspace"`         // path shown at approval time; identity is the private anchor
	Paths         []string              `json:"paths"`             // the secret-looking bind sources the approver saw
	Volumes       []ServiceVolumeAccess `json:"volumes,omitempty"` // actual external/custom volume capabilities
	VolumeBinding ServiceVolumeBinding  `json:"volume_binding,omitempty"`
	Images        map[string]string     `json:"images"` // local immutable image IDs for elevated consumers
	ApprovedAt    time.Time             `json:"approved_at"`
	ApprovedBy    string                `json:"approved_by"`
}

type ServiceVolumeBinding struct {
	Endpoint string                        `json:"endpoint,omitempty"`
	DaemonID string                        `json:"daemon_id,omitempty"`
	Objects  []runtime.NamedVolumeIdentity `json:"objects,omitempty"`
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

// ApprovedServiceSecrets reports whether this repository's exact Compose content was approved.
// Any read failure reads as "not approved": the decoys stay, which is the safe direction.
func ApprovedServiceSecrets(workspace, file string, data []byte) (ServiceApproval, bool) {
	_, rel, anchor, key, err := serviceApprovalScope(workspace, file, data, false)
	if err != nil {
		return ServiceApproval{}, false
	}
	root, err := serviceApprovalRoot()
	if err != nil {
		return ServiceApproval{}, false
	}
	raw, err := os.ReadFile(filepath.Join(root, key+".json"))
	if err != nil {
		return ServiceApproval{}, false
	}
	var approval ServiceApproval
	if err := json.Unmarshal(raw, &approval); err != nil || approval.Version != 5 || approval.Anchor != anchor ||
		approval.File != rel || approval.Digest != composeDigest(data) {
		return ServiceApproval{}, false
	}
	if len(approval.Images) == 0 || !validServiceImagePins(approval.Images) {
		return ServiceApproval{}, false
	}
	return approval, true
}

func serviceComposePath(workspace, file string) (canonical, rel string, err error) {
	canonical, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	rel, err = filepath.Rel(canonical, abs)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", "", fmt.Errorf("compose file %q is outside repository %q", file, workspace)
	}
	rel = filepath.ToSlash(rel)
	return canonical, rel, nil
}

func serviceApprovalScope(workspace, file string, data []byte, create bool) (canonical, rel, anchor, key string, err error) {
	canonical, rel, err = serviceComposePath(workspace, file)
	if err != nil {
		return "", "", "", "", err
	}
	anchor, err = serviceApprovalAnchor(canonical, create)
	if err != nil {
		return "", "", "", "", err
	}
	sum := sha256.Sum256([]byte("v5\x00" + anchor + "\x00" + rel + "\x00" + composeDigest(data)))
	return canonical, rel, anchor, hex.EncodeToString(sum[:]), nil
}

// ServiceSecretReview is what `coop up` puts in front of a human: the secret-looking files one
// compose file binds into its services, which stay decoys until Approve is called.
type ServiceSecretReview struct {
	File    string       // repo-relative compose path
	Files   []ReviewFile // secret files eligible for read-only approval
	Blocked []ReviewFile // sources kept hidden because a directory or writable bind cannot be approved

	workspace string
	data      []byte
	images    map[string]string
	imageRefs map[string]string
	previous  map[string]string
	renewal   bool
}

// ReviewFile is one hidden file, with the two things that tell a human whether to expect it: does
// the repo say its services need this file, and is it new since the last time they said yes.
type ReviewFile struct {
	Path        string
	Requested   bool   // .agent/project.yaml lists it under services.require_real_files
	New         bool   // this workspace approved this compose file before, and this path was not in it
	BlockReason string // non-empty when approval cannot safely reveal this source
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
	return reviewServiceSecretsData(workspace, file, data, false)
}

func reviewServiceSecretsData(workspace, file string, data []byte, force bool) (*ServiceSecretReview, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	decoys, hidden, err := serviceShadowPlan(workspace, abs, data)
	if err != nil {
		return nil, err
	}
	if len(hidden) == 0 {
		return nil, nil
	}
	blocked := map[string]string{}
	for _, list := range decoys {
		for _, d := range list {
			if d.dir {
				blocked[d.source] = "directory — bind individual files read-only to approve them"
			} else if d.writable && blocked[d.source] == "" {
				blocked[d.source] = "writable bind — change it to :ro (or read_only: true)"
			}
		}
	}
	canonical, rel, err := serviceComposePath(workspace, file)
	if err != nil {
		return nil, err
	}
	if approval, ok := ApprovedServiceSecrets(canonical, file, data); ok && !force {
		remaining := make([]string, 0, len(hidden))
		approved := make(map[string]bool, len(approval.Paths))
		for _, p := range approval.Paths {
			approved[p] = true
		}
		for _, p := range hidden {
			if !approved[p] || blocked[p] != "" {
				remaining = append(remaining, p)
			}
		}
		hidden = remaining
		if len(hidden) == 0 {
			return nil, nil
		}
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
	previous, hadPrevious := lastApprovalFor(canonical, rel)
	seen := make(map[string]bool, len(previous))
	for _, p := range previous {
		seen[p] = true
	}
	files := make([]ReviewFile, 0, len(hidden))
	var blockedFiles []ReviewFile
	for _, p := range hidden {
		item := ReviewFile{Path: p, Requested: requested[p], New: hadPrevious && !seen[p], BlockReason: blocked[p]}
		if item.BlockReason != "" {
			blockedFiles = append(blockedFiles, item)
		} else {
			files = append(files, item)
		}
	}
	// The order is the file's own, so a reader can follow the prompt against their Compose file.
	// What deserves a second look is carried by the LABELS under each path — "Not requested in
	// .agent/project.yaml", "Added since your last approval" — which a reordering could not make
	// louder and a reader cannot miss in a list this short.
	return &ServiceSecretReview{File: rel, Files: files, Blocked: blockedFiles, workspace: canonical, data: data}, nil
}

// lastApprovalFor returns the paths approved most recently for this workspace and compose path,
// across every version of that file. Approvals are keyed by content, so an edited compose file
// starts from nothing; this is what still lets the prompt say which of the paths are new.
func lastApprovalFor(workspace, file string) ([]string, bool) {
	anchor, err := serviceApprovalAnchor(workspace, false)
	if err != nil {
		return nil, false
	}
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
		if json.Unmarshal(raw, &approval) != nil || (approval.Version != 4 && approval.Version != 5) || approval.Anchor != anchor || approval.File != file {
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
	if r == nil || len(r.Files) == 0 {
		return errors.New("no read-only secret files to approve")
	}
	if !validServiceImagePins(r.images) {
		return errors.New("service images must be reviewed on Docker before approval — run 'coop up'")
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
	file := filepath.Join(r.workspace, filepath.FromSlash(r.File))
	if prior, ok := ApprovedServiceSecrets(r.workspace, file, r.data); ok && approvedServiceImagesMatch(prior.Images, r.images) {
		paths = append(append([]string(nil), prior.Paths...), paths...)
		sort.Strings(paths)
	}
	_, _, anchor, key, err := serviceApprovalScope(r.workspace, file, r.data, true)
	if err != nil {
		return err
	}
	approval := ServiceApproval{Version: 5, Anchor: anchor, Digest: composeDigest(r.data), File: r.File, Workspace: r.workspace, Paths: paths, ApprovedAt: time.Now().UTC(), ApprovedBy: approverName()}
	if prior, ok := ApprovedServiceSecrets(r.workspace, file, r.data); ok && approvedServiceImagesMatch(prior.Images, r.images) {
		approval.Volumes = prior.Volumes
		approval.VolumeBinding = prior.VolumeBinding
	}
	approval.Images, err = pinsForServiceGrant(r.workspace, file, r.data, approval, r.images)
	if err != nil {
		return err
	}
	return writeServiceApproval(root, key, approval)
}

func writeServiceApproval(root, key string, approval ServiceApproval) error {
	raw, err := json.MarshalIndent(approval, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(root, key+".json")
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

// ServiceStartReview freezes one validated Compose file for the terminal review and subsequent
// start. Volume permission is mandatory; declining secret files instead keeps their decoys.
type ServiceStartReview struct {
	Secrets              *ServiceSecretReview
	Volumes              []ServiceVolumeAccess
	VolumeApprovalNeeded bool
	NewVolumes           []string

	workspace string
	file      string
	data      []byte
	runtime   runtime.Runtime
	binding   ServiceVolumeBinding
	images    map[string]string
	imageRefs map[string]string
	previous  map[string]string
}

func (r *ServiceSecretReview) Renewal() bool                     { return r != nil && r.renewal }
func (r *ServiceSecretReview) Images() map[string]string         { return r.images }
func (r *ServiceSecretReview) ImageRefs() map[string]string      { return r.imageRefs }
func (r *ServiceSecretReview) PreviousImages() map[string]string { return r.previous }
func (r *ServiceStartReview) Images() map[string]string          { return r.images }
func (r *ServiceStartReview) ImageRefs() map[string]string       { return r.imageRefs }
func (r *ServiceStartReview) PreviousImages() map[string]string  { return r.previous }
func (r *ServiceStartReview) Renewal() bool                      { return r != nil && r.Secrets.Renewal() }

func (r *ServiceStartReview) Endpoint() string { return r.binding.Endpoint }
func (r *ServiceStartReview) DaemonID() string { return r.binding.DaemonID }
func (r *ServiceStartReview) VolumeIdentity(name string) (runtime.NamedVolumeIdentity, bool) {
	for _, identity := range r.binding.Objects {
		if identity.Name == name {
			return identity, true
		}
	}
	return runtime.NamedVolumeIdentity{}, false
}

func ReviewServiceStart(workspace, file string, rt runtime.Runtime, terminal bool) (*ServiceStartReview, error) {
	data, err := readValidatedCompose(file, workspace, false)
	if err != nil {
		return nil, err
	}
	bound, err := rt.FreezeCompose(context.Background())
	if err != nil {
		return nil, fmt.Errorf("inspect Docker before service review: %w", err)
	}
	volumes, err := outsideServiceVolumeAccess(data)
	if err != nil {
		return nil, err
	}
	binding, missing, err := inspectServiceVolumeBinding(context.Background(), bound, data, volumes)
	if err != nil {
		return nil, err
	}
	current, ok := ApprovedServiceSecrets(workspace, file, data)
	var images map[string]string
	if terminal {
		images, err = reviewServiceImages(context.Background(), bound, workspace, file, data, volumes)
		if err != nil {
			return nil, err
		}
	}
	imageRefs := map[string]string{}
	if len(images) > 0 {
		var doc composeDoc
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, err
		}
		for name := range images {
			imageRefs[name] = doc.Services[name].Image
		}
	}
	changed := terminal && ok && !approvedServiceImagesMatch(current.Images, images)
	secrets, err := reviewServiceSecretsData(workspace, file, data, changed)
	if err != nil {
		return nil, err
	}
	if secrets != nil {
		secrets.images, secrets.renewal = images, changed
		secrets.imageRefs, secrets.previous = imageRefs, current.Images
	}
	needed := len(volumes) > 0 && (!ok || changed || !sameServiceVolumes(current.Volumes, volumes) || !sameServiceVolumeBinding(current.VolumeBinding, binding))
	return &ServiceStartReview{Secrets: secrets, Volumes: volumes, VolumeApprovalNeeded: needed,
		NewVolumes: missing, workspace: workspace, file: file, data: data, runtime: bound, binding: binding,
		images: images, imageRefs: imageRefs, previous: current.Images}, nil
}

func sameServiceVolumeBinding(a, b ServiceVolumeBinding) bool {
	return a.Endpoint == b.Endpoint && a.DaemonID == b.DaemonID && slices.Equal(a.Objects, b.Objects)
}

func inspectServiceVolumeBinding(ctx context.Context, rt runtime.Runtime, data []byte, volumes []ServiceVolumeAccess) (ServiceVolumeBinding, []string, error) {
	if len(volumes) == 0 {
		return ServiceVolumeBinding{}, nil, nil
	}
	docker, err := runtime.InspectDocker(ctx, rt)
	if err != nil {
		return ServiceVolumeBinding{}, nil, fmt.Errorf("inspect Docker before volume review: %w", err)
	}
	defer docker.Close()
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ServiceVolumeBinding{}, nil, err
	}
	creatable := map[string]bool{}
	external := map[string]bool{}
	for logical, declaration := range doc.Volumes {
		actual := declaration.Name
		if actual == "" {
			actual = logical
		}
		if declaration.External {
			external[actual] = true
		} else if declaration.Name != "" {
			creatable[actual] = true
		}
	}
	binding := ServiceVolumeBinding{Endpoint: docker.Endpoint(), DaemonID: docker.Info().ID}
	var missing []string
	for _, volume := range volumes {
		identity, present, err := docker.InspectNamedVolume(ctx, volume.Name)
		if err != nil {
			return ServiceVolumeBinding{}, nil, fmt.Errorf("inspect Docker volume %q: %w", volume.Name, err)
		}
		if !present {
			if !creatable[volume.Name] || external[volume.Name] {
				return ServiceVolumeBinding{}, nil, fmt.Errorf("external Docker volume %q does not exist on this daemon", volume.Name)
			}
			missing = append(missing, volume.Name)
			continue
		}
		binding.Objects = append(binding.Objects, identity)
	}
	return binding, missing, nil
}

func sameServiceVolumes(a, b []ServiceVolumeAccess) bool {
	return slices.EqualFunc(a, b, func(x, y ServiceVolumeAccess) bool {
		return x.Name == y.Name && x.Writable == y.Writable && slices.Equal(x.Consumers, y.Consumers)
	})
}

func (r *ServiceStartReview) ApproveVolumes() error {
	if r == nil || len(r.Volumes) == 0 {
		return errors.New("no external volumes to approve")
	}
	if err := r.prepareVolumes(); err != nil {
		return err
	}
	canonical, rel, anchor, key, err := serviceApprovalScope(r.workspace, r.file, r.data, true)
	if err != nil {
		return err
	}
	approval := ServiceApproval{Version: 5, Anchor: anchor, Digest: composeDigest(r.data), File: rel,
		Workspace: canonical, Volumes: r.Volumes, VolumeBinding: r.binding, ApprovedAt: time.Now().UTC(), ApprovedBy: approverName()}
	if prior, ok := ApprovedServiceSecrets(r.workspace, r.file, r.data); ok && approvedServiceImagesMatch(prior.Images, r.images) {
		approval.Paths = prior.Paths
	}
	approval.Images, err = pinsForServiceGrant(r.workspace, r.file, r.data, approval, r.images)
	if err != nil {
		return err
	}
	root, err := serviceApprovalRoot()
	if err != nil {
		return err
	}
	if err := writeServiceApproval(root, key, approval); err != nil {
		return err
	}
	r.VolumeApprovalNeeded = false
	return nil
}

// ApproveRenewal publishes both answers in one record after the terminal has
// collected them. A declined second prompt therefore cannot replace the old grant.
func (r *ServiceStartReview) ApproveRenewal() error {
	if !r.Renewal() || !validServiceImagePins(r.images) {
		return errors.New("no changed service image to approve")
	}
	if len(r.Volumes) > 0 {
		if err := r.prepareVolumes(); err != nil {
			return err
		}
	}
	canonical, rel, anchor, key, err := serviceApprovalScope(r.workspace, r.file, r.data, true)
	if err != nil {
		return err
	}
	approval := ServiceApproval{Version: 5, Anchor: anchor, Digest: composeDigest(r.data), File: rel,
		Workspace: canonical, Paths: r.Secrets.Paths(), Volumes: r.Volumes, VolumeBinding: r.binding,
		ApprovedAt: time.Now().UTC(), ApprovedBy: approverName()}
	approval.Images, err = pinsForServiceGrant(r.workspace, r.file, r.data, approval, r.images)
	if err != nil {
		return err
	}
	root, err := serviceApprovalRoot()
	if err != nil {
		return err
	}
	if err := writeServiceApproval(root, key, approval); err != nil {
		return err
	}
	r.VolumeApprovalNeeded = false
	return nil
}

func (r *ServiceStartReview) prepareVolumes() error {
	current, missing, err := inspectServiceVolumeBinding(context.Background(), r.runtime, r.data, r.Volumes)
	if err != nil || !sameServiceVolumeBinding(current, r.binding) || !slices.Equal(missing, r.NewVolumes) {
		return errors.Join(err, errors.New("docker daemon or volume changed after review — run 'coop up' again"))
	}
	if len(missing) > 0 {
		docker, err := runtime.InspectDocker(context.Background(), r.runtime)
		if err != nil || docker.Endpoint() != r.binding.Endpoint || docker.Info().ID != r.binding.DaemonID {
			if docker != nil {
				_ = docker.Close()
			}
			return errors.Join(err, errors.New("docker daemon changed during volume approval"))
		}
		for _, name := range missing {
			if _, err := docker.CreatePlainNamedVolume(context.Background(), name); err != nil {
				docker.Close()
				return fmt.Errorf("create approved plain Docker volume %q: %w", name, err)
			}
		}
		docker.Close()
		r.binding, r.NewVolumes, err = inspectServiceVolumeBinding(context.Background(), r.runtime, r.data, r.Volumes)
		if err != nil || len(r.NewVolumes) != 0 {
			return errors.Join(err, errors.New("approved Docker volumes could not be verified"))
		}
	}
	return nil
}

func (r *ServiceStartReview) verify(ctx context.Context, rt runtime.Runtime, workspace, file string, data []byte) error {
	if r == nil || !bytes.Equal(data, r.data) {
		return errors.New("compose file changed after review — run 'coop up' again")
	}
	canonical, rel, err := serviceComposePath(workspace, file)
	if err != nil {
		return err
	}
	reviewCanonical, reviewRel, err := serviceComposePath(r.workspace, r.file)
	if err != nil || canonical != reviewCanonical || rel != reviewRel {
		return errors.New("compose repository or path changed after review")
	}
	volumes, err := outsideServiceVolumeAccess(data)
	if err != nil {
		return err
	}
	if !sameServiceVolumes(volumes, r.Volumes) {
		return errors.New("compose volumes changed after review")
	}
	approval, ok := ApprovedServiceSecrets(workspace, file, data)
	if ok && r.images != nil && !approvedServiceImagesMatch(approval.Images, r.images) {
		return errors.New("service image approval changed after review — run 'coop up' again")
	}
	if len(volumes) > 0 {
		if r.VolumeApprovalNeeded {
			return errors.New("external Docker volumes require approval in a terminal")
		}
		binding, missing, err := inspectServiceVolumeBinding(ctx, rt, data, volumes)
		if err != nil || len(missing) != 0 || !sameServiceVolumeBinding(binding, r.binding) {
			return errors.Join(err, errors.New("docker daemon or volume changed after review — run 'coop up' again"))
		}
		if !ok || !sameServiceVolumes(approval.Volumes, volumes) || !sameServiceVolumeBinding(approval.VolumeBinding, binding) {
			return errors.New("external Docker volume approval is missing or changed")
		}
	}
	return nil
}

func approverName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}
