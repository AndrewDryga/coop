//go:build cooplivetest

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

func TestLiveACPProcessPublishesBeforeReleaseAndKeepsStableLeader(t *testing.T) {
	root := t.TempDir()
	cmd, marker, recorded, release := liveACPHelperCommand(t, root, "blocked_release", true)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	waitForFile(t, recorded)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inner command started before registry publication: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "state", "live-process-groups"))
	if err != nil {
		t.Fatal(err)
	}
	finals := 0
	finalName := ""
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			finals++
			finalName = entry.Name()
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("published process record mode = %04o", info.Mode().Perm())
			}
		}
	}
	if finals != 1 {
		t.Fatalf("published process records = %d, want 1", finals)
	}
	data, err := os.ReadFile(filepath.Join(root, "state", "live-process-groups", finalName))
	if err != nil {
		t.Fatal(err)
	}
	var record liveprocess.Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Supervisor != "cleanup-owner" {
		t.Fatalf("record cleanup identity = %q, want cleanup-owner", record.Supervisor)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitCommand(cmd, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestLiveACPProcessPreservesEditorStdin(t *testing.T) {
	root := t.TempDir()
	cmd, _, recorded, _ := liveACPHelperCommand(t, root, "stdio", true)
	cmd.Stdin = strings.NewReader("editor-request\n")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(payload)); got != "editor-request" {
		t.Fatalf("inner ACP stdin = %q, want editor-request", got)
	}
}

func TestLiveACPProcessFailsClosedWithoutIdentity(t *testing.T) {
	root := t.TempDir()
	cmd, marker, _, _ := liveACPHelperCommand(t, root, "missing_token", true)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper rejected expected start failure: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inner command ran without stable identity: %v", err)
	}
}

func TestLiveACPProcessFailsClosedWithoutAuthenticatedControl(t *testing.T) {
	root := t.TempDir()
	cmd, marker, _, _ := liveACPHelperCommand(t, root, "invalid_control", false)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper rejected expected control failure: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inner command ran without authenticated control: %v", err)
	}
}

func TestLiveACPProcessFailsClosedWithoutCleanupIdentity(t *testing.T) {
	root := t.TempDir()
	cmd, marker, _, _ := liveACPHelperCommand(t, root, "missing_cleanup_id", true)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper rejected expected cleanup identity failure: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inner command ran without a cleanup identity: %v", err)
	}
}

func TestLiveACPProcessFailsClosedAtRegistryLimit(t *testing.T) {
	root := t.TempDir()
	cmd, marker, _, _ := liveACPHelperCommand(t, root, "full_registry", true)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper rejected expected registry admission failure: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inner command ran after registry admission failed: %v", err)
	}
}

