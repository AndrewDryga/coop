package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// admissionFixture gives every case its own private state root and project, so
// nothing leaks between them and no test can touch the operator's real store.
func admissionFixture(t *testing.T) (*config.Config, string, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{ConfigDir: t.TempDir(), Egress: "open"}, repo, root
}

func admitFixture(t *testing.T, cfg *config.Config, repo string, options NetworkAdmission) (*CapturedEgress, error) {
	t.Helper()
	capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: repo}, options)
	if capture != nil {
		t.Cleanup(func() { _ = capture.Close() })
	}
	return capture, err
}

func rememberFilteredPosture(t *testing.T, root, repo string, rules []egress.Rule) {
	t.Helper()
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	review, err := store.ReviewApproval(repo, egress.Filtered, rules, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(t.Context(), repo, egress.Filtered, rules, nil, nil, review.Digest); err != nil {
		t.Fatal(err)
	}
}

// An ordinary run must not create authority state as a side effect: a machine
// that has never used restricted networking keeps an empty state directory.
func TestAdmitNetworkOrdinaryRunWritesNoHostState(t *testing.T) {
	for _, mode := range []egress.Mode{egress.Open, egress.None} {
		t.Run(string(mode), func(t *testing.T) {
			cfg, repo, root := admissionFixture(t)
			capture, err := admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: &mode})
			if err != nil || capture != nil {
				t.Fatal("ordinary run captured network authority", capture, err)
			}
			if cfg.Egress != string(mode) {
				t.Fatal("ordinary posture changed", cfg.Egress)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("ordinary run created authority state", err)
			}
		})
	}
}

func TestAdmitNetworkResolvesThePrecedenceLadder(t *testing.T) {
	open, none := egress.Open, egress.None
	for name, test := range map[string]struct {
		remembered  bool
		explicitEnv string
		projectYAML string
		invocation  *egress.Mode
		domains     []string
		want        string
	}{
		"built-in default":             {want: "open"},
		"project request":              {projectYAML: "box:\n  egress: offline\n", want: "none"},
		"host preference over repo":    {explicitEnv: "none", projectYAML: "box:\n  egress: open\n", want: "none"},
		"remembered over host":         {remembered: true, explicitEnv: "open", want: "filtered"},
		"remembered survives deletion": {remembered: true, want: "filtered"},
		"invocation over remembered":   {remembered: true, invocation: &open, want: "open"},
		"invocation over preference":   {explicitEnv: "none", invocation: &open, want: "open"},
		"lone allow-domain implies":    {domains: []string{"example.com"}, want: "filtered"},
		"explicit none":                {invocation: &none, want: "none"},
		// A file that asks for more than a human approved is a pending review,
		// not a posture: the launch refuses and names the review.
		"repo widening is pending":   {remembered: true, projectYAML: "box:\n  egress: open\n", want: "pending"},
		"unapproved open is pending": {projectYAML: "box:\n  egress: open\n", want: "pending"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, repo, root := admissionFixture(t)
			if test.projectYAML != "" {
				writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), test.projectYAML)
			}
			if test.explicitEnv != "" {
				t.Setenv("COOP_EGRESS", test.explicitEnv)
				loaded, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				loaded.ConfigDir = cfg.ConfigDir
				cfg = loaded
			}
			if test.remembered {
				rememberFilteredPosture(t, root, repo, nil)
			}
			before := cfg.Egress
			_, err := admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: test.invocation, Domains: test.domains})
			switch test.want {
			case "pending":
				if err == nil || !strings.Contains(err.Error(), "cannot start because this project asks for") || !strings.Contains(err.Error(), "Review it: coop approve") {
					t.Fatal("an unapproved widening was not refused with the review command", err)
				}
				if cfg.Egress != before {
					t.Fatalf("a refused launch still resolved a posture: %q", cfg.Egress)
				}
				return
			case "filtered":
				// Filtered resolution is proved by the refusal that follows it: this
				// fixture's runtime is not Docker, which is where qualification starts.
				if err == nil || !strings.Contains(err.Error(), "requires a local Docker runtime") {
					t.Fatal("filtered resolution did not reach qualification matching", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if cfg.Egress != test.want {
				t.Fatalf("resolved %q, want %q", cfg.Egress, test.want)
			}
		})
	}
}

