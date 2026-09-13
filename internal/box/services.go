package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// EnsureServices brings the repo's sibling services up (compose up -d --wait) so a box can
// reach them by name. It is idempotent — already-running services are a fast no-op — and a
// no-op (nil, nil) when the repo has no compose file. On success it returns the non-empty
// service names in Compose's resolved order, from the same project and file selection it
// started. Progress is written to stdout/stderr; the caller decides where to point them and
// gates on a compose-capable runtime (Apple `container` has no compose). Shared by `coop up`
// and box.Run's auto-start.
func EnsureServices(rt runtime.Runtime, workspace, policyRepo string, stdout, stderr io.Writer, exposedRoots ...string) ([]string, error) {
	p, err := project.Load(policyRepo)
	if err != nil {
		return nil, err
	}
	return EnsureServicesFile(rt, workspace, ComposeFileAt(workspace, p.ComposeRel()), stdout, stderr, append([]string{policyRepo}, exposedRoots...)...)
}

// EnsureServicesFile is the explicit-file form used by trusted review policy. The file must live
// inside workspace; ValidateComposeFile enforces that its bind mounts cannot escape that boundary.
func EnsureServicesFile(rt runtime.Runtime, workspace, file string, stdout, stderr io.Writer, exposedRoots ...string) ([]string, error) {
	started, err := startServicesFile(rt, workspace, file, stdout, stderr, false, true, exposedRoots...)
	return started.names, err
}

// ServiceStart is what one start produced: the resolved service names in Compose order and the
// host ports Compose published for them.
type ServiceStart struct {
	Names []string
	Ports []ServicePort
}

// UpServices is EnsureServicesFile for `coop up`: it carries the published ports out with the
// names, and leaves the hidden-file notice to the caller — `coop up` puts those files in front of
// a human before starting, so the lower-level warning would be the second copy of one sentence.
func UpServices(rt runtime.Runtime, workspace, file string, stdout, stderr io.Writer, exposedRoots ...string) (ServiceStart, error) {
	started, err := startServicesFile(rt, workspace, file, stdout, stderr, false, false, exposedRoots...)
	return ServiceStart{Names: started.names, Ports: started.ports}, err
}

type startedServices struct {
	names  []string
	ports  []ServicePort
	hidden []string
}

func startServicesFile(rt runtime.Runtime, workspace, file string, stdout, stderr io.Writer, repoReadOnly, noticeHidden bool, exposedRoots ...string) (startedServices, error) {
	if file == "" {
		return startedServices{}, nil
	}
	// coop runs this file on the HOST daemon, so validate it first: an in-box agent may author it
	// (the compose path is no longer shadowed), but the host refuses anything that reaches outside a
	// repo-scoped, loopback-only container. The specific violation rides out to `coop up` / the
	// auto-up warning, so a refused file names exactly why.
	args, cleanup, hidden, err := snapshotComposeArgs(workspace, file, repoReadOnly, exposedRoots...)
	if err != nil {
		return startedServices{}, &ComposeRefused{Verb: "run", File: filepath.Base(file), Err: err}
	}
	defer cleanup()
	started := startedServices{hidden: hidden}
	if len(hidden) > 0 && noticeHidden {
		// coop's own channel, NOT the compose writer the caller passed: a box start hands that one a
		// buffer it reads only when compose FAILS, so this notice would be discarded on the very path
		// that needs it. Say it on every start, not just the first — the service that needed the file
		// fails in its own way ("missing BEGIN PRIVATE KEY"), and this is the only line that names the
		// cause and the fix.
		ui.Note("")
		ui.Warn("services get an empty file in place of %s (looks like a secret) — to let them read the real file, run `coop up` in a terminal and approve %s; the approval lasts until that file changes",
			strings.Join(hidden, ", "), filepath.Base(file))
	}
	// Publish each `expose`d sidecar port to its stable per-workspace host port via a merged
	// override (the base file's `expose` publishes nothing, so this adds the only host mapping).
	ports := servicePortsWithArgs(rt, workspace, args)
	if sp := ports; len(sp) > 0 {
		override, cleanup, err := writeServiceOverride(sp, workspace, exposedRoots...)
		if err != nil {
			return started, err
		}
		defer cleanup()
		args = append(args, "-f", override)
	}
	services, err := resolvedComposeServices(rt, args, stderr)
	if err != nil {
		return started, err
	}
	upArgs := append(append([]string(nil), args...), "up", "-d", "--wait", "--remove-orphans")
	if err := runCompose(rt, stdout, stderr, "up", upArgs); err != nil {
		return started, err
	}
	started.names, started.ports = services, ports
	return started, nil
}