func TestLiveACPProcessRegistryAdmissionIsSerialized(t *testing.T) {
	registry, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	var wg sync.WaitGroup
	results := make(chan error, liveprocess.MaxPublishedRecords+8)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func(pid int) {
			defer wg.Done()
			results <- publishLiveACPRecord(registry, liveprocess.Record{PID: pid})
		}(i + 2)
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != liveprocess.MaxPublishedRecords {
		t.Fatalf("concurrent process record admissions = %d, want %d", succeeded, liveprocess.MaxPublishedRecords)
	}
	entries, err := os.ReadDir(registry.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != liveprocess.MaxPublishedRecords {
		t.Fatalf("published process records = %d, want %d", len(entries), liveprocess.MaxPublishedRecords)
	}
}

func TestLiveACPProcessControlSurvivesSIGHUPReload(t *testing.T) {
	root := t.TempDir()
	cmd, marker, ready, _ := liveACPHelperCommand(t, root, "reload", true)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := waitCommand(cmd, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reloaded supervisor did not start a registered generation: %v", err)
	}
}

func TestLiveACPProcessHelper(t *testing.T) {
	if os.Getenv("COOP_TEST_LIVE_ACP_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	scenario := os.Getenv("COOP_TEST_LIVE_ACP_SCENARIO")
	if scenario == "reload" && os.Getenv("COOP_TEST_LIVE_ACP_GENERATION") == "" {
		registry, cleanupID, enabled, err := openLiveACPRegistry()
		if err != nil || !enabled {
			t.Fatalf("initial live registry validation = enabled %t, err %v", enabled, err)
		}
		if cleanupID != "cleanup-owner" {
			t.Fatalf("initial cleanup identity = %q", cleanupID)
		}
		registry.Close()
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		if err := os.WriteFile(os.Getenv("COOP_TEST_LIVE_ACP_RECORDED"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		<-hup
		restore, err := prepareACPReload()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Setenv("COOP_TEST_LIVE_ACP_GENERATION", "2"); err != nil {
			restore()
			t.Fatal(err)
		}
		if err := syscall.Exec(os.Args[0], os.Args, os.Environ()); err != nil {
			restore()
			t.Fatal(err)
		}
	}
	if scenario == "missing_token" {
		readLiveACPStartToken = func(int) string { return "" }
	}
	if scenario == "blocked_release" {
		recorded := os.Getenv("COOP_TEST_LIVE_ACP_RECORDED")
		release := os.Getenv("COOP_TEST_LIVE_ACP_RELEASE")
		beforeLiveACPRelease = func() {
			if err := os.WriteFile(recorded, nil, 0o600); err != nil {
				panic(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(release); err == nil {
					return
				}
				if time.Now().After(deadline) {
					panic("release was not published")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	if scenario == "full_registry" {
		registry := os.Getenv(liveprocess.ProcessDirEnv)
		for i := 0; i < liveprocess.MaxPublishedRecords; i++ {
			path := filepath.Join(registry, "occupied-"+strconv.Itoa(i)+".json")
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	marker := os.Getenv("COOP_TEST_LIVE_ACP_MARKER")
	innerScript := `
test ! -e /dev/fd/3 || exit 126
coop_pgid=$(ps -o pgid= -p $$) || exit 126
printf '%s' "$coop_pgid" > "$COOP_TEST_LIVE_ACP_MARKER"
`
	if scenario == "stdio" {
		innerScript = `
test ! -e /dev/fd/3 || exit 126
coop_pgid=$(ps -o pgid= -p $$) || exit 126
line=stdin-eof
IFS= read -r line || :
printf '%s' "$line" > "$COOP_TEST_LIVE_ACP_RECORDED"
printf '%s' "$coop_pgid" > "$COOP_TEST_LIVE_ACP_MARKER"
`
	}
	inner := exec.Command("/bin/sh", "-c", innerScript)
	inner.Env = os.Environ()
	if scenario == "stdio" {
		inner.Stdin = os.Stdin
	}
	inner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := startACPProcess(inner, "internal-supervisor")
	wantFailure := scenario == "missing_token" || scenario == "invalid_control" ||
		scenario == "missing_cleanup_id" || scenario == "full_registry"
	if wantFailure {
		if err == nil {
			_ = syscall.Kill(-inner.Process.Pid, syscall.SIGKILL)
			_ = inner.Wait()
			t.Fatal("unsafe live ACP process start succeeded")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, marker)
	rawPGID, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	innerPGID, err := strconv.Atoi(strings.TrimSpace(string(rawPGID)))
	if err != nil || innerPGID != inner.Process.Pid {
		t.Fatalf("inner process group = %q, want registered group %d", rawPGID, inner.Process.Pid)
	}
	time.Sleep(50 * time.Millisecond) // the inner command exits; the resident group leader must not.
	token := processidentity.StartToken(inner.Process.Pid)
	if processidentity.Inspect(inner.Process.Pid, token) != processidentity.Match {
		t.Fatal("resident live ACP group leader exited with the inner command")
	}
	_ = syscall.Kill(-inner.Process.Pid, syscall.SIGKILL)
	if err := inner.Wait(); err == nil {
		t.Fatal("killed live ACP wrapper exited successfully")
	}
}

func liveACPHelperCommand(t *testing.T, root, scenario string, validControl bool) (*exec.Cmd, string, string, string) {
	t.Helper()
	state := filepath.Join(root, "state")
	registry := filepath.Join(state, "live-process-groups")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	controlPath := filepath.Join(root, "control")
	controlData := liveprocess.ControlMarker
	if !validControl {
		controlData = "invalid\n"
	}
	if err := os.WriteFile(controlPath, []byte(controlData), 0o600); err != nil {
		t.Fatal(err)
	}
	control, err := os.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	marker := filepath.Join(root, "inner-started")
	recorded := filepath.Join(root, "recorded")
	release := filepath.Join(root, "release")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLiveACPProcessHelper$")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=" + root, "XDG_STATE_HOME=" + state,
		liveprocess.ControlFDEnv + "=3", liveprocess.ProcessDirEnv + "=" + registry,
		"COOP_TEST_LIVE_ACP_HELPER=1", "COOP_TEST_LIVE_ACP_SCENARIO=" + scenario,
		"COOP_TEST_LIVE_ACP_MARKER=" + marker, "COOP_TEST_LIVE_ACP_RECORDED=" + recorded,
		"COOP_TEST_LIVE_ACP_RELEASE=" + release,
	}
	if scenario != "missing_cleanup_id" {
		cmd.Env = append(cmd.Env, liveprocess.CleanupIDEnv+"=cleanup-owner")
	}
	cmd.ExtraFiles = []*os.File{control}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	return cmd, marker, recorded, release
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for helper control file")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitCommand(cmd *exec.Cmd, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return errors.New("live ACP helper timed out")
	}
}
