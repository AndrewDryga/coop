package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/fsimage"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

var errFilesystemControllerStopped = errors.New("filesystem controller is no longer running")

// A generation is never restarted. Its pinned root continues denying after
// process death until every admission and restartable consumer is retired.
type filesystemControllerRecord struct {
	Version                                    int
	Name, Directory, Endpoint, DaemonID, Image string
	Container                                  runtime.DockerRef
	Pending                                    string
	StartedAt                                  time.Time
	Ready                                      *fsfilter.ControllerReady
	// CgroupDriver is "systemd" when the controller and its admissions are systemd slices,
	// empty for plain cgroups (every record before slices were supported).
	CgroupDriver string `json:",omitempty"`
	// AppArmor is the helper profile the controller started under on systemd slices (see
	// filesystemControllerRecord.appArmorOption), empty when unconfined; a controller is reused only
	// under the same one.
	AppArmor string `json:",omitempty"`
}

func (r filesystemControllerRecord) systemd() bool { return r.CgroupDriver == "systemd" }

// appArmorOption is the --security-opt value its helpers run under on systemd slices.
func (r filesystemControllerRecord) appArmorOption() string {
	if r.AppArmor != "" {
		return "apparmor=" + r.AppArmor
	}
	return "apparmor=unconfined"
}

// cgroupParent is the controller's own cgroup as Docker names a container's parent.
func (r filesystemControllerRecord) cgroupParent() string {
	if r.systemd() {
		return fsfilter.ControllerSlice(r.Name)
	}
	return "/" + r.Name
}

type filesystemController struct {
	record filesystemControllerRecord
	dir    string
	lease  *os.File
}

func filesystemControllerState() (string, error) {
	state, err := serviceStateRoot("filesystem-controllers")
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(state)
}

