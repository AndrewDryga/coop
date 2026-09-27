package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestAuthorityMountValidationRunsInsideTheSharedLaunchWindow(t *testing.T) {
	repo := t.TempDir()
	exclusive, err := forkspace.LockServiceLaunch(context.Background(), repo, true)
	if err != nil {
		t.Fatal(err)
	}
	validated := make(chan struct{})
	finished := make(chan error, 1)
	want := errors.New("validation stopped the launch")
	go func() {
		unlock, err := enterAuthorityMountWindow(context.Background(), repo, func() error {
			close(validated)
			return want
		})
		if unlock != nil {
			unlock()
		}
		finished <- err
	}()
	select {
	case <-validated:
		exclusive()
		t.Fatal("mount validation ran before the exclusive anchor transition ended")
	case <-time.After(100 * time.Millisecond):
	}
	exclusive()
	select {
	case err := <-finished:
		if !errors.Is(err, want) {
			t.Fatalf("mount-window validation error = %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mount validation did not resume after the anchor transition")
	}
}

func anchoredProjectFixture(t *testing.T) (RunSpec, string) {
	t.Helper()
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ReviewApproval(project, egress.Filtered, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return RunSpec{Repo: project}, state
}

func TestAuthorityMountGuardPermitsOnlySafeProjectRelations(t *testing.T) {
	spec, state := anchoredProjectFixture(t)
	project := spec.Repo
	descendant := filepath.Join(project, "cache")
	if err := os.Mkdir(descendant, 0o700); err != nil {
		t.Fatal(err)
	}
	mutableAlias := filepath.Join(project, "mutable-source")
	mutableTarget := t.TempDir()
	if err := os.Mkdir(filepath.Join(mutableTarget, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mutableTarget, mutableAlias); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project-parent")
	if err := os.Symlink(filepath.Dir(project), alias); err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	sharedLeaf := filepath.Join(shared, "leaf")
	if err := os.Mkdir(sharedLeaf, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		options []string
		allowed bool
	}{
		{"exact project", []string{"-v", project + ":/workspace"}, true},
		{"writable project descendant", []string{"--mount", "type=bind,source=" + descendant + ",target=/cache"}, false},
		{"readonly project descendant", []string{"-v", descendant + ":/cache:ro"}, false},
		{"mutable symlink inside project", []string{"-v", mutableAlias + ":/cache:ro"}, false},
		{"nested path through mutable symlink inside project", []string{"-v", filepath.Join(mutableAlias, "nested") + ":/cache:ro"}, false},
		{"writable project parent", []string{"-v", filepath.Dir(project) + ":/host"}, false},
		{"writable project parent compact", []string{"-v" + filepath.Dir(project) + ":/host"}, false},
		{"writable project parent bundled short flags", []string{"-iv" + filepath.Dir(project) + ":/host"}, false},
		{"writable project parent alias", []string{"-v", alias + ":/host"}, false},
		{"readonly project parent exposes prospective private state", []string{"-v", filepath.Dir(project) + ":/host:ro"}, false},
		{"private state", []string{"-v", state + ":/state:ro"}, false},
		{"private state parent", []string{"--mount", "type=bind,source=" + filepath.Dir(state) + ",target=/parent,readonly"}, false},
		{"private state descendant", []string{"-v", filepath.Join(state, "future") + ":/future:ro"}, false},
		{"nested source below another writable mount", []string{"-v", shared + ":/shared", "-v", sharedLeaf + ":/leaf:ro"}, false},
		{"unrelated", []string{"-v", t.TempDir() + ":/other"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateAuthorityMounts(context.Background(), spec, test.options, state, nil, authorityMountAllowlist{})
			if (err == nil) != test.allowed {
				t.Fatalf("mount decision = %v, want allowed=%v", err, test.allowed)
			}
		})
	}
}

func TestAuthorityMountGuardInspectsNamedVolumesAndOpaqueInheritance(t *testing.T) {
	spec, state := anchoredProjectFixture(t)
	parent := filepath.Dir(spec.Repo)
	reader := func(_ context.Context, names []string) (runtime.VolumeExposure, error) {
		if len(names) != 1 || names[0] != "shared" {
			t.Fatalf("inspected volumes = %v", names)
		}
		return runtime.VolumeExposure{Sources: []string{parent}}, nil
	}
	if err := validateAuthorityMounts(context.Background(), spec, []string{"-v", "shared:/data"}, state, reader, authorityMountAllowlist{}); err == nil {
		t.Fatal("writable named volume backed by the project parent was accepted")
	}
	if err := validateAuthorityMounts(context.Background(), spec, []string{"-v", "shared:/data:ro"}, state, reader, authorityMountAllowlist{}); err == nil {
		t.Fatal("read-only project-parent volume exposed prospective private state")
	}
	safeReader := func(context.Context, []string) (runtime.VolumeExposure, error) {
		return runtime.VolumeExposure{Sources: []string{t.TempDir()}}, nil
	}
	if err := validateAuthorityMounts(context.Background(), spec, []string{"-v", "shared:/data:ro"}, state, safeReader, authorityMountAllowlist{}); err != nil {
		t.Fatal("unrelated read-only volume was rejected", err)
	}
	for _, options := range [][]string{
		{"--volumes-from", "another-box"},
		{"--mount", "type=volume,target=/data,volume-opt=device=" + parent},
		{"--volume-driver", "local", "-v", "shared:/data"},
	} {
		if err := validateAuthorityMounts(context.Background(), spec, options, state, reader, authorityMountAllowlist{}); err == nil {
			t.Fatalf("opaque volume authority was accepted: %v", options)
		}
	}
	privateReader := func(context.Context, []string) (runtime.VolumeExposure, error) {
		return runtime.VolumeExposure{Sources: []string{state}}, nil
	}
	if err := validateAuthorityMounts(context.Background(), spec, []string{"-v", "shared:/data:ro"}, state, privateReader, authorityMountAllowlist{}); err == nil || !strings.Contains(err.Error(), "protected host path") {
		t.Fatalf("read-only private-state volume = %v", err)
	}
}

func TestAuthorityMountGuardProtectsForkAnchorState(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := forkspace.Workspace(repo, "worker")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "worker")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "worker")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Repo: workspace, ForkName: identity.Name, ForkGeneration: string(identity.Generation), ActivityRepo: repo}
	if err := validateAuthorityMounts(context.Background(), spec,
		[]string{"-v", filepath.Dir(workspace) + ":/forks"}, "", nil, authorityMountAllowlist{}); err == nil {
		t.Fatal("writable fork-home mount could transplant the generation marker")
	}
	if err := validateAuthorityMounts(context.Background(), spec,
		[]string{"-v", forkspace.StateDir(repo) + ":/state:ro"}, "", nil, authorityMountAllowlist{}); err == nil {
		t.Fatal("fork private anchor state was exposed read-only")
	}
}

