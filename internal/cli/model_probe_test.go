package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestModelProbeAdmissionPreservesPolicyAndIsolation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct{ name, host, policy, wantMode, wantError string }{
		// Nothing chose a mode, so the probe gets the filtered default; this runtime cannot serve it.
		{"implicit filtered", "", "", "filtered", "restricted networking needs docker"},
		{"explicit open", "open", "", "open", ""},
		{"project offline", "", "box:\n  egress: offline\n", "none", ""},
		{"project filtered", "", "box:\n  egress: filtered\n", "filtered", "restricted networking needs docker"},
		{"pending grant", "", "box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: example.com}\n      protocol: tls\n      ports: [443]\n", "filtered", "coop approve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(tc.policy), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "filtered"} // config.Load's unset value
			if tc.host != "" {
				cfg.SetEgress(tc.host) // the operator's COOP_EGRESS
			}
			hostMode, hostChose := cfg.Egress, cfg.Explicit("COOP_EGRESS")
			cfg.SetActiveProfile("claude", "selected")
			a := &app{cfg: cfg, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true,
				acpPeers: []agents.Target{{Provider: "gemini"}}, acpCapture: &box.CapturedEgress{Fingerprint: "must-not-borrow"}}
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					probe, target, err := a.admitModelProbe(t.Context(), "claude", repo, io.Discard)
					if (err != nil) != (tc.wantError != "") || err != nil && !strings.Contains(err.Error(), tc.wantError) {
						t.Errorf("admission error = %v, want %q", err, tc.wantError)
					}
					if err == nil {
						defer probe.acpCapture.Close()
					}
					if probe.cfg.Egress != tc.wantMode || !probe.cfg.Explicit("COOP_EGRESS") && tc.wantError == "" {
						t.Errorf("resolved mode = %q", probe.cfg.Egress)
					}
					if target.String() != "claude@selected" || len(probe.acpNetworkTargets) != 1 || probe.acpNetworkTargets[0].String() != target.String() {
						t.Errorf("scope = %v, target = %v", probe.acpNetworkTargets, target)
					}
					if len(probe.acpPeers) != 0 || probe.preset != nil || probe.acpCapture == a.acpCapture {
						t.Error("probe borrowed editor state")
					}
					probe.cfg.SetActiveProfile("claude", "private")
				})
			}
			wg.Wait()
			if cfg.Egress != hostMode || cfg.Explicit("COOP_EGRESS") != hostChose || cfg.ActiveProfile("claude") != "selected" {
				t.Fatal("admission mutated shared parent config")
			}
		})
	}
}