// Registry -> generation is the only lock order. Running admissions hold only
// the generation's shared lease; independent policies publish concurrently.
func filesystemControllerLock(ctx context.Context, path string, exclusive, wait bool) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		file.Close()
		return nil, errors.Join(errors.New("invalid filesystem controller lease"), err)
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(fd, mode|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !wait || !errors.Is(err, unix.EWOULDBLOCK) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r filesystemControllerRecord) validate() error {
	validID := func(s string) bool { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
	if r.Version != 1 || !fsfilter.ValidController(r.Name) || filepath.Base(r.Directory) != r.Directory || !strings.HasPrefix(r.Directory, r.Name+"-") || strings.Trim(r.Directory, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || r.Endpoint == "" || r.DaemonID == "" || !strings.HasPrefix(r.Image, "sha256:") || !validID(strings.TrimPrefix(r.Image, "sha256:")) {
		return errors.New("invalid filesystem controller record")
	}
	expected := filesystemRef(r.Name, "controller")
	if r.Container.Name != expected.Name || r.Container.Labels[LabelKey] != "filesystem" || r.Container.Labels[filesystemLabel] != r.Name || r.Container.Labels["coop.filesystem.role"] != "controller" || (r.Container.ID != "" && !validID(r.Container.ID)) {
		return errors.New("invalid filesystem controller container")
	}
	if r.CgroupDriver != "" && r.CgroupDriver != "systemd" {
		return errors.New("invalid filesystem controller cgroup driver")
	}
	if r.AppArmor != "" && (r.CgroupDriver != "systemd" || !strings.HasPrefix(r.AppArmor, fsfilter.AppArmorProfilePrefix)) {
		return errors.New("invalid filesystem controller AppArmor profile")
	}
	if r.Pending != "" && r.Pending != r.Container.Name && r.Pending != r.Name+"-recovery" {
		return errors.New("invalid controller creation intent")
	}
	if r.Ready != nil {
		if err := r.Ready.Validate(); err != nil {
			return err
		}
		if r.Ready.Birth.Root != r.Name || r.StartedAt.IsZero() || r.Container.ID == "" {
			return errors.New("invalid filesystem controller readiness")
		}
	}
	return nil
}

func readFilesystemController(dir string) (filesystemControllerRecord, error) {
	var r filesystemControllerRecord
	root, err := safefile.OpenRoot(dir)
	if err != nil {
		return r, err
	}
	defer root.Close()
	data, err := safefile.ReadRegular(root, "record.json", 1<<20)
	if err != nil {
		return r, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&r); err != nil {
		return r, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return r, errors.New("unexpected controller record data")
	}
	if filepath.Base(dir) != r.Directory {
		return r, errors.New("controller directory differs from record")
	}
	return r, r.validate()
}

func (c *filesystemController) persist() error {
	if err := filesystemJSON(filepath.Join(c.dir, "record.json"), c.record); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(c.dir))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
func (c *filesystemController) closeLease() {
	if c != nil && c.lease != nil {
		c.lease.Close()
		c.lease = nil
	}
}

func controllerLive(ctx context.Context, docker *runtime.Docker, r filesystemControllerRecord) (bool, error) {
	if err := r.validate(); err != nil {
		return false, err
	}
	if r.Ready == nil || r.Endpoint != docker.Endpoint() || r.DaemonID != docker.Info().ID {
		return false, nil
	}
	value, present, err := docker.InspectContainer(ctx, r.Container)
	if err != nil {
		return false, err
	}
	return present && value.Image == r.Image && value.State.Running && !value.State.Paused && value.RestartCount == 0 && value.State.StartedAt.Equal(r.StartedAt), nil
}

func acquireFilesystemController(ctx context.Context, docker *runtime.Docker, image string, roots []string) (*filesystemController, error) {
	driver, err := filesystemCgroups(docker.Info())
	if err != nil {
		return nil, errors.Join(errFilesystemUnsupported, err)
	}
	profile := ""
	if driver == "systemd" && daemonAppArmor(docker.Info()) && appArmorProfileLoaded() {
		profile = fsfilter.AppArmorProfile
	}
	state, err := filesystemControllerState()
	if err != nil {
		return nil, err
	}
	if err := privateFilesystemControllerState(state, roots); err != nil {
		return nil, err
	}
	registry, err := filesystemControllerLock(ctx, filepath.Join(state, "registry"), true, true)
	if err != nil {
		return nil, err
	}
	defer registry.Close()
	entries, err := os.ReadDir(state)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "coop-fs-controller-") {
			continue
		}
		dir := filepath.Join(state, entry.Name())
		r, err := readFilesystemController(dir)
		if err != nil {
			return nil, err
		}
		if r.Image != image || r.Endpoint != docker.Endpoint() || r.DaemonID != docker.Info().ID || r.CgroupDriver != driver || r.AppArmor != profile {
			continue
		}
		live, err := controllerLive(ctx, docker, r)
		if err != nil {
			return nil, err
		}
		if !live {
			continue
		}
		lease, err := filesystemControllerLock(ctx, filepath.Join(dir, "lease"), false, false)
		if err != nil {
			return nil, err
		}
		return &filesystemController{record: r, dir: dir, lease: lease}, nil
	}
	name := "coop-fs-controller-" + randid.Hex(16)
	dir, err := privateTempDirUnder(state, roots[0], name+"-", roots[1:]...)
	if err != nil {
		return nil, err
	}
	c := &filesystemController{dir: dir, record: filesystemControllerRecord{Version: 1, Name: name, Directory: filepath.Base(dir), Endpoint: docker.Endpoint(), DaemonID: docker.Info().ID, Image: image, Container: filesystemRef(name, "controller"), CgroupDriver: driver, AppArmor: profile}}
	c.lease, err = filesystemControllerLock(ctx, filepath.Join(dir, "lease"), false, false)
	if err != nil {
		return nil, err
	}
	if err = c.persist(); err == nil {
		err = c.start(ctx, docker)
	}
	if err != nil {
		c.closeLease()
		return nil, fmt.Errorf("filesystem controller custody retained at %s: %w", dir, err)
	}
	return c, nil
}

func (c *filesystemController) create(ctx context.Context, docker *runtime.Docker, spec runtime.DockerCreate) (string, error) {
	if c.record.Pending != "" {
		return "", errors.New("controller creation needs reconciliation")
	}
	c.record.Pending = spec.Ref.Name
	if err := c.persist(); err != nil {
		return "", err
	}
	id, err := docker.CreateContainer(ctx, spec)
	if id != "" || runtime.DockerCreateLeftNothing(err) {
		c.record.Pending = ""
		if spec.Ref.Name == c.record.Container.Name {
			c.record.Container.ID = id
		}
		err = errors.Join(err, c.persist())
	}
	return id, err
}