func TestAuthorityMountGuardTreatsCompanionsAsMutableProjectRoots(t *testing.T) {
	repo := t.TempDir()
	companion := t.TempDir()
	nested := filepath.Join(companion, "safe")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{
		Repo:                  repo,
		CompanionRepositories: []CompanionRepository{{Name: "docs", HostPath: companion}},
	}
	for _, test := range []struct {
		name    string
		options []string
		allowed bool
	}{
		{"exact companion", []string{"-v", companion + ":/companions/docs:ro"}, true},
		{"nested companion source", []string{"-v", nested + ":/extra:ro"}, false},
		{"writable companion parent", []string{"-v", filepath.Dir(companion) + ":/host"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateAuthorityMounts(context.Background(), spec, test.options, "", nil, authorityMountAllowlist{})
			if (err == nil) != test.allowed {
				t.Fatalf("companion mount decision = %v, want allowed=%v", err, test.allowed)
			}
		})
	}
}

func TestAuthorityMountGuardProtectsForkStateFromMainProjectRuns(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := forkspace.Workspace(repo, "worker")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "worker")
	if err != nil {
		t.Fatal(err)
	}
	_, err = forkspace.EnsureGenerationLocked(repo, "worker")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Repo: repo}
	if err := validateAuthorityMounts(context.Background(), spec,
		[]string{"-v", parent + ":/projects"}, "", nil, authorityMountAllowlist{}); err == nil {
		t.Fatal("ordinary main-project run could expose sibling fork authority state")
	}
}

