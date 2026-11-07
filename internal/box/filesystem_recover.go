package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

func lockFilesystemAdmission(dir string, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Open(filepath.Join(dir, "lease"), flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "filesystem admission lease")
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("filesystem lease is not a regular file")
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (a *filesystemAdmission) releaseLease() {
	a.controller.closeLease()
	if a.lease != nil {
		a.lease.Close()
		a.lease = nil
	}
}

func readFilesystemRecord(dir string) (filesystemRecord, error) {
	var record filesystemRecord
	root, err := safefile.OpenRoot(dir)
	if err != nil {
		return record, err
	}
	defer root.Close()
	data, err := safefile.ReadRegular(root, "record.json", 1<<20)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return record, errors.New("unexpected filesystem record data")
	}
	if err := record.validate(); err != nil {
		return record, err
	}
	return record, nil
}

func (r filesystemRecord) validate() error {
	if r.Version != 1 || !fsfilter.ValidParent(r.Parent) || r.Endpoint == "" || r.DaemonID == "" || !filepath.IsAbs(r.Workspace) || len(r.Consumers) > 512 {
		return errors.New("invalid filesystem custody record")
	}
	supervisor, ok := parseSupervisorLabel(r.Supervisor)
	// A scope is the workspace's real path, which a path spelled through a link no longer resolves
	// to once the workspace is gone. Such a record must still be readable, or it could never retire.
	if !ok || supervisor.scope != workspaceScope(r.Workspace) && !filesystemWorkspaceGone(r.Workspace) {
		return errors.New("invalid filesystem supervisor")
	}
	validID := func(id string) bool { return len(id) == 64 && strings.Trim(id, "0123456789abcdef") == "" }
	if r.Image != "" && (!strings.HasPrefix(r.Image, "sha256:") || !validID(strings.TrimPrefix(r.Image, "sha256:"))) {
		return errors.New("invalid filesystem helper image")
	}
	if r.Helper.Name != r.Parent+"-owner" || r.Helper.Labels[LabelKey] != "filesystem" || r.Helper.Labels[filesystemLabel] != r.Parent || r.Helper.ID != "" && !validID(r.Helper.ID) {
		return errors.New("invalid filesystem helper identity")
	}
	names := map[string]bool{r.Helper.Name: true, r.Parent + "-recovery": true}
	for _, ref := range r.Consumers {
		if ref.Name == "" || names[ref.Name] || ref.Labels[filesystemLabel] != r.Parent || ref.ID != "" && !validID(ref.ID) {
			return errors.New("invalid filesystem consumer identity")
		}
		names[ref.Name] = true
	}
	if r.Pending != "" && !names[r.Pending] {
		return errors.New("unknown filesystem creation intent")
	}
	if len(r.Probes) > 32 {
		return errors.New("too many filesystem probes")
	}
	for _, probe := range r.Probes {
		if !probe.valid() {
			return errors.New("invalid filesystem probe record")
		}
	}
	if r.Image != "" && r.Controller == nil {
		return errFilesystemRecordBeforeController
	}
	if r.Controller != nil {
		if err := r.Controller.validate(); err != nil {
			return err
		}
		if r.Controller.Ready == nil || r.Controller.Endpoint != r.Endpoint || r.Controller.DaemonID != r.DaemonID || r.Controller.Image != r.Image {
			return errors.New("filesystem admission controller differs")
		}
	}
	if r.Ready != nil {
		if !r.matchesController(*r.Ready) {
			return errors.New("filesystem admission belongs to another controller")
		}
		return r.Ready.Validate(r.Parent)
	}
	return nil
}

// filesystemWorkspaceGone reports whether workspace no longer exists. Only that answer counts: a
// path that cannot be checked may still be a workspace in use.
func filesystemWorkspaceGone(workspace string) bool {
	_, err := os.Stat(workspace)
	return errors.Is(err, fs.ErrNotExist)
}

// pendingCreateSettled reports whether a create that a custody record in dir names as pending has
// had time to land. Docker finishes a create whose client gave up, so its absence proves nothing at
// first. The record was last written about when its owner stopped waiting, which dates the create.
func pendingCreateSettled(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "record.json"))
	return err == nil && time.Since(info.ModTime()) >= unfinishedCreateSettle
}