func (c *filesystemController) start(ctx context.Context, docker *runtime.Docker) error {
	options := (&filesystemAdmission{dir: c.dir}).helperOptions(false, c.record)
	_, err := c.create(ctx, docker, runtime.DockerCreate{Ref: c.record.Container, Image: c.record.Image, Options: options, Command: []string{"controller", c.record.Name}})
	if err != nil {
		return err
	}
	if err = docker.StartContainer(ctx, c.record.Container); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, data, err := docker.ObserveContainerLogs(ctx, c.record.Container, 30, 1<<20)
		if err != nil {
			return err
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var ready fsfilter.ControllerReady
			if json.Unmarshal(line, &ready) == nil && ready.Validate() == nil && ready.Birth.Root == c.record.Name {
				if !value.State.Running || value.State.Paused || value.RestartCount != 0 || value.State.StartedAt.IsZero() || value.Image != c.record.Image {
					return errors.New("filesystem controller exited during readiness")
				}
				c.record.Ready = &ready
				c.record.StartedAt = value.State.StartedAt
				if err = filesystemJSON(filepath.Join(c.dir, "ready.json"), ready); err != nil {
					return err
				}
				return c.persist()
			}
		}
		if !value.State.Running {
			final, logErr := docker.ContainerLogs(ctx, c.record.Container, 30, 1<<20)
			return errors.Join(fmt.Errorf("filesystem controller exited with status %d before readiness: %s", value.State.ExitCode, strings.TrimSpace(string(final))), logErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func leaseRecordedFilesystemController(ctx context.Context, docker *runtime.Docker, r filesystemControllerRecord, requireLive bool, roots ...string) (*filesystemController, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	state, err := filesystemControllerState()
	if err != nil {
		return nil, err
	}
	if err := privateFilesystemControllerState(state, roots); err != nil {
		return nil, err
	}
	registry, err := filesystemControllerLock(ctx, filepath.Join(state, "registry"), false, true)
	if err != nil {
		return nil, err
	}
	defer registry.Close()
	dir := filepath.Join(state, r.Directory)
	current, err := readFilesystemController(dir)
	if err != nil {
		return nil, err
	}
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(current)
	if !bytes.Equal(a, b) {
		return nil, errors.New("filesystem controller identity changed")
	}
	lease, err := filesystemControllerLock(ctx, filepath.Join(dir, "lease"), false, false)
	if err != nil {
		return nil, err
	}
	c := &filesystemController{dir: dir, record: current, lease: lease}
	if requireLive {
		live, err := controllerLive(ctx, docker, current)
		if err != nil {
			c.closeLease()
			return nil, err
		}
		if !live {
			c.closeLease()
			return nil, errFilesystemControllerStopped
		}
	}
	return c, nil
}

func (r filesystemRecord) cgroupParent() string {
	if r.Controller != nil && r.Controller.systemd() {
		return fsfilter.AdmissionSlice(r.Controller.Name, r.Parent)
	}
	if r.Controller != nil {
		return "/" + r.Controller.Name + "/" + r.Parent
	}
	return "/" + r.Parent
}
func (r filesystemRecord) matchesController(ready fsfilter.Ready) bool {
	return r.Controller != nil && r.Controller.Ready != nil && ready.Controller == r.Controller.Name && ready.BootID == r.Controller.Ready.Birth.BootID
}

// The owner has already exited and all consumers are absent. The warm
// controller can run the same idempotent retirement command as a recovery box.
func (a *filesystemAdmission) retireSharedPolicy(ctx context.Context) error {
	var birth fsfilter.RunBirth
	root, err := safefile.OpenRoot(a.dir)
	if err != nil {
		return err
	}
	data, readErr := safefile.ReadRegular(root, "birth.json", 1<<20)
	root.Close()
	command := []string{"abandon", a.record.Controller.Name, a.record.Parent}
	if readErr == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&birth); err != nil {
			return err
		}
		if decoder.Decode(new(any)) != io.EOF {
			return errors.New("unexpected admission birth data")
		}
		if err = birth.Validate(a.record.Parent, a.record.Controller.Name); err != nil {
			return err
		}
		command = []string{"release-run", "/state/birth.json", "/state/controller.json"}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	} else if a.record.Ready != nil {
		return errors.New("published filesystem admission is missing its birth receipt")
	}
	live, err := controllerLive(ctx, a.docker, *a.record.Controller)
	if err != nil {
		return err
	}
	if live {
		argv := command
		if readErr == nil {
			b, err := json.Marshal(birth)
			if err != nil {
				return err
			}
			c, err := json.Marshal(a.record.Controller.Ready)
			if err != nil {
				return err
			}
			argv = []string{"retire-run", string(b), string(c)}
		}
		if err = a.docker.ExecApply(ctx, a.record.Controller.Container, append([]string{"/usr/local/bin/coop-fs"}, argv...)...); err == nil {
			return nil
		}
		// An exec can time out after committing. Retry the exact idempotent receipt
		// with an independently supervised helper; never interpret timeout as success.
	}
	ref := filesystemRef(a.record.Parent, "recovery")
	if err := a.remove(ctx, ref); err != nil {
		return err
	}
	id, err := a.create(ctx, runtime.DockerCreate{Ref: ref, Image: a.record.Image, Options: a.helperOptions(true, *a.record.Controller), Command: command})
	ref.ID = id
	if err != nil {
		return err
	}
	var output bytes.Buffer
	code, err := a.docker.StartAttached(ctx, ref, nil, &output, &output, nil)
	if code != 0 || err != nil {
		return errors.Join(fmt.Errorf("filesystem cleanup failed: %s", output.String()), err)
	}
	return a.docker.RemoveContainer(ctx, ref)
}

func (c *filesystemController) remove(ctx context.Context, docker *runtime.Docker, ref runtime.DockerRef) error {
	value, present, err := docker.InspectContainer(ctx, ref)
	if err != nil {
		return err
	}
	if !present {
		if c.record.Pending != ref.Name {
			return nil
		}
		if !pendingCreateSettled(c.dir) {
			return errors.New("controller creation outcome remains unknown")
		}
		c.record.Pending = ""
		return c.persist()
	}
	ref.ID = value.ID
	if c.record.Pending == ref.Name {
		c.record.Pending = ""
		if ref.Name == c.record.Container.Name {
			c.record.Container.ID = ref.ID
		}
		if err = c.persist(); err != nil {
			return err
		}
	}
	return docker.RemoveContainer(ctx, ref)
}

// controllerReferenced reports whether an admission binds the controller. A readable record that
// binds it is enough even when another record cannot be read: either answer keeps it from recovery.
func controllerReferenced(name string) (bool, error) {
	used, err := referencedControllers()
	if used[name] {
		return true, nil
	}
	return false, err
}

// referencedControllers names the controllers that admission records bind. A record it cannot read
// is the error, returned with the names from every record it could read.
func referencedControllers() (map[string]bool, error) {
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(state)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	var unreadable error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "coop-fs-") {
			continue
		}
		r, err := readFilesystemRecord(filepath.Join(state, entry.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			if unreadable == nil {
				unreadable = err
			}
			continue
		}
		if r.Controller != nil {
			used[r.Controller.Name] = true
		}
	}
	return used, unreadable
}