func TestAuthorityMountGuardProtectsProspectiveHostAuthority(t *testing.T) {
	registryRoot := filepath.Join(t.TempDir(), "execution-registry")
	t.Setenv(forkspace.TestExecutionRegistryRootEnv, registryRoot)
	parent := t.TempDir()
	repo := filepath.Join(parent, "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	networkParent := t.TempDir()
	networkState := filepath.Join(networkParent, "network")
	registries, err := forkspace.ExecutionRegistryDirs(repo)
	if err != nil {
		t.Fatal(err)
	}
	fallback := registries[len(registries)-1]
	forkState := forkspace.StateDir(repo)
	for _, path := range []string{networkState, fallback, forkState} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prospective authority path already exists: %s: %v", path, err)
		}
	}
	for _, test := range []struct {
		name   string
		source string
	}{
		{"future network state", networkState},
		{"future network state parent", networkParent},
		{"future fork state", forkState},
		{"future fork state parent", filepath.Dir(forkState)},
		{"future execution registry", fallback},
		{"future execution registry parent", registryRoot},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateAuthorityMounts(context.Background(), RunSpec{Repo: repo},
				[]string{"-v", test.source + ":/host"}, networkState, nil, authorityMountAllowlist{})
			if err == nil {
				t.Fatalf("writable mount of %q exposed prospective authority", test.source)
			}
		})
	}
	if err := validateAuthorityMounts(context.Background(), RunSpec{Repo: repo},
		[]string{"-v", t.TempDir() + ":/other"}, networkState, nil, authorityMountAllowlist{}); err != nil {
		t.Fatalf("unrelated mount was refused: %v", err)
	}
}

