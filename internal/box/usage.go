package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Quota inspection selects the same authority as a run but does not impose the filtered API-key
// broker's separate network/support restrictions. It never returns credential values to CLI.
func UsageQuotaAuthority(cfg *config.Config, provider, account string) (agents.UsageQuotaInput, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return agents.UsageQuotaInput{}, errors.New("unknown quota provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	selected, exists, err := previewNativeAuthority(ctx, cfg, ag, account)
	if err != nil {
		return agents.UsageQuotaInput{}, err
	}
	if exists && (selected.canonical || selected.record == nil) {
		input := agents.UsageQuotaInput{APIKey: selected.state.APIKey}
		if selected.canonical {
			input.Current = func(ctx context.Context, deadline time.Time) (agents.NativeCredentialState, map[string][]byte, error) {
				record, err := renewNativeAccount(ctx, cfg, ag, account, deadline)
				if err != nil {
					return agents.NativeCredentialState{}, nil, err
				}
				state, err := ag.NativeCredentials().Inspect(record.Artifacts, time.Now())
				return state, record.Artifacts, err
			}
		}
		return input, nil
	}
	home := cfg.AgentProfileDir(provider, account)
	input := agents.UsageQuotaInput{ProfileDir: home}
	marker := ProfileMarkerPresent(cfg, provider, account)
	values, err := effectiveCredentialEnv(cfg, RunSpec{Homes: true}, ag, provider, account, marker)
	if err != nil {
		return input, errors.New("selected authentication authority is unavailable")
	}
	for _, key := range ag.ActiveCredentialEnvKeys(home, marker) {
		if strings.TrimSpace(values[key]) == "" {
			continue
		}
		if input.EnvKey != "" {
			return input, errors.New("more than one authentication authority is selected")
		}
		input.EnvKey = key
		input.APIKey = true
	}
	if input.EnvKey == "" && marker {
		if detector, ok := ag.(agents.StoredAPIKeyDetector); ok {
			input.APIKey, err = detector.StoredAPIKey(home)
			if err != nil {
				return input, errors.New("selected authentication authority is unavailable")
			}
		}
	}
	return input, nil
}

// Native OAuth refresh rotates the original store. Keep usage's writer out of ordinary Coop
// runs (including mounted consult peers), without copying refresh authority into another home.
type credentialLease struct {
	file *os.File
	key  string
}

func (lease *credentialLease) close() {
	_ = syscall.Flock(int(lease.file.Fd()), syscall.LOCK_UN)
	_ = lease.file.Close()
}

func credentialUseLease(ctx context.Context, cfg *config.Config, profile string, exclusive bool) (*credentialLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	canonical, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return nil, errors.New("credential home is unavailable")
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, errors.New("credential home is unavailable")
	}
	dir := filepath.Join(cfg.ConfigDir, "credential-use")
	if err := config.EnsurePrivateDir(dir); err != nil {
		return nil, errors.New("credential lease is unavailable")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	path := filepath.Join(dir, key+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, errors.New("credential lease is unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("credential lease is unsafe")
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	for {
		if ctx.Err() != nil {
			file.Close()
			return nil, errors.New("credential is busy — try again after the active run")
		}
		err = syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			current, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(info, current) {
				file.Close()
				return nil, errors.New("credential lease changed while opening")
			}
			return &credentialLease{file: file, key: key}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, errors.New("credential lease is unavailable")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func runCredentialUseLeases(cfg *config.Config, rt runtime.Runtime, spec *RunSpec) (func(), error) {
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	// A pending retained-history index import may replace its repository-local
	// index only while no native client is using that complete home.
	if spec.Homes && !spec.Login && !spec.Mode.Restricted() {
		ctx := spec.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		for _, name := range credentialScope(cfg, *spec) {
			home, err := cfg.NativeHome(name)
			if err != nil {
				continue
			}
			lease, err := credentialUseLease(ctx, cfg, home, false)
			if err != nil {
				release()
				return nil, err
			}
			releases = append(releases, lease.close)
			if err := checkNativeHistorySettled(home); err != nil {
				release()
				return nil, err
			}
		}
	}
	if !rt.SupportsRestrictedFilesystem() {
		return release, nil
	}
	for _, name := range credentialScope(cfg, *spec) {
		if spec.native.selected(name) {
			continue
		}
		if _, err := cfg.NativeHome(name); err == nil && !spec.Login {
			// Offline native homes contain no account authority either. Their
			// shared history lease above replaces the retired profile lease.
			continue
		}
		ag, _ := agents.Get(name)
		if !ag.Usage().NativeCredentialLease {
			continue
		}
		lease, err := credentialUseLease(spec.Ctx, cfg, cfg.AgentProfileDir(name, cfg.ActiveProfile(name)), spec.Login)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, lease.close)
		if err := checkCredentialContainers(spec.Ctx, rt, name, lease.key, spec.Login); err != nil {
			release()
			return nil, err
		}
		spec.ExtraArgs = append(spec.ExtraArgs, "--label", "coop.credential."+name+"="+lease.key)
		if spec.Login {
			spec.ExtraArgs = append(spec.ExtraArgs, "--label", "coop.credential.writer="+lease.key)
		}
	}
	return release, nil
}

// A failed teardown can outlive its host process/flock. Container labels keep that original
// store busy until absence is proven; include created/stopped containers that could restart.
func checkCredentialContainers(ctx context.Context, rt runtime.Runtime, provider, key string, exclusive bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	label := "coop.credential.writer"
	if exclusive {
		label = "coop.credential." + provider
	}
	containers, err := rt.ContainersByLabel(ctx, label, key)
	if err != nil {
		return errors.New("credential runtime ownership is unavailable — try again")
	}
	if len(containers) > 0 {
		return errors.New("credential is busy — its previous runtime has not exited")
	}
	return nil
}

// ReadUsageQuotaNative is a maintenance helper, not a model run: no repository, generated
// instructions, MCP, configurable image or inherited environment enters the process.
func ReadUsageQuotaNative(ctx context.Context, cfg *config.Config, rt runtime.Runtime, ag agents.Agent, profile string, command []string) ([]byte, error) {
	if !rt.SupportsRestrictedFilesystem() {
		return nil, errors.New("native quota helper requires Docker")
	}
	if len(command) == 0 || !strings.HasPrefix(command[0], "/") {
		return nil, errors.New("native quota helper has no command")
	}
	home := cfg.AgentProfileDir(ag.Name(), profile)
	lease, err := credentialUseLease(ctx, cfg, home, true)
	if err != nil {
		return nil, err
	}
	defer lease.close()
	if err := checkCredentialContainers(ctx, rt, ag.Name(), lease.key, true); err != nil {
		return nil, err
	}
	check := ag.Usage().NativeCredentialCheck
	if check == nil {
		return nil, errors.New("native quota helper has no credential check")
	}
	if err := check(home); err != nil {
		return nil, err
	}
	base := cfg.Clone()
	base.BaseImage = ManagedBaseRepository
	ResolveBaseImage(base)
	image := rt.ImageIDContext(ctx, base.BaseImage)
	if len(image) != 71 || !strings.HasPrefix(image, "sha256:") || strings.Trim(image[7:], "0123456789abcdef") != "" {
		return nil, errors.New("native quota helper image is unavailable — run coop init to build the current base")
	}
	owner := randomHex(20)
	if strings.ContainsAny(home+cfg.HomeInBox, ",\n\r") {
		return nil, errors.New("native quota helper credential path is unsafe")
	}
	args := usageQuotaNativeArgs(cfg, ag.Name(), home, image, owner, lease.key, command, ag.Usage().NativeEnv)
	var output quotaOutput
	code, runErr := rt.RunInterruptible(ctx, nil, &output, io.Discard, args...)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rt.RemoveByLabel(cleanupCtx, "coop.usage", owner); err != nil {
		return nil, errors.New("native quota helper cleanup failed")
	}
	if ctx.Err() != nil {
		return nil, errors.New("native quota lookup timed out")
	}
	if runErr != nil || code != 0 || output.exceeded {
		return nil, errors.New("native quota lookup unavailable")
	}
	return output.Bytes(), nil
}

func usageQuotaNativeArgs(cfg *config.Config, provider, home, image, owner, key string, command, env []string) []string {
	args := []string{"run", "--rm", "--init", "--label", "coop.usage=" + owner,
		"--label", "coop.credential.writer=" + key, "--label", "coop.credential." + provider + "=" + key,
		"--user", boxAgentIdentity().user(), "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--cpus=1", "--memory=512m", "--pids-limit=128", "--workdir=/tmp",
		"--tmpfs", "/tmp:rw,nosuid,nodev,mode=1777,size=64m", "--tmpfs", cfg.HomeInBox + ":rw,nosuid,nodev,mode=1777,size=16m",
		"--mount", "type=bind,src=" + home + ",dst=" + cfg.HomeInBox + "/." + provider,
		"--env", "HOME=" + cfg.HomeInBox}
	for _, value := range env {
		args = append(args, "--env", value)
	}
	args = append(args, "--entrypoint", command[0], image)
	return append(args, command[1:]...)
}

type quotaOutput struct {
	bytes.Buffer
	exceeded bool
}

func (out *quotaOutput) Write(data []byte) (int, error) {
	n := len(data)
	if n > (1<<20)-out.Len() {
		out.exceeded = true
		data = data[:(1<<20)-out.Len()]
	}
	_, _ = out.Buffer.Write(data)
	return n, nil
}