// Snapshot approved bytes outside the writable workspace. All commands in one operation use
// this file; the explicit project directory preserves relative binds and ownership labels.
// hidden names the repo-relative secret-looking bind sources the services get decoys for — empty
// when there are none or when a human approved this exact file (ReviewServiceSecrets).
func snapshotComposeArgs(workspace, file string, repoReadOnly bool, exposedRoots ...string) (args []string, cleanup func(), hidden []string, err error) {
	data, err := readValidatedCompose(file, workspace, repoReadOnly)
	if err != nil {
		return nil, nil, nil, err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, nil, nil, err
	}
	dir, err := privateWorkspaceTempDir(workspace, "coop-compose-", exposedRoots...)
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	args = []string{"compose", "-p", ComposeProject(workspace),
		"--project-directory", filepath.Dir(abs), "--env-file", os.DevNull, "-f", path}
	// The sidecars get the box's secret shadowing too: a decoy over every hidden path a repo bind
	// would otherwise hand them raw (see serviceShadowOverride) — unless a human approved this
	// exact file's content on this host, which is the one way a service legitimately reads a
	// secret-looking file (a generated dev TLS key). Every compose invocation — start, port
	// discovery, teardown — carries the same decision, so the project definition is one and the same.
	decoys, hidden, err := serviceShadowPlan(workspace, abs, data)
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("project secret shadowing into sibling services: %w", err)
	}
	if len(hidden) > 0 {
		if approval, ok := ApprovedServiceSecrets(data); ok {
			// Only the files the human actually saw and approved come out from behind a decoy.
			decoys, hidden = keepDecoysOutside(decoys, approval.Paths)
		}
	}
	shadow, needed, err := writeServiceShadowOverride(decoys, dir)
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("project secret shadowing into sibling services: %w", err)
	}
	if needed {
		args = append(args, "-f", shadow)
	}
	return args, cleanup, hidden, nil
}

// ComposeRefused is a Compose file coop would not hand the host daemon: the validation found a
// bind, a path, or a shape it refuses to run. It carries the violation itself so a caller can
// render the cause without the wrapper sentence around it.
type ComposeRefused struct {
	Verb string // what coop was asked to do with the file: "run" or "stop"
	File string // the Compose file's base name
	Err  error  // the specific violation
}

func (e *ComposeRefused) Error() string {
	return "refusing to " + e.Verb + " " + e.File + ": " + e.Err.Error()
}

func (e *ComposeRefused) Unwrap() error { return e.Err }

// ErrNoComposeServices is a Compose file that parses but declares nothing to start. It is a
// distinct outcome from a broken file: the fix is adding a service, not repairing one.
var ErrNoComposeServices = errors.New("compose config --services returned no services")

// ErrExposedTempDir is the one snapshot refusal a person fixes in their environment rather than
// in the Compose file: TMPDIR resolves inside a directory an agent can see, so the validated copy
// coop is about to run would be writable by the very agent it is protecting the host from.
var ErrExposedTempDir = errors.New("temporary directory must resolve outside agent-exposed directories — choose an external TMPDIR")

// CheckServiceTempDir reports whether the private snapshot area the service commands need can be
// created outside every directory an agent can see. It is the same resolution the snapshot makes,
// run BEFORE a caller announces that it is starting anything: a refusal that arrives under
// "Starting services…" reads as if the start had begun, and it had not.
func CheckServiceTempDir(workspace string, exposedRoots ...string) error {
	dir, err := privateWorkspaceTempDir(workspace, "coop-compose-", exposedRoots...)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func privateWorkspaceTempDir(workspace, pattern string, exposedRoots ...string) (string, error) {
	absParent, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(absParent)
	if err != nil {
		return "", err
	}
	for _, root := range append([]string{workspace}, exposedRoots...) {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		inside, err := futurePathWithin(abs, parent)
		if err != nil {
			return "", err
		}
		if inside {
			return "", ErrExposedTempDir
		}
	}
	// Resolve before allocating: a TMPDIR alias inside the repo must not remain in any
	// later write/read/cleanup path, even when its current target is outside the repo.
	return os.MkdirTemp(parent, pattern)
}

func resolvedComposeServices(rt runtime.Runtime, args []string, stderr io.Writer) ([]string, error) {
	var stdout bytes.Buffer
	configArgs := append(append([]string(nil), args...), "config", "--services")
	if err := runCompose(rt, &stdout, stderr, "config --services", configArgs); err != nil {
		return nil, err
	}
	var services []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			services = append(services, name)
		}
	}
	if len(services) == 0 {
		return nil, ErrNoComposeServices
	}
	return services, nil
}