// controllerOfDirectory is the controller whose state a folder holds: the folder is the controller's
// name, a "-" and a random suffix. "" for a folder of any other shape.
func controllerOfDirectory(dir string) string {
	const length = len("coop-fs-controller-") + 32
	if len(dir) > length && dir[length] == '-' && fsfilter.ValidController(dir[:length]) {
		return dir[:length]
	}
	return ""
}

// An exclusive generation lease excludes enrollment, active runs and their
// cleanup. Durable service records additionally protect generations after their
// host process has released that lease. Never lock admissions under registry.
//
// Every controller's recovery is tried; report picks the controllers whose failures are returned
// (nil: all of them).
func recoverFilesystemControllers(ctx context.Context, rt runtime.Runtime, report func(name string) bool) (int, error) {
	state, err := serviceStateRoot("filesystem-controllers")
	if err != nil {
		return 0, err
	}
	if _, err = os.Stat(state); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	registry, err := filesystemControllerLock(ctx, filepath.Join(state, "registry"), true, true)
	if err != nil {
		return 0, err
	}
	defer registry.Close()
	entries, err := os.ReadDir(state)
	if err != nil {
		return 0, err
	}
	count := 0
	var problems []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "coop-fs-controller-") {
			continue
		}
		dir := filepath.Join(state, entry.Name())
		reported := report == nil || report(controllerOfDirectory(entry.Name()))
		lease, err := filesystemControllerLock(ctx, filepath.Join(dir, "lease"), true, false)
		if errors.Is(err, unix.EWOULDBLOCK) {
			continue
		}
		if err != nil {
			if reported {
				problems = append(problems, fmt.Errorf("filesystem controller custody retained at %s: %w", dir, err))
			}
			continue
		}
		removed, err := recoverFilesystemController(ctx, rt, dir)
		lease.Close()
		if removed {
			count++
		}
		if err != nil && reported {
			problems = append(problems, fmt.Errorf("filesystem controller custody retained at %s: %w", dir, err))
		}
	}
	return count, errors.Join(problems...)
}

