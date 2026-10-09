package box

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestUsageCredentialLeaseCoordinatesOriginalHome(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	home := t.TempDir()
	first, err := credentialUseLease(context.Background(), cfg, home, false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := credentialUseLease(context.Background(), cfg, home, false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if unlock, err := credentialUseLease(ctx, cfg, alias, true); err == nil {
		unlock.close()
		t.Fatal("writer passed two active original-home readers")
	}
	other, err := credentialUseLease(context.Background(), cfg, t.TempDir(), true)
	if err != nil {
		t.Fatal("unrelated credential blocked", err)
	}
	other.close()
	first.close()
	second.close()
	writer, err := credentialUseLease(context.Background(), cfg, alias, true)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.close()
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if unlock, err := credentialUseLease(cancelled, cfg, home, false); err == nil {
		unlock.close()
		t.Fatal("cancelled reader acquired a lease")
	}
}

func TestUsageNativeCleanupFailureKeepsOriginalCredentialBusy(t *testing.T) {
	ag, _ := agents.Get("gemini")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	profile := cfg.AgentProfileDir(ag.Name(), "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "oauth_creds.json"), []byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	alive := filepath.Join(t.TempDir(), "alive")
	t.Setenv("COOP_USAGE_FIXTURE_ALIVE", alive)
	docker := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
case "$1" in
ps) if [ -f "$COOP_USAGE_FIXTURE_ALIVE" ]; then printf 'fixture-container\n'; fi ;;
inspect) printf '{}\n' ;;
image) printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' ;;
run) touch "$COOP_USAGE_FIXTURE_ALIVE"; printf '{"tier":"fixture","buckets":[]}\n' ;;
rm) exit 1 ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: docker}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ReadUsageQuotaNative(ctx, cfg, rt, ag, "work", []string{"/usr/local/bin/node", "fixed"}); err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cleanup failure=%v", err)
	}
	lease, err := credentialUseLease(ctx, cfg, profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	if err := checkCredentialContainers(ctx, rt, ag.Name(), lease.key, false); err == nil {
		t.Fatal("new reader allowed a surviving helper")
	}
	if err := checkCredentialContainers(ctx, rt, ag.Name(), lease.key, true); err == nil {
		t.Fatal("new writer allowed surviving runtime")
	}
}

func TestOfflineNativeHomesDoNotLeaseRetiredCredentialProfiles(t *testing.T) {
	for _, name := range agents.Names() {
		t.Run(name, func(t *testing.T) {
			cfg := (&config.Config{ConfigDir: t.TempDir()}).WithNativeHomes(map[string]string{name: t.TempDir()})
			spec := RunSpec{Ctx: t.Context(), Agent: name, Homes: true}
			release, err := runCredentialUseLeases(cfg, runtime.Runtime{Name: "docker"}, &spec)
			if err != nil {
				t.Fatal(err)
			}
			home, err := cfg.NativeHome(name)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			if exclusive, err := credentialUseLease(ctx, cfg, home, true); err == nil {
				exclusive.close()
				t.Error("offline history home has no shared lease")
			}
			cancel()
			release()
			exclusive, err := credentialUseLease(t.Context(), cfg, home, true)
			if err != nil {
				t.Fatal(err)
			}
			exclusive.close()
			if len(spec.ExtraArgs) != 0 {
				t.Fatal("native home inherited a legacy credential lease", spec.ExtraArgs)
			}
		})
	}
}

func TestUsageSurvivingLoginWriterBlocksOrdinaryRun(t *testing.T) {
	t.Setenv("COOP_USAGE_WRITER", "0")
	cfg := &config.Config{ConfigDir: t.TempDir()}
	cfg.SetActiveProfile("gemini", "work")
	if err := os.MkdirAll(cfg.AgentProfileDir("gemini", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
case "$1" in
ps) case "$*" in *label=coop.credential.writer=*) if [ "$COOP_USAGE_WRITER" = 1 ]; then printf 'leftover-login\n'; fi ;; esac ;;
inspect) printf '{}\n' ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: docker}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	login := RunSpec{Ctx: ctx, Agent: "gemini", Homes: true, Login: true}
	release, err := runCredentialUseLeases(cfg, rt, &login)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !strings.Contains(strings.Join(login.ExtraArgs, " "), "coop.credential.writer=") {
		t.Fatal("exclusive login was not labeled as a credential writer")
	}
	t.Setenv("COOP_USAGE_WRITER", "1")
	run := RunSpec{Ctx: ctx, Agent: "gemini", Homes: true}
	if release, err := runCredentialUseLeases(cfg, rt, &run); err == nil {
		release()
		t.Fatal("ordinary run admitted while an orphaned login could rewrite credentials")
	}
}

func TestUsageNativeHelperPlanHasOnlySelectedAuthority(t *testing.T) {
	ag, _ := agents.Get("gemini")
	cfg := &config.Config{HomeInBox: "/home/node", BaseImage: "untrusted-project-image"}
	command := []string{"/usr/local/bin/node", "--input-type=module", "-e", "fixed-code"}
	args := usageQuotaNativeArgs(cfg, ag.Name(), "/original/selected", "sha256:exact", "owned", "canonical", command, ag.Usage().NativeEnv)
	joined := strings.Join(args, " ")
	for _, denied := range []string{"untrusted-project-image", "--env-file", "MCP", "--privileged", "/workspace", "oauth-copy"} {
		if strings.Contains(joined, denied) {
			t.Fatalf("helper inherited %s", denied)
		}
	}
	for _, required := range []string{"--read-only", "--cap-drop=ALL", "--pids-limit=128", "--cpus=1", "--memory=512m", "HOME=/home/node", "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE=false", "type=bind,src=/original/selected,dst=/home/node/.gemini", "coop.credential.writer=canonical"} {
		if !slices.Contains(args, required) {
			t.Fatalf("missing %s: %v", required, args)
		}
	}
	if !slices.Equal(args[len(args)-4:], []string{"sha256:exact", "--input-type=module", "-e", "fixed-code"}) {
		t.Fatal("helper command/image changed")
	}
	var out quotaOutput
	_, _ = out.Write(make([]byte, (1<<20)+3))
	_, _ = out.Write([]byte("more"))
	if !out.exceeded || out.Len() != 1<<20 {
		t.Fatal("native output was not bounded")
	}
}