// A remembered restriction is host-owned. An explicit invocation may step past
// it for one run; it must never rewrite what the operator decided.
func TestAdmitNetworkExplicitOverrideLeavesTheRememberedPosture(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	rememberFilteredPosture(t, root, repo, nil)
	open := egress.Open
	if _, err := admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: &open}); err != nil {
		t.Fatal(err)
	}
	if cfg.Egress != "open" {
		t.Fatal("explicit override refused", cfg.Egress)
	}
	store, err := networkstate.OpenExisting(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	approval, err := store.Approval(repo)
	if err != nil || approval == nil || approval.Posture != egress.Filtered {
		t.Fatal("one invocation changed the remembered posture", approval, err)
	}
}

// An unapproved project request fails admission with structured details, never
// an agent prompt and never a launch.
func TestAdmitNetworkUnapprovedProjectRulesRefuse(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"),
		"box:\n  egress_rules:\n    - to: {domain: example.com}\n      protocol: tls\n      ports: [443]\n")
	capture, err := admitFixture(t, cfg, repo, NetworkAdmission{})
	if capture != nil || err == nil || !strings.Contains(err.Error(), "This box cannot start because this project asks for network access that has not been approved\n\n  Review it: coop approve") {
		t.Fatal("unapproved project request was admitted, or refused without the review command", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("unapproved request created authority state", err)
	}
}

// A project's own Dockerfile is built on the locked client image and proven at
// launch (derived_image.go), so admission no longer refuses one. COOP_IMAGE has
// no such proof available, so it still fails before any state or runtime
// resource exists.
func TestAdmitNetworkRefusesAnImageOverrideButNotAProjectDockerfile(t *testing.T) {
	for _, kind := range []string{"policy", "file"} {
		t.Run(kind, func(t *testing.T) {
			cfg, repo, _ := admissionFixture(t)
			if kind == "policy" {
				writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  dockerfile: .agent/Dockerfile\n")
			}
			writeCopyFixture(t, filepath.Join(repo, ".agent", "Dockerfile"), "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n")
			filtered := egress.Filtered
			_, err := admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: &filtered})
			// This fixture's runtime is not Docker, so admission gets as far as
			// qualification and stops there — the point is that the Dockerfile is
			// not the reason.
			if err == nil || !strings.Contains(err.Error(), "requires a local Docker runtime") {
				t.Fatal("a project Dockerfile was refused at admission", err)
			}
		})
	}
	cfg, repo, root := admissionFixture(t)
	cfg.ImageOverride = "someone-elses:latest"
	filtered := egress.Filtered
	capture, err := admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: &filtered})
	if capture != nil || err == nil || !strings.Contains(err.Error(), "unset COOP_IMAGE") {
		t.Fatal("an unqualified image override reached a filtered launch", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("refused support gap created authority state", err)
	}
}

// Operator domains and an operator-owned rules file grant directly; a rules
// file inside an agent mount is only a request.
func TestAdmitNetworkClassifiesOperatorInputBeforeCapture(t *testing.T) {
	cfg, repo, _ := admissionFixture(t)
	inside := filepath.Join(repo, "rules.yaml")
	if err := os.WriteFile(inside, []byte(networkRulesFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := admitFixture(t, cfg, repo, NetworkAdmission{RulesFile: inside}); err == nil ||
		!strings.Contains(err.Error(), "has not been approved") {
		t.Fatal("a repository rules file granted authority", err)
	}
	outside := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(outside, []byte(networkRulesFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := admitFixture(t, cfg, repo, NetworkAdmission{RulesFile: outside}); err == nil ||
		!strings.Contains(err.Error(), "requires a local Docker runtime") {
		t.Fatal("an operator rules file did not grant its own authority", err)
	}
}