// RecoverFilesystemAdmissions retires only this host's provably orphaned
// admissions. Service protection remains until every restartable consumer is
// absent; a successful process exit alone never authorizes removing it.
//
// A workspace selects its own admissions. An empty one selects what no workspace's recovery can
// reach, the admissions whose workspace no longer exists: a host setup check's scratch folder, or a
// deleted worktree. The helper of a workspace that still exists is never that pass's to touch.
func RecoverFilesystemAdmissions(ctx context.Context, rt runtime.Runtime, workspace string) (int, error) {
	return recoverFilesystem(ctx, rt, workspace, recoverFilesystemAdmissions)
}

// recoverFilesystem is RecoverFilesystemAdmissions with its admissions pass passed in, so a test can
// retire an admission that binds a controller without running that controller's cleanup.
func recoverFilesystem(ctx context.Context, rt runtime.Runtime, workspace string, admissionsPass func(context.Context, runtime.Runtime, string) (int, error)) (int, error) {
	// A controller that no admission bound when a workspace's pass began is not that workspace's.
	// Its recovery is still tried, so it goes once its recorded Docker answers again, but only the
	// host-wide pass returns why it could not. That Docker may be gone for good (one that is no longer
	// running), and every project's service start and session cleanup would otherwise fail over it.
	// The snapshot comes first: a controller whose last admission this pass retires is this
	// workspace's. When admissions cannot all be read, the pass reports every controller, as before.
	var report func(string) bool
	if workspace != "" {
		if used, err := referencedControllers(); err == nil {
			report = func(name string) bool { return used[name] }
		}
	}
	admissions, err := admissionsPass(ctx, rt, workspace)
	controllers, controllerErr := recoverFilesystemControllers(ctx, rt, report)
	orphans := 0
	var orphanErr error
	if workspace == "" && rt.SupportsRestrictedFilesystem() {
		orphans, orphanErr = sweepOrphanControllers(ctx, rt)
	}
	return admissions + controllers + orphans, errors.Join(err, controllerErr, orphanErr)
}

func recoverFilesystemAdmissions(ctx context.Context, rt runtime.Runtime, workspace string) (int, error) {
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		return 0, err
	}
	state, err = filepath.EvalSymlinks(state)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(state)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	host, err := thisHost()
	if err != nil {
		return 0, err
	}
	var problems []error
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(errors.Join(problems...), err)
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "coop-fs-") {
			continue
		}
		dir := filepath.Join(state, entry.Name())
		lease, err := lockFilesystemAdmission(dir, false)
		if errors.Is(err, unix.EWOULDBLOCK) || os.IsNotExist(err) {
			continue
		}
		if err == nil {
			var done bool
			done, err = recoverFilesystemAdmission(ctx, rt, dir, lease, workspace, host)
			lease.Close()
			if done {
				removed++
			}
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("filesystem custody retained at %s: %w", dir, err))
		}
	}
	return removed, errors.Join(problems...)
}

func recoverFilesystemAdmission(ctx context.Context, rt runtime.Runtime, dir string, lease *os.File, workspace, host string) (bool, error) {
	record, err := readFilesystemRecord(dir)
	if os.IsNotExist(err) {
		return false, nil // another recovery completed before lease acquisition
	}
	if err != nil {
		return false, err
	}
	supervisor, _ := parseSupervisorLabel(record.Supervisor)
	selected := workspace != "" && workspaceScope(workspace) == supervisor.scope || workspace == "" && filesystemWorkspaceGone(record.Workspace)
	if !selected || supervisor.origin(host) != originHere || !record.Services && !supervisorProvablyDead(supervisor, host) {
		return false, nil
	}
	// Recovery must start a release-owned cleanup helper to reconcile VM pins.
	// The fixed endpoint AND recorded daemon identity remain mandatory.
	docker, err := runtime.BindDockerForSetup(ctx, rt, record.Endpoint, record.DaemonID)
	if err != nil {
		return false, err
	}
	defer docker.Close()
	if record.Services {
		for _, ref := range record.Consumers {
			_, present, err := docker.InspectContainer(ctx, ref)
			if err != nil || present {
				return false, err
			}
		}
	}
	a := &filesystemAdmission{docker: docker, dir: dir, record: record, lease: lease}
	if record.Controller != nil {
		a.controller, err = leaseRecordedFilesystemController(ctx, docker, *record.Controller, false)
		if err != nil {
			return false, err
		}
	}
	err = a.closeContext(ctx)
	return err == nil, err
}