func recoverFilesystemController(ctx context.Context, rt runtime.Runtime, dir string) (bool, error) {
	r, err := readFilesystemController(dir)
	if err != nil {
		return false, err
	}
	referenced, err := controllerReferenced(r.Name)
	if err != nil || referenced {
		return false, err
	}
	docker, err := runtime.BindDockerForSetup(ctx, rt, r.Endpoint, r.DaemonID)
	if err != nil {
		return false, err
	}
	defer docker.Close()
	live, err := controllerLive(ctx, docker, r)
	if err != nil {
		return false, err
	}
	if live {
		image, labels, err := docker.Image(ctx, fsimage.Tag())
		if err != nil && !errors.Is(err, runtime.ErrDockerImageMissing) {
			return false, err
		}
		if err == nil && image == r.Image && labels[fsimage.BuildLabel] == fsimage.Fingerprint() {
			return false, nil
		}
	}
	if err = docker.CgroupParentUnused(ctx, r.cgroupParent()); err != nil {
		return false, err
	}
	c := &filesystemController{record: r, dir: dir}
	if err = c.remove(ctx, docker, r.Container); err != nil {
		return false, err
	}
	ref := filesystemRef(r.Name, "recovery")
	if err = c.remove(ctx, docker, ref); err != nil {
		return false, err
	}
	command := []string{"abandon", r.Name}
	if _, err = os.Lstat(filepath.Join(dir, "birth.json")); err == nil {
		command = []string{"release-controller", "/state/birth.json", "/state/ready.json"}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	options := (&filesystemAdmission{dir: dir}).helperOptions(true, r)
	id, err := c.create(ctx, docker, runtime.DockerCreate{Ref: ref, Image: r.Image, Options: options, Command: command})
	ref.ID = id
	if err != nil {
		return false, err
	}
	var output bytes.Buffer
	code, err := docker.StartAttached(ctx, ref, nil, &output, &output, nil)
	if code != 0 || err != nil {
		return false, errors.Join(fmt.Errorf("controller cleanup failed: %s", output.String()), err)
	}
	if err = docker.RemoveContainer(ctx, ref); err != nil {
		return false, err
	}
	if err = os.RemoveAll(dir); err != nil {
		return false, err
	}
	return true, nil
}

func privateFilesystemControllerState(state string, roots []string) error {
	for _, root := range roots {
		inside, err := futurePathWithin(root, state)
		if err != nil {
			return err
		}
		if inside {
			return &ExposedTempDirError{Dir: state}
		}
	}
	return nil
}

const controllerStateFolder = "/filesystem-controllers/coop-fs-controller-"

// orphanControllers picks the filesystem controllers a killed process left: each binds its own state
// folder, and when that folder is gone nothing can lease or stop the controller again. One is chosen
// only when no other container binds anything under the same state root, so a box still enforced by
// it keeps it. A controller bound to an existing folder is never chosen.
func orphanControllers(controllers, all []ContainerBindsView, exists func(string) bool) []string {
	var orphans []string
	for _, c := range controllers {
		state := ""
		for _, bind := range c.Binds {
			if strings.Contains(bind, controllerStateFolder) {
				state = bind
			}
		}
		if state == "" || exists(state) {
			continue
		}
		root := state[:strings.Index(state, controllerStateFolder)]
		used := false
		for _, other := range all {
			if other.ID == c.ID {
				continue
			}
			for _, bind := range other.Binds {
				if bind == root || strings.HasPrefix(bind, root+"/") {
					used = true
				}
			}
		}
		if !used {
			orphans = append(orphans, c.ID)
		}
	}
	return orphans
}

// ContainerBindsView is runtime.ContainerBinds, kept here so the choice can be tested without Docker.
type ContainerBindsView = runtime.ContainerBinds

// sweepOrphanControllers removes the controllers orphanControllers picks.
func sweepOrphanControllers(ctx context.Context, rt runtime.Runtime) (int, error) {
	controllers, err := rt.LabelledContainerBinds(ctx, map[string]string{LabelKey: "filesystem", "coop.filesystem.role": "controller"})
	if err != nil || len(controllers) == 0 {
		return 0, err
	}
	all, err := rt.LabelledContainerBinds(ctx, nil)
	if err != nil {
		return 0, err
	}
	exists := func(path string) bool {
		_, err := os.Lstat(path)
		return !errors.Is(err, os.ErrNotExist)
	}
	removed := 0
	var problems []error
	for _, id := range orphanControllers(controllers, all, exists) {
		if err := rt.RemoveListedContainer(ctx, id); err != nil {
			problems = append(problems, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(problems...)
}
