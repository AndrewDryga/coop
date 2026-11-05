package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestCheckNetworkApprovalUnapprovedRulesMatchAdmission(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
	beforeEgress, beforeExplicit := cfg.Egress, cfg.Explicit("COOP_EGRESS")
	options := NetworkAdmission{}
	spec := RunSpec{Repo: repo}

	previewErr := CheckNetworkApproval(cfg, spec, options)
	if previewErr == nil || !strings.Contains(previewErr.Error(), "Review it: coop approve") {
		t.Fatalf("unapproved rules preview error = %v, want the approval refusal", previewErr)
	}
	if cfg.Egress != beforeEgress || cfg.Explicit("COOP_EGRESS") != beforeExplicit {
		t.Fatalf("refused preview changed config: egress %q (was %q), explicit %v (was %v)",
			cfg.Egress, beforeEgress, cfg.Explicit("COOP_EGRESS"), beforeExplicit)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unapproved preview created authority root: %v", err)
	}
	_, admissionErr := admitFixture(t, cfg, repo, options)
	if admissionErr == nil || admissionErr.Error() != previewErr.Error() {
		t.Fatalf("preview refusal = %v, full admission refusal = %v", previewErr, admissionErr)
	}
	if cfg.Egress != beforeEgress || cfg.Explicit("COOP_EGRESS") != beforeExplicit {
		t.Fatalf("refused preview changed config: egress %q (was %q), explicit %v (was %v)",
			cfg.Egress, beforeEgress, cfg.Explicit("COOP_EGRESS"), beforeExplicit)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unapproved preview created authority root: %v", err)
	}

	// Holding prompts is a launch behavior, not permission to detach a worker whose
	// first assignment would immediately stop for approval.
	heldErr := CheckNetworkApproval(cfg, spec, NetworkAdmission{HoldPending: true})
	if heldErr == nil || heldErr.Error() != previewErr.Error() {
		t.Fatalf("holding preview refusal = %v, want the same approval refusal %v", heldErr, previewErr)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("holding preview created authority root: %v", err)
	}
}

func TestCheckNetworkApprovalBareFilteredPreviewIsReadOnly(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	beforeEgress, beforeExplicit := cfg.Egress, cfg.Explicit("COOP_EGRESS")

	if err := CheckNetworkApproval(cfg, RunSpec{Repo: repo}, NetworkAdmission{}); err != nil {
		t.Fatalf("bare filtered preview: %v", err)
	}
	if cfg.Egress != beforeEgress || cfg.Explicit("COOP_EGRESS") != beforeExplicit {
		t.Fatalf("preview changed config: egress %q (was %q), explicit %v (was %v)",
			cfg.Egress, beforeEgress, cfg.Explicit("COOP_EGRESS"), beforeExplicit)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("bare filtered preview created authority root: %v", err)
	}
}

func TestCheckNetworkApprovalApprovedRulesDoNotGrantLaunch(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
	rules := []egress.Rule{{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}}}
	rememberFilteredPosture(t, root, repo, rules)

	if err := CheckNetworkApproval(cfg, RunSpec{Repo: repo}, NetworkAdmission{}); err != nil {
		t.Fatalf("approved rules preview: %v", err)
	}
	if cfg.Egress != "open" || cfg.Explicit("COOP_EGRESS") {
		t.Fatalf("approved preview changed config: egress %q, explicit %v", cfg.Egress, cfg.Explicit("COOP_EGRESS"))
	}
	_, err := admitFixture(t, cfg, repo, NetworkAdmission{})
	if err == nil || !strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
		t.Fatalf("full admission after preview = %v, want restricted runtime preflight", err)
	}
}

func TestCheckNetworkApprovalExplicitOpenOrOfflineSetsRulesAside(t *testing.T) {
	for _, mode := range []egress.Mode{egress.Open, egress.None} {
		t.Run(mode.Word(), func(t *testing.T) {
			cfg, repo, root := admissionFixture(t)
			writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
			beforeEgress, beforeExplicit := cfg.Egress, cfg.Explicit("COOP_EGRESS")

			if err := CheckNetworkApproval(cfg, RunSpec{Repo: repo}, NetworkAdmission{InvocationMode: &mode}); err != nil {
				t.Fatalf("explicit %s preview: %v", mode.Word(), err)
			}
			if cfg.Egress != beforeEgress || cfg.Explicit("COOP_EGRESS") != beforeExplicit {
				t.Fatalf("preview changed config: egress %q (was %q), explicit %v (was %v)",
					cfg.Egress, beforeEgress, cfg.Explicit("COOP_EGRESS"), beforeExplicit)
			}
			capture, err := admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: &mode})
			if capture != nil || err != nil || cfg.Egress != string(mode) {
				t.Fatalf("full admission for explicit %s = capture %v, err %v, egress %q",
					mode.Word(), capture != nil, err, cfg.Egress)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("explicit %s run created authority root: %v", mode.Word(), err)
			}
		})
	}
}