// DownServices stops the current workspace's hashed Compose project. Volumes are optional.
func DownServices(rt runtime.Runtime, workspace, policyRepo string, volumes bool, stdout, stderr io.Writer, exposedRoots ...string) error {
	p, err := project.Load(policyRepo)
	if err != nil {
		return err
	}
	file := ComposeFileAt(workspace, p.ComposeRel())
	if file == "" {
		return nil
	}
	return DownServicesFile(rt, workspace, file, volumes, stdout, stderr, append([]string{policyRepo}, exposedRoots...)...)
}

// DownServicesFile is the explicit-file counterpart to EnsureServicesFile. Review runs use it to
// remove their short-lived project, network, and volumes before the disposable candidate goes away.
func DownServicesFile(rt runtime.Runtime, workspace, file string, volumes bool, stdout, stderr io.Writer, exposedRoots ...string) error {
	if file == "" {
		return nil
	}
	args, cleanup, _, err := snapshotComposeArgs(workspace, file, false, exposedRoots...)
	if err != nil {
		return &ComposeRefused{Verb: "stop", File: filepath.Base(file), Err: err}
	}
	defer cleanup()
	args = append(args, "down", "--remove-orphans")
	if volumes {
		args = append(args, "--volumes")
	}
	return runCompose(rt, stdout, stderr, "down", args)
}

const (
	composeProjectLabel    = "com.docker.compose.project"
	composeWorkingDirLabel = "com.docker.compose.project.working_dir"
)

// ServiceVolume is one volume `coop down --delete-volumes` would destroy: the runtime's own name
// for it (what the user sees in `docker volume ls`) and what it holds, when coop knows — a volume
// its own scaffold declared. Holds is empty for anything else; naming the target is the promise,
// guessing at its contents is not.
type ServiceVolume struct {
	Name  string
	Holds string
}

// scaffoldVolumeHolds describes the volumes coop's generated Compose file declares. The deletion
// preview is the one place a person decides whether data they care about is about to go, so the
// two volumes coop itself created say what is in them. See internal/scaffold/compose.go.
var scaffoldVolumeHolds = map[string]string{
	"pgdata":    "database data",
	"redisdata": "Redis data",
}

// ErrVolumesUnknown is a runtime that would not say which volumes this project owns. It is a
// refusal, not an empty list: the answer is about to be printed in front of a permanent deletion.
var ErrVolumesUnknown = errors.New("the runtime did not return the project's volume information")

// ServiceVolumes lists the volumes Compose created for this workspace's project — its declared
// named volumes plus the anonymous volumes Compose attached to its services, both of which carry
// the project label. A volume declared `external: true` is NOT created by Compose and carries no
// such label, so it is absent here: `--delete-volumes` deletes this project's data, never a
// volume somebody else owns. Bind mounts are files, not volumes, and never appear.
//
// It reports what the runtime actually answered. An unreadable answer is an error, not an empty
// list: "nothing to delete" has to be evidence, since it is printed right before a deletion.
func ServiceVolumes(rt runtime.Runtime, workspace string, stderr io.Writer) ([]ServiceVolume, error) {
	project := ComposeProject(workspace)
	var out bytes.Buffer
	// The runtime's own complaint goes straight to the terminal, where every other runtime
	// diagnostic appears; the error below is what coop can say about it.
	code, err := rt.Run(nil, &out, stderr, "volume", "ls", "--filter", "label="+composeProjectLabel+"="+project, "--format", "{{.Name}}")
	if err != nil || code != 0 {
		return nil, ErrVolumesUnknown
	}
	var volumes []ServiceVolume
	for _, line := range strings.Split(out.String(), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		volumes = append(volumes, ServiceVolume{Name: name, Holds: scaffoldVolumeHolds[strings.TrimPrefix(name, project+"_")]})
	}
	return volumes, nil
}