// A catalog refresh only needs an answer, so it must not start a project build that can take many
// minutes, even of files a person approved.
func TestModelProbeRefusesToBuildAProject(t *testing.T) {
	var got box.NetworkAdmission
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true,
		admitNetwork: func(_ *config.Config, _ runtime.Runtime, _ box.RunSpec, options box.NetworkAdmission) (*box.CapturedEgress, error) {
			got = options
			return nil, nil
		}}
	if _, _, err := a.admitModelProbe(t.Context(), "claude", t.TempDir(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got.ProjectBuild != box.RefuseProjectBuild {
		t.Fatalf("a model probe admitted with %+v", got)
	}
}

// claudeWork is the account the probes below run on.
var claudeWork = agents.Target{Provider: "claude", Accounts: []string{"work"}}

// A sign-in the probe hits names the probe's own account, in the words coop credentials uses. The
// "Sign in again" detail is what the probe's box prints when the provider refuses the saved sign-in.
func TestModelProbeFailuresAreSafeAndUseful(t *testing.T) {
	const secret = "SECRET-CANARY-token-value"
	for _, tc := range []struct{ detail, want string }{
		{"image \"private\" is not built; coop build " + secret, "The Coop box image is not built: coop build"},
		{"Review it: coop approve " + secret, "Project access needs review: coop approve"},
		{"restricted networking needs docker " + secret, "Filtered networking requires Docker: set COOP_RUNTIME=docker"},
		{"run coop build --egress filtered " + secret, "The filtered project image needs rebuilding: coop build --egress filtered"},
		{"run coop net setup " + secret, "Filtered networking needs host setup: coop net setup"},
		{"no space left on device " + secret, "The container runtime has no free storage."},
		{"canonical credential file must be owner-private with one link " + secret, "Credential storage failed a safety check; preserve it for host recovery."},
		{"native account needs host sign-in or renewal " + secret, "Sign in again: coop login claude@work"},
		{"Sign in again: coop login claude@work " + secret, "Sign in again: coop login claude@work"},
		{secret + "\x1b[31m", "The model probe closed before answering."},
	} {
		cause := modelFetchCause("claude", modelProbeFailure(claudeWork, io.EOF, tc.detail))
		if cause != tc.want || strings.Contains(cause, secret) || strings.ContainsAny(cause, "\n\x1b") {
			t.Errorf("cause = %q, want %q", cause, tc.want)
		}
	}
	if got := modelFetchCause("claude", errors.New(secret)); got != "The model request failed." {
		t.Fatal(got)
	}
	if got := modelFetchCause("claude", modelProbeFailure(claudeWork, fmt.Errorf("setup: %w", box.ErrNetworkSetupFailed), secret)); got != "Filtered networking setup failed; run coop net setup for details." {
		t.Fatal(got)
	}
	if got := modelFetchCause("claude", modelProbeFailure(claudeWork, context.DeadlineExceeded, secret)); got != "The model request timed out." {
		t.Fatal(got)
	}
	buffer := &tailBuffer{max: 16 << 10}
	_, _ = buffer.Write([]byte(strings.Repeat(secret, 2000)))
	if len(buffer.String()) != 16<<10 {
		t.Fatal("stderr collector is not bounded")
	}
}

func TestModelProbeSetupNoticeDoesNotExposeTranscript(t *testing.T) {
	private := &tailBuffer{max: 100}
	writer := &modelSetupOutput{Writer: private, agent: "claude"}
	out := captureStderr(t, func() {
		_, _ = writer.Write([]byte("SECRET-CANARY build output\n"))
		_, _ = writer.Write([]byte("more private output\n"))
	})
	if out != "Preparing filtered networking for Claude model discovery…\n" || !strings.Contains(private.String(), "SECRET-CANARY") {
		t.Fatalf("notice = %q, private tail = %q", out, private.String())
	}
}

func TestModelProbeChildExitCollectsLateStderr(t *testing.T) {
	repo := t.TempDir()
	shim := filepath.Join(repo, "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec 1>&-\nprintf '%s\\n' 'image \"x\" is not built; coop build; SECRET-CANARY' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo}}
	stderr := &tailBuffer{max: 16 << 10}
	child, err := a.spawnBox(t.Context(), shim, nil, "model-diagnostics", nil, agents.Target{Provider: "claude"}, "", true, stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Stop()
	_, err = acpModelHandshake(t.Context(), child, repo, agents.ModelCatalogSpec{})
	if err == nil {
		t.Fatal("closed child succeeded")
	}
	select {
	case <-child.WaitDone:
	case <-time.After(wait.Deadline):
		t.Fatal("child exit did not complete stderr collection")
	}
	if got := modelFetchCause("claude", modelProbeFailure(claudeWork, err, stderr.String())); got != "The Coop box image is not built: coop build" {
		t.Fatal(got)
	}
}

func TestModelProbeFilteredChildCarriesOnlySelectedAccount(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir(), Egress: "filtered"}
	cfg.SetEgress("filtered")
	cfg.SetActiveProfile("claude", "selected")
	seedNativeFixture(t, cfg, "claude", "selected")
	target := agents.Target{Provider: "claude", Accounts: []string{"selected"}}
	probe := &app{cfg: cfg, acpNetworkTargets: []agents.Target{target}, acpCapture: &box.CapturedEgress{
		Project: cfg.RepoOverride, Fingerprint: strings.Repeat("a", 64), QualificationID: strings.Repeat("b", 64),
	}}
	shim := filepath.Join(t.TempDir(), "inner")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' \"$COOP_EGRESS\" \"$COOP_ACP_TARGET\" \"$COOP_ACP_ACCOUNTS\" \"$COOP_NETWORK_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	child, err := probe.spawnBox(t.Context(), shim, nil, "catalog-scope", nil, target, "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(child.Out)
	child.Stop()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.HasPrefix(text, "filtered\nclaude@selected\n") || !strings.Contains(text, `"claude":{"account":"selected","default":"default"}`) || !strings.Contains(text, `"session_id":"acp-catalog-scope"`) || strings.Contains(text, "gemini") {
		t.Fatalf("child scope = %s", out)
	}
	other := agents.Target{Provider: "claude", Accounts: []string{"sibling"}}
	if child, err := probe.spawnBox(t.Context(), shim, nil, "catalog-other", nil, other, "", true, io.Discard); err == nil {
		child.Stop()
		t.Fatal("unadmitted sibling account started")
	}
}

func TestModelProbeRPCFailureNeverCachesPayload(t *testing.T) {
	r := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"message\":\"SECRET-CANARY\"}}\n")
	child := &acpproxy.Child{In: nopWriteCloser{Writer: io.Discard}, Out: r}
	_, err := acpModelHandshake(t.Context(), child, "/repo", agents.ModelCatalogSpec{})
	a := modelsApp(t)
	if err := writeModelsCache(a.cfg, "claude", []agents.Model{{ID: "last-good"}}); err != nil {
		t.Fatal(err)
	}
	a.acpModels = func(string) ([]agents.Model, error) { return nil, err }
	cause, failed := a.refreshCatalog("claude", nil)
	cache, _ := loadModelsCache(a.cfg, "claude")
	if !failed || cause != "The agent rejected the model discovery request." || cache.AttemptError != cause || cache.Models[0].ID != "last-good" || strings.Contains(err.Error(), "SECRET-CANARY") {
		t.Fatalf("error %v, cache %+v", err, cache)
	}
}

// A refresh that Coop's own launch check stops, as an mcp.json inside the folder does, says what
// coop run says there and leaves the cache as it was: no provider was asked, and every folder reads
// the agent's cache.
func TestModelsRefreshShowsTheLaunchRefusalAndKeepsTheCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	folder := t.TempDir() // not a Git repository, as in the report
	source := filepath.Join(folder, "mcp.json")
	if err := os.WriteFile(source, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: folder, MCPFile: source, Homes: true, Egress: "filtered"}
	seedNativeFixture(t, cfg, "claude", config.DefaultProfile)
	if err := writeModelsCache(cfg, "claude", []agents.Model{{ID: "last-good"}}); err != nil {
		t.Fatal(err)
	}
	before := readFileString(modelsCachePath(cfg, "claude"))
	a := &app{cfg: cfg, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true}
	out := captureStdout(t, func() {
		if code, err := a.cmdModels([]string{"claude", "--refresh"}); code != 0 || err != nil {
			t.Fatalf("cmdModels = (%d, %v)", code, err)
		}
	})
	repo, err := box.ResolveRepo(folder)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("  ⚠ Could not refresh. Showing the list saved just now\n\n"+
		"      mcp.json source %q is inside mounted repository %q; move the file and set COOP_MCP_FILE to a path outside directories exposed to agents\n", source, repo)
	if !strings.Contains(out, want) || !strings.Contains(out, "last-good") {
		t.Errorf("menu does not show the launch refusal under the saved list; want %q in:\n%s", want, out)
	}
	if after := readFileString(modelsCachePath(cfg, "claude")); after != before {
		t.Errorf("a launch refusal changed the cache:\nbefore %s\nafter  %s", before, after)
	}
}

