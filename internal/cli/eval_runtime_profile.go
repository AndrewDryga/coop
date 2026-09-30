package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/eval"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

type evalProfileKey struct{ profile, configuration string }
type evalProfileCaptures map[evalProfileKey]*box.CapturedEgress

func (captures evalProfileCaptures) close() {
	for _, capture := range captures {
		_ = capture.Close()
	}
}

func evalProfileConfig(base *config.Config) *config.Config {
	cfg := evalTrialConfig(base)
	cfg.SetRuntimeLimits("1", "2g", "128")
	cfg.NoNewPrivileges = true
	cfg.AutoUp, cfg.Network, cfg.Cache = false, false, false
	return cfg
}

// Dedicated profiles provide build inputs, not a second channel for sidecars, host environment
// or network grants. Their image may add tools; execution stays the fixed offline-build protocol.
func validateEvalProfilePolicy(profile string) error {
	p, err := project.Load(profile)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(profile, p.DockerfileRel()))
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("runtime profile needs an explicit project Dockerfile and host 'coop build --egress filtered'")
	}
	if len(p.Box.Env) != 0 || p.Box.Compose != "" || p.Box.AutoUp != nil && *p.Box.AutoUp || p.Box.Network != nil && *p.Box.Network || len(p.Serve.Ports) != 0 || len(p.Services.RequireRealFiles) != 0 || len(p.Box.EgressRules) != 0 {
		return fmt.Errorf("runtime profiles cannot add environment, services, published ports or network rules")
	}
	return nil
}

func validateEvalProviderOnly(policy egress.Snapshot, provider string) error {
	if policy.Mode != egress.Filtered {
		return fmt.Errorf("runtime profile needs filtered provider-only networking")
	}
	for _, grant := range policy.Grants {
		if len(grant.Origins) == 0 {
			return fmt.Errorf("runtime profile has a network grant without provider authority")
		}
		for _, origin := range grant.Origins {
			if origin.Kind != "provider" || origin.Provider != provider || origin.Feature != "" {
				return fmt.Errorf("runtime profile has non-core provider network grants; use a dedicated profile without extra approvals")
			}
		}
	}
	return nil
}

// Freeze approval and measured image/platform before the manifest exists. Retained profile bytes
// are evidence only: every resolution is against the original, explicitly built host directory.
func (a *app) prepareEvalProfiles(ctx context.Context, plan *eval.Plan, frozen []eval.FrozenConfig) (_ evalProfileCaptures, err error) {
	captures := evalProfileCaptures{}
	defer func() {
		if err != nil {
			captures.close()
		}
	}()
	for i := range plan.Suite.Cases {
		r := plan.Suite.Cases[i].Runtime
		if r == nil {
			continue
		}
		if err := r.VerifyProfile(ctx); err != nil {
			return nil, err
		}
		if err := validateEvalProfilePolicy(r.Profile); err != nil {
			return nil, err
		}
		for _, configuration := range frozen {
			key := evalProfileKey{r.Profile, configuration.Label}
			capture := captures[key]
			if capture == nil {
				cfg := evalProfileConfig(a.cfg)
				provider, err := applyEvalConfiguration(cfg, configuration)
				if err != nil {
					return nil, err
				}
				mode := egress.Filtered
				// Admission only: no candidate workspace or provider command is launched here.
				spec := box.RunSpec{Ctx: ctx, Repo: plan.Suite.Dir, PolicyRepo: r.Profile, Agent: provider, AgentCommand: true, Homes: cfg.Homes, Workdir: r.Workdir, Batch: true, Quiet: true}
				capture, err = box.AdmitNetwork(cfg, a.rt, spec, box.NetworkAdmission{InvocationMode: &mode})
				if err != nil {
					return nil, err
				}
				captures[key] = capture
				policy, err := capture.Store.LoadSnapshot(capture.Project, capture.Fingerprint)
				if err != nil {
					return nil, err
				}
				if err := validateEvalProviderOnly(policy, provider); err != nil {
					return nil, err
				}
			}
			image, err := box.FilteredImageIdentity(ctx, a.rt, capture)
			if err != nil {
				return nil, err
			}
			q, err := capture.Store.Qualification(capture.QualificationID)
			if err != nil {
				return nil, err
			}
			platform := q.Candidate.Runtime.OS + "/" + runtime.Architecture(q.Candidate.Runtime.Architecture)
			if r.ImageID != "" && (r.ImageID != image || r.Platform != platform) {
				return nil, fmt.Errorf("runtime profile resolved different image/platform across configurations")
			}
			r.ImageID, r.Platform = image, platform
		}
		if err := r.VerifyProfile(ctx); err != nil {
			return nil, err
		}
	}
	return captures, nil
}
