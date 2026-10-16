//go:build providere2e

package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	hostruntime "runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/taskchannel"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type processTrace struct {
	Version     int                   `json:"version"`
	Sequence    int                   `json:"sequence"`
	Source      string                `json:"source"`
	Event       string                `json:"event"`
	PID         int                   `json:"pid"`
	Argv        []string              `json:"argv"`
	Cwd         string                `json:"cwd"`
	Run         *processRun           `json:"run"`
	Environment []processEnv          `json:"environment"`
	ExitCode    *int                  `json:"exit_code"`
	Signal      string                `json:"signal"`
	Consult     *processConsultTrace  `json:"consult"`
	Delegate    *processDelegateTrace `json:"delegate"`
}

type processConsultTrace struct {
	Step        int    `json:"step"`
	Provider    string `json:"provider"`
	Delivery    string `json:"delivery"`
	Result      string `json:"result"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	SessionHash string `json:"session_hash"`
	PromptHash  string `json:"prompt_hash"`
}

type processDelegateTrace struct {
	Step       int    `json:"step"`
	Provider   string `json:"provider"`
	Result     string `json:"result"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	PromptHash string `json:"prompt_hash"`
}

type processRun struct {
	Image        string         `json:"image"`
	Workdir      string         `json:"workdir"`
	HostWorkdir  string         `json:"host_workdir"`
	Provider     string         `json:"provider"`
	ProviderArgv []string       `json:"provider_argv"`
	Mounts       []processMount `json:"mounts"`
	Environment  []processEnv   `json:"environment"`
	EnvFiles     []string       `json:"env_files"`
	Labels       []string       `json:"labels"`
	Network      string         `json:"network"`
	Init         bool           `json:"init"`
	Interactive  bool           `json:"interactive"`
	TTY          bool           `json:"tty"`
}

type processMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	Named    bool   `json:"named"`
	// MCPServers are the server names a generated MCP config carried at launch time; coop removes
	// those files when the run ends, so the fixture reads them while they exist.
	MCPServers []string `json:"mcp_servers"`
}

type processEnv struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Redacted bool   `json:"redacted"`
}

// processLeaseAuthorityRoot is where a spawned coop resolves its task-authority registry: the
// durable state root under the fixture's own HOME, never a cache directory. In-process helpers that
// seed or inspect host records must point at exactly this path, or they write somewhere the child
// will never read.
func processLeaseAuthorityRoot(layout procharness.Layout) string {
	return filepath.Join(layout.Home, ".local", "state", "coop", "task-leases", tasks.LeaseAuthorityVersion)
}

type processLeaseMetadata struct {
	Version       int       `json:"version"`
	RunID         string    `json:"run_id"`
	ControllerPID int       `json:"controller_pid"`
	Provider      string    `json:"provider"`
	Target        string    `json:"target"`
	AcquiredAt    time.Time `json:"acquired_at"`
	HeartbeatAt   time.Time `json:"heartbeat_at"`
}

func processLeaseRecordPaths(t *testing.T, layout procharness.Layout, taskID string) (string, string) {
	t.Helper()
	key, err := tasks.LeaseAuthorityKey(filepath.Join(layout.Repo, tasksRoot), taskID)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(processLeaseAuthorityRoot(layout), key)
	return base + ".lock", base + ".json"
}

func assertProcessLeaseHeld(t *testing.T, layout procharness.Layout, taskID, provider, target string) {
	t.Helper()
	lockPath, metadataPath := processLeaseRecordPaths(t, layout, taskID)
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	locked := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if locked == nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		t.Fatal("loop task authority is not held by the controller")
	}
	if closeErr := lock.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(locked, syscall.EWOULDBLOCK) && !errors.Is(locked, syscall.EAGAIN) {
		t.Fatalf("probe loop task authority: %v", locked)
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta processLeaseMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Version != 1 || meta.RunID == "" || meta.ControllerPID <= 1 ||
		meta.Provider != provider || meta.Target != target ||
		meta.AcquiredAt.IsZero() || meta.HeartbeatAt.IsZero() {
		t.Fatalf("loop authority metadata = %+v, want %s on %s", meta, provider, target)
	}
}

