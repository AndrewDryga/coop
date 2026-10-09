package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	for _, tc := range []struct{ name, policy, wantMode, wantError string }{
		{"implicit open", "", "open", ""},
		{"project offline", "box:\n  egress: offline\n", "none", ""},
		{"project filtered", "box:\n  egress: filtered\n", "filtered", "restricted networking needs docker"},
		{"pending grant", "box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: example.com}\n      protocol: tls\n      ports: [443]\n", "open", "coop approve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(tc.policy), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "open"}
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
			if cfg.Egress != "open" || cfg.Explicit("COOP_EGRESS") || cfg.ActiveProfile("claude") != "selected" {
				t.Fatal("admission mutated shared parent config")
			}
		})
	}
}

func TestModelProbeFailuresAreSafeAndUseful(t *testing.T) {
	const secret = "SECRET-CANARY-token-value"
	for _, tc := range []struct{ detail, want string }{
		{"image \"private\" is not built; coop build " + secret, "The Coop box image is not built: coop build"},
		{"Review it: coop approve " + secret, "Project access needs review: coop approve"},
		{"restricted networking needs docker " + secret, "Filtered networking requires Docker: set COOP_RUNTIME=docker"},
		{"run coop build --egress filtered " + secret, "The filtered project image needs rebuilding: coop build --egress filtered"},
		{"run coop net setup " + secret, "Filtered networking needs host setup: coop net setup"},
		{"no space left on device " + secret, "The container runtime has no free storage."},
		{"legacy Gemini encrypted cache has no recorded storage identity " + secret, "The selected account needs host recovery; its legacy encrypted cache is preserved."},
		{"legacy encrypted cache has no recorded storage identity " + secret, "The selected account needs host recovery; its legacy encrypted cache is preserved."},
		{"legacy writer inventory is unavailable " + secret, "Docker could not verify existing credential mounts; retry when Docker is ready."},
		{"canonical credential file must be owner-private with one link " + secret, "Credential storage failed a safety check; preserve it for host recovery."},
		{"legacy native credential writer is busy " + secret, "An existing session is using this account; finish it before retrying."},
		{"native account needs host sign-in or renewal " + secret, "Sign in to Claude to refresh its models: coop login claude"},
		{secret + "\x1b[31m", "The model probe closed before answering."},
	} {
		cause := modelFetchCause("claude", modelProbeFailure("claude", io.EOF, tc.detail))
		if cause != tc.want || strings.Contains(cause, secret) || strings.ContainsAny(cause, "\n\x1b") {
			t.Errorf("cause = %q, want %q", cause, tc.want)
		}
	}
	if got := modelFetchCause("claude", errors.New(secret)); got != "The model request failed." {
		t.Fatal(got)
	}
	if got := modelFetchCause("claude", modelProbeFailure("claude", fmt.Errorf("setup: %w", box.ErrNetworkSetupFailed), secret)); got != "Filtered networking setup failed; run coop net setup for details." {
		t.Fatal(got)
	}
	if got := modelFetchCause("claude", modelProbeFailure("claude", context.DeadlineExceeded, secret)); got != "The model request timed out." {
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
	_, err = acpModelHandshake(t.Context(), child, repo)
	if err == nil {
		t.Fatal("closed child succeeded")
	}
	select {
	case <-child.WaitDone:
	case <-time.After(wait.Deadline):
		t.Fatal("child exit did not complete stderr collection")
	}
	if got := modelFetchCause("claude", modelProbeFailure("claude", err, stderr.String())); got != "The Coop box image is not built: coop build" {
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
	if _, err := applyACPAccountBindings(cfg.Clone(), `{"claude":{"account":"selected","default":"changed"}}`); err == nil {
		t.Fatal("child accepted default-account drift")
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
	_, err := acpModelHandshake(t.Context(), child, "/repo")
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

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