// StopSessionServices removes only the current workspace's Compose containers while preserving
// its volumes. It uses immutable runtime ownership labels instead of the workspace's mutable
// Compose file, so interrupted agent edits cannot prevent cleanup. The next turn starts services
// again through EnsureServices.
func StopSessionServices(ctx context.Context, rt runtime.Runtime, workspace, policyRepo string) error {
	// Cleanup must survive a policy typo introduced after the services started. ComposePath's
	// default-on-error behavior preserves the existing immutable-label cleanup path; launch and
	// ordinary service control still reject the invalid policy before touching the runtime.
	composeDir := filepath.Dir(filepath.Join(workspace, filepath.FromSlash(project.ComposePath(policyRepo))))
	project := ComposeProject(workspace)
	_, err := rt.RemoveByLabels(ctx, map[string]string{
		composeProjectLabel:    project,
		composeWorkingDirLabel: filepath.Clean(composeDir),
	})
	if err != nil {
		return fmt.Errorf("stop session services: %w", err)
	}
	// The project's networks go with its containers: Docker hands out about thirty subnets in
	// total, and every session in a fresh worktree gets its own project, so leaving a network
	// behind per session exhausts the pool ("address pools fully subnetted") in a day's work.
	if _, err := RemoveProjectNetworks(ctx, rt, project); err != nil {
		return fmt.Errorf("stop session services: %w", err)
	}
	return nil
}

// RemoveProjectNetworks removes the compose project's networks that no container — running or
// stopped — is attached to any more, and reports how many it removed. Compose recreates a network
// on the next start, so nothing is lost; a network still in use is left alone.
func RemoveProjectNetworks(ctx context.Context, rt runtime.Runtime, project string) (int, error) {
	ids, err := rt.NetworkIDs(ctx, "label="+composeProjectLabel+"="+project)
	if err != nil {
		return 0, fmt.Errorf("list %s networks: %w", project, err)
	}
	return removeUnusedNetworks(ctx, rt, ids)
}

func removeUnusedNetworks(ctx context.Context, rt runtime.Runtime, ids []string) (int, error) {
	removed := 0
	for _, id := range ids {
		users, err := rt.ContainerIDsOnNetwork(ctx, id)
		if err != nil {
			return removed, fmt.Errorf("list containers on network %s: %w", id, err)
		}
		if len(users) > 0 {
			continue
		}
		if err := rt.RemoveNetwork(ctx, id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func runCompose(rt runtime.Runtime, stdout, stderr io.Writer, action string, args []string) error {
	code, err := rt.Run(nil, stdout, stderr, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("compose %s exited with status %d", action, code)
	}
	return nil
}

// autoUpServices reports whether box.Run should auto-start sibling services before launching a
// box: the COOP_AUTO_UP toggle is on (default), the box joins the services network (so it could
// reach them), it isn't offline (COOP_EGRESS=none, where there's nothing to reach), and the
// runtime supports compose — Apple `container` does not. Whether a compose file actually exists
// is checked separately, by EnsureServices.
// LiveBoxes lists the coop boxes running in repo's project right now, other than except (a box
// may pass its own execution id). Sibling services start only when this is empty: a running
// agent can edit the compose file and swap a validated bind source for a link to a host path in
// the moment between coop's check and Docker opening it, and the only launch it could race is one
// that happens while it runs — a peer or consult box mid-iteration, or a `coop up` typed
// alongside it. With no box running there is nothing to race. Live binds stay live.
func LiveBoxes(repo, except string) ([]forkspace.ExecutionObservation, error) {
	authority, _, err := forkspace.ResolveProjectBinding(repo)
	if err != nil {
		return nil, err
	}
	observations, problems := forkspace.Executions(authority)
	if len(problems) > 0 {
		return nil, fmt.Errorf("read the project's sandbox registry: %w", errors.Join(problems...))
	}
	var live []forkspace.ExecutionObservation
	for _, observation := range observations {
		if observation.Running && observation.Record.ID != except {
			live = append(live, observation)
		}
	}
	return live, nil
}

// DescribeLiveBoxes names running boxes for a refusal: kind and workspace, deduplicated.
func DescribeLiveBoxes(live []forkspace.ExecutionObservation) string {
	var parts []string
	seen := map[string]bool{}
	for _, observation := range live {
		part := string(observation.Record.Kind) + " in " + filepath.Base(observation.Record.Workspace)
		if observation.Record.Fork != nil {
			part = string(observation.Record.Kind) + " in fork " + observation.Record.Fork.Name
		}
		if !seen[part] {
			seen[part] = true
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ", ")
}

func autoUpServices(cfg *config.Config, spec RunSpec, rtName string) bool {
	return cfg.AutoUp && spec.Network && cfg.Egress == "open" && rtName != "container"
}