func buildProcessBinary(t *testing.T, root, output, pkg string) {
	t.Helper()
	if err := fixtureBinaries.Copy(t.Context(), root, output, pkg); err != nil {
		t.Fatalf("build %s: %v", pkg, err)
	}
}

func installDockerRuntimeFixture(t *testing.T, fixtureBin string) string {
	t.Helper()
	runtimeBin := filepath.Join(filepath.Dir(fixtureBin), "docker")
	if err := os.Link(fixtureBin, runtimeBin); err != nil {
		t.Fatalf("install Docker runtime fixture: %v", err)
	}
	return runtimeBin
}

func initProcessRepo(t *testing.T, gitBin, repo string, env []string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Coop Fixture")
	run("config", "user.email", "fixture@invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/presets/\n.agent/runs/\n.agent/tasks/\n.delegate-ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md", ".gitignore")
	run("commit", "-q", "-m", "fixture root")
}

func controlledPath(paths ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, path := range append(paths, "/usr/bin", "/bin") {
		if path != "" && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

func readProcessTrace(t *testing.T, path string) []*processTrace {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var trace []*processTrace
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 128<<10)
	for scanner.Scan() {
		var event processTrace
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode process trace: %v\n%s", err, scanner.Bytes())
		}
		trace = append(trace, &event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return trace
}

// One box run's whole trace: three runtime invocations (image probe, daemon probe, run), the parsed
// run contract, provider start and exit, and the runtime's exit. A command that reaps orphaned boxes
// on its way in (loop, fork, build) adds exactly two runtime `ps` listings (boxes, then a dead run's
// own project services) and one `network` listing. A loop
// WORK box adds its task channel on top (box/taskchannel.go): the run-private volume is created,
// coop's helper container is started on it, the host checks the volume's backing exposure before
// launch, and the volume is removed after the box — four more runtime invocations.
type traceShape int

const (
	noOrphanBoxSweep  traceShape = iota // a direct provider run makes no runtime call of its own before `run`
	sweepsOrphanBoxes                   // this command reaps orphaned boxes and networks before it starts
	loopWorkBox                         // sweeps, then carries the task channel around the box run
)

const (
	directTraceEvents   = 7
	sweptTraceEvents    = directTraceEvents + 3
	loopWorkTraceEvents = sweptTraceEvents + 4
)

func (s traceShape) events() int {
	switch s {
	case sweepsOrphanBoxes:
		return sweptTraceEvents
	case loopWorkBox:
		return loopWorkTraceEvents
	}
	return directTraceEvents
}

func assertSequentialTrace(t *testing.T, trace []*processTrace, want int) {
	t.Helper()
	if len(trace) != want {
		t.Fatalf("process trace has %d events, want %d: %#v", len(trace), want, trace)
	}
	for i, event := range trace {
		if event.Version != 1 || event.Sequence != i+1 {
			t.Fatalf("trace event %d has version/sequence %d/%d", i, event.Version, event.Sequence)
		}
		if event.Event == "error" {
			t.Fatalf("trace contains runtime error event: %#v", event)
		}
	}
}

func assertDirectRuntimeInvocations(t *testing.T, trace []*processTrace, shape traceShape, provider string) {
	t.Helper()
	var got [][]string
	for _, event := range trace {
		if event.Source == "runtime" && event.Event == "invoke" {
			got = append(got, event.Argv)
		}
	}
	wantPrefix := [][]string{{"image", "inspect", "fixture-image"}}
	if shape != noOrphanBoxSweep {
		// The orphan sweep, before the box work begins: label-filtered container listings for boxes and
		// for a dead run's own project services, then one for the compose networks a dead session
		// leaves holding a subnet.
		wantPrefix = append(wantPrefix, []string{"ps", "<validated>"}, []string{"ps", "<validated>"}, []string{"network", "<validated>"})
	}
	wantPrefix = append(wantPrefix, []string{"info"})
	wantSuffix := [][]string{}
	if shape == loopWorkBox {
		// The task channel brackets the box: its volume and helper come up first (the box must
		// find the socket listening), and the volume goes once the box is gone.
		wantPrefix = append(wantPrefix, []string{"volume", "<validated>"}, []string{"run", "<task-channel>"}, []string{"volume", "<validated>"})
		wantSuffix = [][]string{{"volume", "<validated>"}}
	}
	run := len(wantPrefix)
	if len(got) != run+1+len(wantSuffix) || !reflect.DeepEqual(got[:run], wantPrefix) || len(got[run]) == 0 || got[run][0] != "run" ||
		!reflect.DeepEqual(got[run+1:], wantSuffix) {
		t.Fatalf("runtime invocations = %q, want %q then run then %q", got, wantPrefix, wantSuffix)
	}
}

// boxLabelsWithoutDynamicAuthorities drops the two runtime-generated authority labels after
// proving every box carries exactly one non-empty value for each. Their values are deliberately
// unpredictable, but their absence would make either orphan cleanup or project activity incomplete.
func boxLabelsWithoutDynamicAuthorities(t *testing.T, labels []string, provider string) []string {
	t.Helper()
	var kept []string
	counts := map[string]int{box.LabelHost: 0, box.LabelExecution: 0}
	for _, label := range labels {
		dynamic := false
		for key := range counts {
			prefix := key + "="
			if strings.HasPrefix(label, prefix) {
				if strings.TrimPrefix(label, prefix) == "" {
					t.Fatalf("box labels %q carry an empty %s", labels, key)
				}
				counts[key]++
				dynamic = true
				break
			}
		}
		if dynamic {
			continue
		}
		kept = append(kept, label)
	}
	for key, count := range counts {
		if count != 1 {
			t.Fatalf("box labels %q carry %d %s labels, want exactly one", labels, count, key)
		}
	}
	return kept
}

func oneProcessEvent(t *testing.T, trace []*processTrace, source, event string) *processTrace {
	t.Helper()
	var matches []*processTrace
	for _, record := range trace {
		if record.Source == source && record.Event == event {
			matches = append(matches, record)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("trace has %d %s/%s events, want one", len(matches), source, event)
	}
	return matches[0]
}

func processNativeHome(t *testing.T, layout procharness.Layout, provider, account, repo string) string {
	t.Helper()
	home, err := box.NativeHomePath(&config.Config{ConfigDir: layout.Config}, provider, account, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	return processTracePath(layout.Root, home)
}

func assertProcessMounts(t *testing.T, layout procharness.Layout, provider, account string, mounts []processMount) {
	assertProcessMountsAtTarget(t, layout, layout.Repo, processTracePath(layout.Root, layout.Repo), provider, account, mounts)
}

func assertProcessMountsAtTarget(t *testing.T, layout procharness.Layout, repo, repoTarget, provider, account string, mounts []processMount) {
	t.Helper()
	profileSource := processNativeHome(t, layout, provider, account, repo)
	repoSource := processTracePath(layout.Root, repo)
	profileTarget := "<container>/home/node/." + provider
	foundProfile, foundRepo := false, false
	for _, mount := range mounts {
		if mount.Named {
			// The run-private task channel (box/taskchannel.go) is the one named volume a box
			// mounts READ-ONLY: coop's helper made the socket, the box only connects to it.
			if strings.HasPrefix(mount.Source, "coop-tasks-") {
				if mount.Target != "<container>"+taskchannel.BoxSocketDir || !mount.ReadOnly {
					t.Errorf("task channel volume must be %s, read-only: %#v", taskchannel.BoxSocketDir, mount)
				}
				continue
			}
			if mount.Source != processCacheVolume("coop-cache") && mount.Source != processCacheVolume("coop-asdf") {
				t.Errorf("unknown named mount %#v", mount)
			}
			if mount.ReadOnly {
				t.Errorf("named volume must be writable: %#v", mount)
			}
			continue
		}
		if !recordedFixturePath(mount.Source) {
			t.Errorf("recorded mount escaped fixture root: %#v", mount)
		}
		if mount.Source == repoSource && mount.Target == repoTarget && !mount.ReadOnly {
			foundRepo = true
			continue
		}
		if mount.Source == profileSource && mount.Target == profileTarget && !mount.ReadOnly {
			foundProfile = true
			continue
		}
		if mount.Target == profileTarget && !mount.ReadOnly {
			t.Errorf("unexpected account mounted for %s: %#v", provider, mount)
			continue
		}
		if !mount.ReadOnly {
			t.Errorf("unexpected writable mount %#v", mount)
		} else if !recordedGeneratedMountPath(mount.Source) {
			t.Errorf("read-only mount did not come from generated fixture state: %#v", mount)
		}
	}
	if !foundRepo {
		t.Fatalf("writable repo mount %s:%s missing from %#v", repoSource, repoTarget, mounts)
	}
	if !foundProfile {
		t.Fatalf("profile mount %s:%s missing from %#v", profileSource, profileTarget, mounts)
	}
}

func processCacheVolume(name string) string {
	if hostruntime.GOOS != "linux" || os.Getuid() == 0 || (os.Getuid() == 1000 && os.Getgid() == 1000) {
		return name
	}
	return fmt.Sprintf("%s-%d-%d", name, os.Getuid(), os.Getgid())
}

func recordedGeneratedMountPath(path string) bool {
	if strings.HasPrefix(path, "<root>/tmp/coop-") {
		return true
	}
	rel := strings.TrimPrefix(path, "<root>/xdg/config/coop/runfiles/")
	if rel == path {
		return false
	}
	parts := strings.Split(rel, "/")
	return len(parts) == 2 && strings.HasPrefix(parts[0], "coop-run-") && strings.HasPrefix(parts[1], "coop-")
}

func recordedFixturePath(path string) bool {
	return path == "<root>" || strings.HasPrefix(path, "<root>/")
}

func processTracePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "<outside-root>"
	}
	if rel == "." {
		return "<root>"
	}
	return "<root>/" + filepath.ToSlash(rel)
}

func processTraceArgv(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		if i == 0 || processSafeFlag(arg) {
			out[i] = arg
			continue
		}
		digest := sha256.Sum256([]byte(arg))
		out[i] = fmt.Sprintf("<sha256:%x>", digest[:8])
	}
	return out
}

func processSafeFlag(value string) bool {
	if len(value) < 2 || len(value) > 64 || value[0] != '-' {
		return false
	}
	for _, r := range value[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func assertProcessEnvironment(t *testing.T, env []processEnv, provider, credentialKey string) {
	t.Helper()
	values := map[string]processEnv{}
	for _, item := range env {
		values[item.Name] = item
	}
	if values["COOP_PRIMARY"].Value != provider || values["FIXTURE_SAFE"].Value != "visible" {
		t.Fatalf("scoped environment = COOP_PRIMARY %#v FIXTURE_SAFE %#v", values["COOP_PRIMARY"], values["FIXTURE_SAFE"])
	}
	if _, ok := values[credentialKey]; ok {
		t.Fatalf("marker-backed profile leaked host %s into the box environment", credentialKey)
	}
}

func awaitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(wait.Deadline) // a fixture guard, not the behavior under test
	for procharness.ProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if procharness.ProcessAlive(pid) {
		t.Errorf("scripted process %d survived successful cleanup", pid)
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func readProcessFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	return string(data)
}