// Coop's own refusal is shown escaped, as any Coop error is, and never recorded. A fixed remedy
// still says it shorter. A failed host setup and a provider failure keep their lines and the
// backoff, since a retry would pay for them again.
func TestModelProbeRefusalIsShownAndNeverRecorded(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct {
		name     string
		admit    error // what the probe's admission returns; nil fails in the provider instead
		want     string
		recorded bool
	}{
		{"coop check", errors.New("project file \x1b[31mbroken\n  line 3: unknown key"), "project file \\u001B[31mbroken\n  line 3: unknown key", false},
		{"fixed remedy", errors.New("coop claude cannot start because a request is pending\n\n  Review it: coop approve /repo"), "Project access needs review: coop approve", false},
		{"host setup", fmt.Errorf("coop claude cannot start — %w: smoke failed", box.ErrNetworkSetupFailed), "Filtered networking setup failed; run coop net setup for details.", true},
		{"stopped", fmt.Errorf("admission: %w", context.Canceled), "The model request was cancelled.", true},
		{"provider", nil, "The model request failed.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
			seedNativeFixture(t, cfg, "claude", config.DefaultProfile)
			a := &app{cfg: cfg, rt: runtime.Runtime{Name: "must-not-execute"}, rtSet: true,
				admitNetwork: func(*config.Config, runtime.Runtime, box.RunSpec, box.NetworkAdmission) (*box.CapturedEgress, error) {
					return nil, tc.admit
				}}
			if tc.admit == nil {
				a.acpModels = func(string) ([]agents.Model, error) { return nil, errors.New("provider said SECRET-CANARY") }
			}
			cause, failed := a.refreshCatalog("claude", nil)
			cache, _ := loadModelsCache(cfg, "claude")
			if !failed || cause != tc.want {
				t.Errorf("refresh = (%q, %v), want (%q, true)", cause, failed, tc.want)
			}
			if recorded := !cache.AttemptedAt.IsZero(); recorded != tc.recorded || recorded && cache.AttemptError != tc.want {
				t.Errorf("recorded %+v, want recorded=%v", cache, tc.recorded)
			}
		})
	}
	// The menu keeps every line of a refusal in the cause block.
	a := modelsApp(t)
	a.acpModels = func(string) ([]agents.Model, error) {
		return nil, modelProbeRefusal(claudeWork, errors.New("first line\nsecond line"), "")
	}
	out := captureStdout(t, func() { _, _ = a.cmdModels([]string{"claude", "--refresh"}) })
	if !strings.Contains(out, "\n      first line\n      second line\n") {
		t.Errorf("a refusal's second line left the cause block:\n%s", out)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// A probe removes its boxes either way; only a filtered probe, whose admitted policy may bind
// services it had to start, removes its own private Compose project too, and only after its boxes
// are gone: when they could not be removed, the services stay with them. An open probe starts none.
func TestModelProbeCleanupRemovesAFilteredProbesServices(t *testing.T) {
	repo := t.TempDir()
	boxes := "label=" + box.LabelSupervisor + "=probe-supervisor"
	project := "label=com.docker.compose.project=" + box.ComposeProjectFor(repo, "probe-supervisor")
	for _, filtered := range []bool{false, true} {
		calls := filepath.Join(t.TempDir(), "calls")
		a := &app{cfg: &config.Config{}, rt: recordingRuntime(t, calls), rtSet: true}
		_ = a.cleanupModelProbe(repo, "probe-supervisor", filtered)
		got := readFileString(calls)
		at, servicesAt := strings.Index(got, boxes), strings.Index(got, project)
		if at < 0 {
			t.Fatalf("filtered=%v: the probe's boxes were not removed:\n%s", filtered, got)
		}
		if (servicesAt >= 0) != filtered || filtered && servicesAt < at {
			t.Fatalf("filtered=%v: services removed at %d, boxes at %d:\n%s", filtered, servicesAt, at, got)
		}
	}

	calls := filepath.Join(t.TempDir(), "calls")
	shim := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(calls) + "\ncase \"$*\" in *" + boxes + "*) exit 1 ;; esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}, rt: runtime.Runtime{Name: shim}, rtSet: true}
	if err := a.cleanupModelProbe(repo, "probe-supervisor", true); err == nil {
		t.Fatal("a failed box removal was not reported")
	}
	if got := readFileString(calls); strings.Contains(got, project) {
		t.Fatalf("the services went while a box may still use them:\n%s", got)
	}
}