func TestAuthorityMountGuardProtectsHostConfigAndStateSiblings(t *testing.T) {
	root := t.TempDir()
	xdgCoop := filepath.Join(root, "xdg-state", "coop")
	networkState := filepath.Join(xdgCoop, "network")
	serviceState := filepath.Join(root, "home-state", "coop")
	gitViews := filepath.Join(root, "git-views")
	boxHome := filepath.Join(root, "operator-config", "coop")
	t.Setenv(ServiceStateRootEnv, serviceState)
	t.Setenv(forkspace.GitViewRootEnv, gitViews)

	cfg := &config.Config{ConfigDir: filepath.Join(root, "config"), BoxHome: boxHome, Homes: true}
	selected := cfg.AgentDir("codex")
	unselected := cfg.AgentProfileDir("codex", "work")
	hostCredentials := filepath.Join(cfg.ConfigDir, "host-credentials")
	customMCP := filepath.Join(root, "operator-mcp", "servers.json")
	cfg.MCPFile = customMCP
	for _, dir := range []string{
		selected, unselected, hostCredentials, filepath.Dir(customMCP),
		xdgCoop, serviceState, gitViews, boxHome,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(customMCP, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	spec := RunSpec{Agent: "codex", Homes: true}
	allow, err := protectRunPrivateState(cfg, spec, authorityMountAllowlist{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		source  string
		allowed bool
	}{
		{"selected credential profile", selected, true},
		{"whole config tree", cfg.ConfigDir, false},
		{"config parent", filepath.Dir(cfg.ConfigDir), false},
		{"Coop host config home", cfg.BoxHome, false},
		{"main Coop config", filepath.Join(cfg.BoxHome, "coop.conf"), false},
		{"remote session policy", filepath.Join(cfg.BoxHome, "session-policies.yaml"), false},
		{"unselected credential profile", unselected, false},
		{"host credential broker state", hostCredentials, false},
		{"configured MCP source", customMCP, false},
		{"configured MCP source parent", filepath.Dir(customMCP), false},
		{"network authority", networkState, false},
		{"session database sibling", filepath.Join(xdgCoop, "sessions"), false},
		{"eval results sibling", filepath.Join(xdgCoop, "evals"), false},
		{"service approvals", filepath.Join(serviceState, "service-approvals"), false},
		{"service session state", filepath.Join(serviceState, "sessions"), false},
		{"service task leases", filepath.Join(serviceState, "task-leases"), false},
		{"service git views", filepath.Join(serviceState, "gitviews"), false},
		{"external git views", gitViews, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateAuthorityMounts(context.Background(), spec,
				[]string{"-v", test.source + ":/host"}, networkState, nil, allow)
			if (err == nil) != test.allowed {
				t.Fatalf("mount decision = %v, want allowed=%v", err, test.allowed)
			}
		})
	}
	bridge := filepath.Join(boxHome, "box", "bin", "emisar-mcp")
	err = validateAuthorityMounts(context.Background(), spec,
		[]string{"-v", bridge + ":/usr/local/bin/emisar-mcp:ro"}, networkState, nil, allow)
	if err == nil || !strings.Contains(err.Error(), bridge) || !strings.Contains(err.Error(), boxHome) ||
		!strings.Contains(err.Error(), "read-only mounts are blocked") || !strings.Contains(err.Error(), "remove it from COOP_RUN_ARGS") {
		t.Fatalf("protected read-only bridge mount guidance = %v", err)
	}
}

func TestEvalMountAllowsOnlyItsGeneratedWorkspace(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv(ServiceStateRootEnv, filepath.Join(t.TempDir(), "service-state"))
	evalRoot := filepath.Join(stateHome, "coop", "eval")
	trial := filepath.Join(evalRoot, "run-1", "work", "case-c0-r0")
	workspace := filepath.Join(trial, "workspace")
	snapshot := filepath.Join(trial, "snapshot")
	verifier := filepath.Join(evalRoot, "starters", "core", "verifiers", "fix-the-cause")
	stagedVerifier := filepath.Join(evalRoot, "run-1", "inputs", "cases", "fix-the-cause", "verifier")
	sibling := filepath.Join(evalRoot, "run-1", "work", "case-c0-r1", "workspace")
	credential := filepath.Join(stateHome, "coop", "credentials")
	for _, path := range []string{workspace, snapshot, verifier, stagedVerifier, sibling, credential} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	candidate := RunSpec{Repo: workspace}
	grader := RunSpec{Repo: snapshot, GradeSnapshot: true, EvalVerifier: verifier}
	stagedGrader := RunSpec{Repo: snapshot, GradeSnapshot: true, EvalVerifier: stagedVerifier}
	for _, tc := range []struct {
		name    string
		spec    RunSpec
		options []string
		allowed bool
	}{
		{"candidate workspace", candidate, []string{"-v", workspace + ":/workspace"}, true},
		{"candidate cannot mount verifier", candidate, []string{"-v", workspace + ":/workspace", "-v", verifier + ":/verifier:ro"}, false},
		{"candidate cannot mount staged verifier", candidate, []string{"-v", workspace + ":/workspace", "-v", stagedVerifier + ":/verifier:ro"}, false},
		{"candidate cannot claim grader verifier", RunSpec{Repo: workspace, EvalVerifier: verifier}, []string{"-v", verifier + ":/verifier:ro"}, false},
		{"candidate cannot mount another trial", candidate, []string{"-v", sibling + ":/other"}, false},
		{"candidate cannot mount run record", candidate, []string{"-v", filepath.Join(evalRoot, "run-1") + ":/run:ro"}, false},
		{"candidate cannot mount eval root", candidate, []string{"-v", evalRoot + ":/eval:ro"}, false},
		{"candidate cannot mount credential sibling", candidate, []string{"-v", credential + ":/credentials:ro"}, false},
		{"grader snapshot and read-only verifier", grader, []string{"-v", snapshot + ":/workspace", "-v", verifier + ":/verifier:ro"}, true},
		{"grader snapshot and read-only staged verifier", stagedGrader, []string{"-v", snapshot + ":/workspace", "-v", stagedVerifier + ":/verifier:ro"}, true},
		{"grader cannot write staged verifier", stagedGrader, []string{"-v", stagedVerifier + ":/verifier"}, false},
		{"grader cannot write verifier", grader, []string{"-v", verifier + ":/verifier"}, false},
		{"grader cannot mount candidate workspace", grader, []string{"-v", workspace + ":/other"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAuthorityMounts(context.Background(), tc.spec, tc.options,
				filepath.Join(stateHome, "coop", "network"), nil, authorityMountAllowlist{})
			if (err == nil) != tc.allowed {
				t.Fatalf("mount decision = %v, want allowed=%v", err, tc.allowed)
			}
		})
	}
	alias := filepath.Join(evalRoot, "run-1", "work", "alias-c0-r0", "workspace")
	if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(credential, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateAuthorityMounts(context.Background(), RunSpec{Repo: alias},
		[]string{"-v", alias + ":/workspace"}, "", nil, authorityMountAllowlist{}); err == nil {
		t.Fatal("symlinked eval workspace reached the runtime")
	}
	if err := os.Chmod(filepath.Join(evalRoot, "run-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateAuthorityMounts(context.Background(), candidate,
		[]string{"-v", workspace + ":/workspace"}, "", nil, authorityMountAllowlist{}); err == nil {
		t.Fatal("nonprivate eval run ancestor reached the runtime")
	}
}

func TestEvalMountDecisionReachesRunAssembly(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv(ServiceStateRootEnv, filepath.Join(t.TempDir(), "service-state"))
	evalRoot := filepath.Join(stateHome, "coop", "eval")
	trial := filepath.Join(evalRoot, "run-1", "work", "case-c0-r0")
	workspace := filepath.Join(trial, "workspace")
	snapshot := filepath.Join(trial, "snapshot")
	verifier := filepath.Join(evalRoot, "run-1", "inputs", "cases", "fix-the-cause", "verifier")
	for _, path := range []string{workspace, snapshot, verifier} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	rt := recorderRuntime(t, recorder)
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	for _, spec := range []RunSpec{
		{Image: "i", Repo: workspace, Workdir: "/workspace", Cmd: []string{"true"}, Batch: true, Quiet: true},
		{Image: "i", Repo: snapshot, Workdir: "/workspace", Cmd: []string{"true"}, Batch: true, Quiet: true,
			GradeSnapshot: true, EvalVerifier: verifier, ExtraArgs: []string{"-v", verifier + ":/verifier:ro"}},
	} {
		if code, err := Run(cfg, rt, spec); err != nil || code != 0 {
			t.Fatalf("eval Run(%q) = %d, %v; want 0, nil", spec.Repo, code, err)
		}
	}
	before, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range []string{workspace + ":/workspace", snapshot + ":/workspace", verifier + ":/verifier:ro"} {
		if !strings.Contains(string(before), mount) {
			t.Fatalf("eval mount %q missing from runtime invocation: %q", mount, before)
		}
	}
	candidate := RunSpec{Image: "i", Repo: workspace, Workdir: "/workspace", Cmd: []string{"true"}, Batch: true, Quiet: true,
		ExtraArgs: []string{"-v", verifier + ":/verifier:ro"}}
	if code, err := Run(cfg, rt, candidate); err == nil || code != -1 {
		t.Fatalf("candidate verifier mount = %d, %v; want refusal before runtime", code, err)
	}
	after, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("runtime invoked for denied verifier mount: before %q, after %q", before, after)
	}
}

func TestAuthorityMountGuardAllowsOnlyOneRemoteSessionOutputSubtree(t *testing.T) {
	root := t.TempDir()
	t.Setenv(ServiceStateRootEnv, filepath.Join(root, "service-state"))
	stateRoot := filepath.Join(root, "session-state")
	sessionID := "remote_1"
	cfg := &config.Config{ConfigDir: filepath.Join(stateRoot, "acp", sessionID), Homes: true}
	selected := cfg.AgentDir("codex")
	outputRoot := filepath.Join(stateRoot, "output", sessionID)
	for _, dir := range []string{selected, outputRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spec := RunSpec{
		Agent: "codex", Homes: true, RunID: "session-" + strings.Repeat("ab", 12),
		SessionOutputRoot: outputRoot,
	}
	allow, err := protectRunPrivateState(cfg, spec, authorityMountAllowlist{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		source  string
		allowed bool
	}{
		{"selected credential profile", selected, true},
		{"this session output", outputRoot, true},
		{"whole session state", stateRoot, false},
		{"session database", filepath.Join(stateRoot, "sessions.db"), false},
		{"another session output", filepath.Join(stateRoot, "output", "remote_2"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateAuthorityMounts(context.Background(), spec,
				[]string{"-v", test.source + ":/host"}, "", nil, allow)
			if (err == nil) != test.allowed {
				t.Fatalf("mount decision = %v, want allowed=%v", err, test.allowed)
			}
		})
	}
}

func TestAuthorityMountGuardAllowsOnlyBoundControllerJobSources(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "sessions")
	t.Setenv(ServiceStateRootEnv, stateRoot)
	sessionID := "remote_123"
	storeID := "store_test"
	repo := filepath.Join(stateRoot, "job-sources", strings.Repeat("a", 64), "repository")
	workspace := forkspace.Workspace(repo, "remote-fork")
	companion := filepath.Join(stateRoot, "repositories", sessionID, "docs")
	for _, dir := range []string{repo, workspace, companion, filepath.Join(stateRoot, "acp", sessionID)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	unlock, err := forkspace.LockState(repo, "remote-fork")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "remote-fork")
	if err == nil {
		err = forkspace.ReserveWorkspaceLocked(repo, forkspace.WorkspaceReservation{
			Version: forkspace.WorkspaceReservationVersion, Fork: identity,
			Kind: forkspace.WorkspaceReservationRemoteSession, OwnerStoreID: storeID, OwnerID: sessionID,
			CreatedAt: time.Now().UTC(),
		})
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{forkspace.Home(repo), workspace, companion} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{ConfigDir: filepath.Join(stateRoot, "acp", sessionID)}
	spec := RunSpec{
		Repo: workspace, ActivityRepo: repo, ForkName: identity.Name,
		ForkGeneration: string(identity.Generation), RunID: "session-" + strings.Repeat("ab", 12),
		ActivityKind: forkspace.ExecutionRemoteSession, ActivityReservationOwner: sessionID,
		SessionStoreID:        storeID,
		ControllerJob:         true,
		CompanionRepositories: []CompanionRepository{{Name: "docs", HostPath: companion}},
	}
	allow, err := protectRunPrivateState(cfg, spec, authorityMountAllowlist{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, source string
		allowed      bool
	}{
		{"bound workspace", workspace, true},
		{"bound companion", companion, true},
		{"whole session state", stateRoot, false},
		{"source mirror", repo, false},
		{"other session companion", filepath.Join(stateRoot, "repositories", "remote_other", "docs"), false},
		{"other session workspace", filepath.Join(stateRoot, "job-sources", strings.Repeat("b", 64), "repository-forks", "remote-fork"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAuthorityMounts(context.Background(), spec,
				[]string{"-v", tc.source + ":/host:ro"}, "", nil, allow)
			if (err == nil) != tc.allowed {
				t.Fatalf("mount decision = %v, want allowed=%v", err, tc.allowed)
			}
		})
	}
	if err := validateAuthorityMounts(context.Background(), spec,
		[]string{"-v", companion + ":/host"}, "", nil, allow); err == nil {
		t.Fatal("controller job companion accepted a writable mount")
	}
	restricted := spec
	restricted.ActivityRepo = ""
	restricted.ActivityKind = ""
	restricted.ActivityReservationOwner = ""
	restrictedAllow, err := protectRunPrivateState(cfg, restricted, authorityMountAllowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAuthorityMounts(context.Background(), restricted,
		[]string{"-v", workspace + ":/host:ro"}, "", nil, restrictedAllow); err != nil {
		t.Fatalf("restricted controller job workspace refused: %v", err)
	}
	aliasParent := filepath.Join(root, "alias-parent")
	if err := os.Symlink(root, aliasParent); err != nil {
		t.Fatal(err)
	}
	aliased := spec
	aliased.ActivityRepo = filepath.Join(aliasParent, "sessions", "job-sources", strings.Repeat("a", 64), "repository")
	aliased.Repo = forkspace.Workspace(aliased.ActivityRepo, aliased.ForkName)
	aliasedAllow, err := protectRunPrivateState(cfg, aliased, authorityMountAllowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAuthorityMounts(context.Background(), aliased,
		[]string{"-v", aliased.Repo + ":/host:ro"}, "", nil, aliasedAllow); err != nil {
		t.Fatalf("state-root ancestor alias refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*RunSpec)
	}{
		{"not a controller job", func(s *RunSpec) { s.ControllerJob = false }},
		{"not a remote session", func(s *RunSpec) { s.RunID = "" }},
		{"wrong session owner", func(s *RunSpec) { s.ActivityReservationOwner = "remote_other" }},
		{"wrong session store", func(s *RunSpec) { s.SessionStoreID = "store_other" }},
		{"companion outside owner tree", func(s *RunSpec) { s.CompanionRepositories[0].HostPath = filepath.Join(stateRoot, "acp", sessionID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := spec
			changed.CompanionRepositories = append([]CompanionRepository(nil), spec.CompanionRepositories...)
			tc.edit(&changed)
			allowed, err := protectRunPrivateState(cfg, changed, authorityMountAllowlist{})
			if err == nil {
				err = validateAuthorityMounts(context.Background(), changed,
					[]string{"-v", workspace + ":/host"}, "", nil, allowed)
			}
			if err == nil {
				t.Fatal("unbound controller job mounted protected workspace")
			}
		})
	}
	alias := filepath.Join(stateRoot, "repositories", sessionID, "alias")
	if err := os.Symlink(companion, alias); err != nil {
		t.Fatal(err)
	}
	changed := spec
	changed.CompanionRepositories = []CompanionRepository{{Name: "alias", HostPath: alias}}
	if _, err := protectRunPrivateState(cfg, changed, authorityMountAllowlist{}); err == nil {
		t.Fatal("controller job companion accepted a symlinked snapshot")
	}
	if err := os.Chmod(filepath.Dir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := protectRunPrivateState(cfg, spec, authorityMountAllowlist{}); err == nil {
		t.Fatal("controller job accepted a shared source-store ancestor")
	}
}

func TestAuthorityMountGuardAllowsOnlyScopedEnvironmentFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv(ServiceStateRootEnv, filepath.Join(root, "service-state"))
	planned := filepath.Join(root, "run", "scoped.env")
	unscoped := filepath.Join(root, "config", "env")
	for _, file := range []string{planned, unscoped} {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("VALUE=test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	allow := authorityMountAllowlist{envFiles: map[string]bool{planned: true}}
	if err := validateAuthorityMounts(context.Background(), RunSpec{},
		[]string{"--env-file", planned}, "", nil, allow); err != nil {
		t.Fatalf("planned scoped environment was refused: %v", err)
	}
	alias := filepath.Join(root, "repo", "switch.env")
	if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(planned, alias); err != nil {
		t.Fatal(err)
	}
	for _, options := range [][]string{{"--env-file", unscoped}, {"--env-file=" + unscoped}, {"--env-file", alias}} {
		if err := validateAuthorityMounts(context.Background(), RunSpec{}, options, "", nil, allow); err == nil {
			t.Fatalf("unscoped environment file was accepted: %v", options)
		}
	}
}

func TestAuthorityMountGuardAcceptsASymlinkedProjectParent(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "real", "project")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := forkspace.Workspace(repo, "worker")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	unclock, err := forkspace.LockState(repo, "worker")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "worker")
	unclock()
	if err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(parent, "alias")
	if err := os.Symlink(filepath.Join(parent, "real"), aliasParent); err != nil {
		t.Fatal(err)
	}
	aliasRepo := filepath.Join(aliasParent, "project")
	aliasWorkspace := forkspace.Workspace(aliasRepo, "worker")
	spec := RunSpec{Repo: aliasWorkspace, ForkName: identity.Name,
		ForkGeneration: string(identity.Generation), ActivityRepo: aliasRepo}
	if err := validateAuthorityMounts(context.Background(), spec,
		[]string{"-v", aliasWorkspace + ":/workspace"}, "", nil, authorityMountAllowlist{}); err != nil {
		t.Fatalf("symlinked spelling of the anchored fork was refused: %v", err)
	}
}
