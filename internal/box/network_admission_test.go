package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
	"github.com/AndrewDryga/coop/internal/testutil/teststate"
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

// filteredReachedRuntimePreflight is how these tests prove a filtered resolution SUCCEEDED without
// a Docker host: the fixture runtime cannot serve the gateway, so admission stops at the runtime
// preflight — after the mode was resolved, before any authority state exists.
const filteredReachedRuntimePreflight = "restricted networking needs docker"

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
		"built-in default":             {want: "filtered"},
		"project request":              {projectYAML: "box:\n  egress: offline\n", want: "none"},
		"host preference over repo":    {explicitEnv: "offline", projectYAML: "box:\n  egress: open\n", want: "none"},
		"remembered over host":         {remembered: true, explicitEnv: "open", want: "filtered"},
		"remembered survives deletion": {remembered: true, want: "filtered"},
		"invocation over remembered":   {remembered: true, invocation: &open, want: "open"},
		"invocation over preference":   {explicitEnv: "offline", invocation: &open, want: "open"},
		"lone allow-domain implies":    {domains: []string{"example.com"}, want: "filtered"},
		"explicit none":                {invocation: &none, want: "none"},
		// A file that asks for more than a human approved is a pending review,
		// not a posture: the launch refuses and names the review.
		"repo widening is pending":   {remembered: true, projectYAML: "box:\n  egress: open\n", want: "pending"},
		"unapproved open is pending": {projectYAML: "box:\n  egress: open\n", want: "pending"},
		// COOP_EGRESS yields to a remembered approval, so the rules are a real request to review.
		"host preference keeps rules pending": {explicitEnv: "offline", projectYAML: projectRuleYAML, want: "pending"},
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
				// fixture's runtime is not Docker, so the runtime preflight stops it.
				if err == nil || !strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
					t.Fatal("filtered resolution did not reach the runtime preflight", err)
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

const projectRuleYAML = "box:\n  egress_rules:\n    - to: {domain: example.com}\n      protocol: tls\n      ports: [443]\n"

// --egress open or offline is the documented way to run one box without the project's rules,
// approved or not: they only add to filtered access, so neither mode can use them. The run writes
// no host state and leaves the remembered approval as it was.
func TestAdmitNetworkOneOffOpenOrOfflineSetsProjectRulesAside(t *testing.T) {
	for _, remembered := range []bool{false, true} {
		for _, mode := range []egress.Mode{egress.Open, egress.None} {
			t.Run(fmt.Sprintf("%s remembered=%v", mode.Word(), remembered), func(t *testing.T) {
				cfg, repo, root := admissionFixture(t)
				writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
				approved := []egress.Rule{{To: egress.Destination{Domain: "other.example.com"}, Protocol: "tls", Ports: []int{443}}}
				if remembered {
					// The file has asked for another rule since: a pending change for filtered runs.
					rememberFilteredPosture(t, root, repo, approved)
				}
				capture, err := admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: &mode})
				if err != nil || capture != nil || cfg.Egress != string(mode) {
					t.Fatalf("--egress %s with project rules = capture %v, %v, posture %q", mode.Word(), capture != nil, err, cfg.Egress)
				}
				if !remembered {
					if _, err := os.Stat(root); !os.IsNotExist(err) {
						t.Fatal("a one-off run created authority state", err)
					}
					return
				}
				store, err := networkstate.OpenExisting(root, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				approval, err := store.Approval(repo)
				if err != nil || approval == nil || approval.Posture != egress.Filtered || len(approval.Envelope) != 1 || approval.Envelope[0].To.Domain != "other.example.com" {
					t.Fatal("a one-off run changed the remembered approval", approval, err)
				}
			})
		}
	}
}

// The person's own rule flags only add to filtered access. On a run that is open or offline —
// by flag, COOP_EGRESS or a remembered approval — they are refused by name with the way out,
// before any state exists, rather than silently dropped.
func TestAdmitNetworkRefusesRuleFlagsOnARunThatIsNotFiltered(t *testing.T) {
	open, none, filtered := egress.Open, egress.None, egress.Filtered
	for name, test := range map[string]struct {
		env        string
		invocation *egress.Mode
		domains    []string
		rulesFile  string // "inside" the project or "outside" it
		want       []string
	}{
		"open flag and a domain":        {invocation: &open, domains: []string{"example.com"}, want: []string{"--allow-domain only adds", "this run is open"}},
		"offline flag and a rules file": {invocation: &none, rulesFile: "outside", want: []string{"--egress-rules only adds", "this run is offline"}},
		"offline flag and a repo file":  {invocation: &none, rulesFile: "inside", want: []string{"--egress-rules only adds", "this run is offline"}},
		"COOP_EGRESS and both flags":    {env: "offline", domains: []string{"example.com"}, rulesFile: "outside", want: []string{"--allow-domain and --egress-rules only add", "this run is offline"}},
		"filtered flag and a domain":    {invocation: &filtered, domains: []string{"example.com"}, want: []string{filteredReachedRuntimePreflight}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, repo, root := admissionFixture(t)
			if test.env != "" {
				t.Setenv("COOP_EGRESS", test.env)
				loaded, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				loaded.ConfigDir = cfg.ConfigDir
				cfg = loaded
			}
			options := NetworkAdmission{InvocationMode: test.invocation, Domains: test.domains}
			if test.rulesFile != "" {
				dir := t.TempDir()
				if test.rulesFile == "inside" {
					dir = repo
				}
				options.RulesFile = filepath.Join(dir, "rules.yaml")
				writeCopyFixture(t, options.RulesFile, networkRulesFixture)
			}
			before := cfg.Egress
			capture, err := admitFixture(t, cfg, repo, options)
			if capture != nil || err == nil {
				t.Fatal("rule flags were admitted", capture != nil, err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
			if test.invocation != nil && *test.invocation == egress.Filtered {
				return
			}
			if !strings.Contains(err.Error(), "Use --egress filtered") || strings.Contains(err.Error(), "network_policy_conflict") {
				t.Errorf("refusal %q does not name the way out in plain words", err)
			}
			if cfg.Egress != before {
				t.Errorf("a refused launch still resolved a posture: %q", cfg.Egress)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Error("a refused launch created authority state", err)
			}
		})
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
			// This fixture's runtime is not Docker, so admission stops at the runtime
			// preflight — before the store, before qualification. The point is that the
			// Dockerfile is not the reason.
			if err == nil || !strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
				t.Fatal("a project Dockerfile was refused at admission", err)
			}
		})
	}
	cfg, repo, root := admissionFixture(t)
	cfg.ImageOverride = "someone-elses:latest"
	filtered := egress.Filtered
	// Named docker so the image refusal is the one under test: the runtime preflight runs first
	// now, and nothing here reaches a runtime command.
	capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "docker"}, RunSpec{Repo: repo},
		NetworkAdmission{InvocationMode: &filtered})
	if capture != nil || err == nil || !strings.Contains(err.Error(), "unset COOP_IMAGE") {
		t.Fatal("an unqualified image override reached a filtered launch", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("refused support gap created authority state", err)
	}
}

// A runtime that cannot serve a filtered box is refused by name, with the feature and the runtime
// that does work — and the refusal lands before the authority store exists, so asking for something
// this host cannot do never leaves state behind for the next launch to inherit.
//
// The built-in default is filtered too, so a run that chose nothing is refused the same way, with
// every choice that would let it start — never quietly run open instead.
func TestAdmitNetworkRefusesARuntimeThatCannotServeFiltered(t *testing.T) {
	filtered := egress.Filtered
	for name, invocation := range map[string]*egress.Mode{"explicit": &filtered, "default": nil} {
		t.Run(name, func(t *testing.T) {
			cfg, repo, root := admissionFixture(t)
			capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "container"},
				RunSpec{Repo: repo}, NetworkAdmission{InvocationMode: invocation})
			if capture != nil {
				_ = capture.Close()
				t.Fatal("a runtime without the gateway produced a capture")
			}
			if err == nil {
				t.Fatalf("a runtime without the gateway was admitted, as %q", cfg.Egress)
			}
			for _, want := range []string{"needs docker", "container", "--egress open or --egress offline", "COOP_EGRESS", "COOP_RUNTIME=docker"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("a refused runtime created authority state", err)
			}
		})
	}
}

// Filtered MCP has a literal-only rule — the gateway cannot grant a destination whose routing an
// environment variable could change after approval. That rule belongs to FILTERED launches only.
// Admission used to derive the shared file's network dependencies before it knew the posture, so a
// server with a ${VARIABLE} header refused an explicitly open launch for a constraint only a
// filtered gateway has; env-backed headers were unusable everywhere.
func TestAdmitNetworkOpenLaunchIgnoresFilteredMCPRules(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	cfg.MCPFile = filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(cfg.MCPFile, []byte(`{"mcpServers":{"tenant":{"url":"https://mcp.example/mcp",`+
		`"headers":{"X-Token":"${TENANT_TOKEN}"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Repo: repo, Homes: true}

	open := egress.Open
	if _, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, spec,
		NetworkAdmission{InvocationMode: &open}); err != nil {
		t.Fatalf("an open launch was refused by a filtered-only MCP rule: %v", err)
	}

	// The rule still holds where it applies: a filtered launch with the same file is refused for it —
	// and refused BEFORE the authority store is opened, so asking leaves no owner key behind. Named
	// docker so the MCP refusal is the one under test; nothing here reaches a runtime command.
	filtered := egress.Filtered
	_, err := AdmitNetwork(cfg, runtime.Runtime{Name: "docker"}, spec,
		NetworkAdmission{InvocationMode: &filtered})
	if err == nil || !strings.Contains(err.Error(), "restricted MCP requires literal configuration") {
		t.Fatalf("a filtered launch accepted a non-literal MCP definition: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("a refused filtered launch created authority state", err)
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
		!strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
		t.Fatal("an operator rules file did not grant its own authority", err)
	}
}

// A running editor session re-admits between turns with ExistingQualification: a host with no current
// proof is refused, naming the one command that prepares it, and nothing is built in the meantime.
func TestAdmissionWithExistingQualificationNeverSetsTheHostUp(t *testing.T) {
	cfg, repo, _ := admissionFixture(t)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "aarch64"}, nil
	}))
	calls := filepath.Join(t.TempDir(), "calls")
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\nwhile [ \"$1\" = --host ] || [ \"$1\" = --config ]; do shift 2; done\nprintf '%s\\n' \"$1\" >> " + calls + "\n" +
		"case \"$1\" in\ninfo) printf '%s\\n' '{\"ID\":\"fixture-daemon\",\"ServerVersion\":\"29.4.0\",\"KernelVersion\":\"fixture\",\"OSType\":\"linux\",\"Architecture\":\"aarch64\",\"SecurityOptions\":[]}' ;;\nimage) exit 1 ;;\n*) exit 91 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: script}, RunSpec{Repo: repo, Workdir: repo},
		NetworkAdmission{InvocationMode: &mode, ExistingQualification: true})
	if capture != nil {
		_ = capture.Close()
	}
	if !errors.Is(err, ErrNetworkSetupRequired) || !strings.Contains(err.Error(), "run 'coop net setup' first") {
		t.Fatalf("admission without a host proof = %v, want the coop net setup refusal", err)
	}
	ran, _ := os.ReadFile(calls)
	for _, command := range strings.Fields(string(ran)) {
		if command != "info" && command != "image" {
			t.Fatalf("admission that may not set the host up ran docker %s (all: %q)", command, ran)
		}
	}
}

// A test binary never resolves the network authority store into the developer's real state: a cli
// test that fell back to ~/.local/state once sealed a run record in the real store. Under TestMain
// the store is outside the real home (a temporary folder may sit inside it), and the developer's
// state is refused outright. NetworkStatePath only builds names, so the refused cases open nothing.
func TestNetworkStatePathStaysOutOfTheRealHome(t *testing.T) {
	home := teststate.DeveloperHome()
	inside := func(dir, path string) bool {
		rel, err := filepath.Rel(dir, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if inside(home, root) && !inside(os.TempDir(), root) {
		t.Fatalf("NetworkStatePath = %s, under the real home %s", root, home)
	}
	for _, value := range []string{"", filepath.Join(home, ".local", "state")} {
		t.Setenv("XDG_STATE_HOME", value)
		if path, err := NetworkStatePath(); err == nil {
			t.Errorf("XDG_STATE_HOME=%q: NetworkStatePath = %s; want a refusal", value, path)
		}
	}
}

// noSetupDocker is a fixture Docker with no host setup: admission stops right after publishing the
// policy it captured, which is what these tests read.
func noSetupDocker(t *testing.T) runtime.Runtime {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "aarch64"}, nil
	}))
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\nwhile [ \"$1\" = --host ] || [ \"$1\" = --config ]; do shift 2; done\n" +
		"case \"$1\" in\ninfo) printf '%s\\n' '{\"ID\":\"fixture-daemon\",\"ServerVersion\":\"29.4.0\",\"KernelVersion\":\"fixture\",\"OSType\":\"linux\",\"Architecture\":\"aarch64\",\"SecurityOptions\":[]}' ;;\nimage) exit 1 ;;\n*) exit 91 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: script}
}

// heldAdmission admits the way an editor session connects.
func heldAdmission(t *testing.T, cfg *config.Config, repo string) error {
	t.Helper()
	capture, err := AdmitNetwork(cfg, noSetupDocker(t), RunSpec{Repo: repo, Workdir: repo},
		NetworkAdmission{ExistingQualification: true, ProjectBuild: DeferProjectBuild, HoldPending: true})
	if capture != nil {
		_ = capture.Close()
	}
	return err
}

// publishedPolicies reads every policy admission published, and fails the test if there is none.
func publishedPolicies(t *testing.T, root string) []egress.Snapshot {
	t.Helper()
	published, err := filepath.Glob(filepath.Join(root, "snapshot-*.json"))
	if err != nil || len(published) == 0 {
		t.Fatalf("no policy was captured: %v, %v", published, err)
	}
	policies := make([]egress.Snapshot, len(published))
	for i, path := range published {
		if err := json.Unmarshal(mustReadFile(t, path), &policies[i]); err != nil {
			t.Fatal(err)
		}
	}
	return policies
}

// A session that already runs under its rules needs another set for a child that holds prompts: one a
// project can do nothing to, so a broken file, a changed service or a sidecar that fails cannot stop
// the child that says so. It has none of the project's rules, approved or not.
func TestAdmitHoldingNetworkTakesNothingFromTheProject(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	project := filepath.Join(repo, ".agent", "project.yaml")
	writeCopyFixture(t, project, projectRuleYAML)
	approveFixture(t, cfg, repo)
	writeCopyFixture(t, project, "box: [broken\n")
	capture, err := AdmitHoldingNetwork(cfg, noSetupDocker(t), RunSpec{Repo: repo, Workdir: repo}, NetworkAdmission{})
	if capture != nil {
		_ = capture.Close()
	}
	if !errors.Is(err, ErrNetworkSetupRequired) {
		t.Fatalf("it stopped at %v, want it to reach the host's setup and go no further", err)
	}
	for _, policy := range publishedPolicies(t, root) {
		if policy.Mode != egress.Filtered || policy.Domain("example.com", 443).Allowed {
			t.Fatalf("a holding child's rules: mode %q, reach the project's own site: %v", policy.Mode, policy.Domain("example.com", 443).Allowed)
		}
	}
}

// A withdrawal waits for a person even here. The session ends on it rather than start anything.
func TestAdmitHoldingNetworkRefusesAWithdrawnProject(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
	approveFixture(t, cfg, repo)
	review, err := ReviewProjectNetworkForget(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer review.Close()
	if err := review.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	capture, err := AdmitHoldingNetwork(cfg, noSetupDocker(t), RunSpec{Repo: repo, Workdir: repo}, NetworkAdmission{})
	if capture != nil {
		_ = capture.Close()
	}
	var pending *networkstate.PendingApproval
	if !errors.As(err, &pending) || !strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("a withdrawn project = %v", err)
	}
	if published, _ := filepath.Glob(filepath.Join(root, "snapshot-*.json")); len(published) != 0 {
		t.Fatalf("a withdrawn project got a policy: %v", published)
	}
}

// An editor connects while its project asks for access nobody approved. What it connects under has
// none of what the file asks for, nor anything approved earlier: a turn never runs there to use it,
// and a service grant would start the project's sidecars. It is filtered however the file asks.
func TestHeldAdmissionLeavesOutAllThatTheProjectAsks(t *testing.T) {
	const earlier = "box:\n  egress_rules:\n    - to: {domain: earlier.example.com}\n      protocol: tls\n      ports: [443]\n"
	for name, test := range map[string]struct{ approved, asks string }{
		"rules nobody approved":                  {asks: projectRuleYAML},
		"unrestricted access nobody approved":    {asks: "box:\n  egress: open\n"},
		"rules changed since they were approved": {approved: earlier, asks: projectRuleYAML},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, repo, root := admissionFixture(t)
			project := filepath.Join(repo, ".agent", "project.yaml")
			if test.approved != "" {
				writeCopyFixture(t, project, test.approved)
				approveFixture(t, cfg, repo)
			}
			writeCopyFixture(t, project, test.asks)
			if err := heldAdmission(t, cfg, repo); !errors.Is(err, ErrNetworkSetupRequired) {
				t.Fatalf("a held admission stopped at %v, want it to reach the host's setup", err)
			}
			if cfg.Egress != string(egress.Filtered) {
				t.Fatalf("the session resolved to %q", cfg.Egress)
			}
			for _, policy := range publishedPolicies(t, root) {
				if policy.Mode != egress.Filtered {
					t.Errorf("a holding child's rules have mode %q", policy.Mode)
				}
				for _, site := range []string{"example.com", "earlier.example.com"} {
					if policy.Domain(site, 443).Allowed {
						t.Errorf("a holding child's rules reach %s", site)
					}
				}
			}
		})
	}
}

// brokenProjectFiles are files that do not load, each reading in part as rules a person might have meant.
var brokenProjectFiles = map[string]string{
	"not yaml":                      "box: [broken\n",
	"rules and then a typo":         projectRuleYAML + "nonsense: true\n",
	"unrestricted and then a typo":  "box:\n  egress: open\n  nonsense: true\n",
	"rules and a port out of range": projectRuleYAML + "serve:\n  ports: [70000]\n",
	"a second document":             projectRuleYAML + "---\n" + projectRuleYAML,
}

// qualifiedDocker is a fixture Docker on a host already set up, so that admission gets as far as a capture.
func qualifiedDocker(t *testing.T, root string) runtime.Runtime {
	t.Helper()
	rt, _, candidate, images := qualificationInventoryFixture(t)
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	writeQualificationFixture(t, store, candidate, egress.ClientACP, time.Now())
	for _, id := range []string{candidate.ClientImage, candidate.GatewayImage} {
		writeQualificationImageFixture(t, images, id, id)
	}
	return rt
}

// A file that does not load says nothing: an editor connects on a capture that serves only a child that
// holds prompts, filtered, with none of what the file seems to ask for and nothing approved earlier.
// Admitting the fixed file is then an ordinary admission: what it asks for waits for a person, and a real
// capture only follows from a file with nothing left to settle.
func TestHeldAdmissionTakesNothingFromABrokenFile(t *testing.T) {
	const earlier = "box:\n  egress_rules:\n    - to: {domain: earlier.example.com}\n      protocol: tls\n      ports: [443]\n"
	for name, broken := range brokenProjectFiles {
		t.Run(name, func(t *testing.T) {
			cfg, repo, root := admissionFixture(t)
			path := filepath.Join(repo, ".agent", "project.yaml")
			writeCopyFixture(t, path, earlier)
			approveFixture(t, cfg, repo)
			writeCopyFixture(t, path, broken)
			rt := qualifiedDocker(t, root)
			connect := func(t *testing.T, cfg *config.Config, options NetworkAdmission) (*CapturedEgress, error) {
				t.Helper()
				options.ProjectBuild = DeferProjectBuild
				capture, err := AdmitNetwork(cfg, rt, RunSpec{Repo: repo, Workdir: repo}, options)
				if capture != nil {
					t.Cleanup(func() { _ = capture.Close() })
				}
				return capture, err
			}
			startup := cfg.Clone()
			held, err := connect(t, startup, NetworkAdmission{HoldPending: true})
			if err != nil {
				t.Fatal(err)
			}
			if !held.BrokenFile || !held.HoldsOnly() || held.Pending != nil || startup.Egress != string(egress.Filtered) {
				t.Fatalf("broken=%v holds=%v pending=%t egress=%q", held.BrokenFile, held.HoldsOnly(), held.Pending != nil, startup.Egress)
			}
			policy, err := held.Policy()
			if err != nil {
				t.Fatal(err)
			}
			for _, site := range []string{"example.com", "earlier.example.com"} {
				if policy.Mode != egress.Filtered || policy.Domain(site, 443).Allowed {
					t.Fatalf("a holding child's rules: mode %q, reach %s: %v", policy.Mode, site, policy.Domain(site, 443).Allowed)
				}
			}

			// Admitting again, as the session does while it waits, refuses until the file is fixed.
			var refused *BrokenProjectFile
			if _, err := connect(t, cfg.Clone(), NetworkAdmission{ExistingQualification: true}); !errors.As(err, &refused) {
				t.Fatalf("admitting the broken file again = %v", err)
			}
			writeCopyFixture(t, path, projectRuleYAML)
			var pending *networkstate.PendingApproval
			if _, err := connect(t, cfg.Clone(), NetworkAdmission{ExistingQualification: true}); !errors.As(err, &pending) {
				t.Fatalf("a fixed file that asks for something new = %v, want it to need approval", err)
			}
			writeCopyFixture(t, path, earlier)
			real, err := connect(t, cfg.Clone(), NetworkAdmission{ExistingQualification: true})
			if err != nil {
				t.Fatal(err)
			}
			if real.HoldsOnly() {
				t.Fatal("a fixed file with nothing to settle still only holds")
			}
			if policy, err = real.Policy(); err != nil || !policy.Domain("earlier.example.com", 443).Allowed {
				t.Fatalf("the fixed file's approved rules are gone: %v", err)
			}
		})
	}
}

// A holding child is filtered whatever was approved earlier, since nothing can tell whether the file
// still asks for it. Once fixed, an approved open mode is what it was: not a capture.
func TestHeldAdmissionOfABrokenFileUnderAnApprovedOpenMode(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	path := filepath.Join(repo, ".agent", "project.yaml")
	writeCopyFixture(t, path, "box:\n  egress: open\n")
	approveFixture(t, cfg, repo)
	writeCopyFixture(t, path, "box: [broken\n")
	rt := qualifiedDocker(t, root)
	startup := cfg.Clone()
	held, err := AdmitNetwork(startup, rt, RunSpec{Repo: repo, Workdir: repo}, NetworkAdmission{ProjectBuild: DeferProjectBuild, HoldPending: true})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	policy, err := held.Policy()
	if err != nil || policy.Mode != egress.Filtered || !held.BrokenFile || startup.Egress != string(egress.Filtered) {
		t.Fatalf("policy mode %q, broken=%v, egress %q, err=%v", policy.Mode, held.BrokenFile, startup.Egress, err)
	}
	writeCopyFixture(t, path, "box:\n  egress: open\n")
	again := cfg.Clone()
	if capture, err := AdmitNetwork(again, rt, RunSpec{Repo: repo, Workdir: repo}, NetworkAdmission{ExistingQualification: true, ProjectBuild: DeferProjectBuild}); capture != nil || err != nil || again.Egress != string(egress.Open) {
		t.Fatalf("the fixed file = (%v, %v), egress %q, want the approved open mode", capture, err, again.Egress)
	}
}

// A file that is not one a project may use at all is refused the same way, and says why as itself.
func TestHeldAdmissionOfALinkedProjectFile(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	target := filepath.Join(t.TempDir(), "project.yaml")
	writeCopyFixture(t, target, projectRuleYAML)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, ".agent", "project.yaml")); err != nil {
		t.Fatal(err)
	}
	capture, err := AdmitNetwork(cfg, qualifiedDocker(t, root), RunSpec{Repo: repo, Workdir: repo}, NetworkAdmission{ProjectBuild: DeferProjectBuild, HoldPending: true})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	if !capture.BrokenFile {
		t.Fatal("a linked project file was read as rules")
	}
	policy, err := capture.Policy()
	if err != nil || policy.Domain("example.com", 443).Allowed {
		t.Fatalf("a holding child's rules reach what a linked file asks for: %v", err)
	}
}

// What holding a broken file does not reach. Each refuses as itself, with nothing started: the file is
// still what the person has to fix first.
func TestHeldAdmissionOfABrokenFileStillRefuses(t *testing.T) {
	refused := func(t *testing.T, err error) {
		t.Helper()
		var broken *BrokenProjectFile
		if !errors.As(err, &broken) || !strings.Contains(err.Error(), ".agent/project.yaml") {
			t.Fatalf("admission = %v, want the broken file", err)
		}
	}
	nothingPublished := func(t *testing.T, root string) {
		t.Helper()
		if published, _ := filepath.Glob(filepath.Join(root, "snapshot-*.json")); len(published) != 0 {
			t.Fatalf("a policy was captured: %v", published)
		}
	}
	broken := func(t *testing.T, repo string) {
		t.Helper()
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), "box: [broken\n")
	}
	posture := func(t *testing.T, cfg *config.Config, value string) *config.Config {
		t.Helper()
		t.Setenv("COOP_EGRESS", value)
		loaded, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		loaded.ConfigDir = cfg.ConfigDir
		return loaded
	}
	t.Run("a caller that did not ask", func(t *testing.T) {
		cfg, repo, root := admissionFixture(t)
		broken(t, repo)
		_, err := admitFixture(t, cfg, repo, NetworkAdmission{})
		refused(t, err)
		nothingPublished(t, root)
		// The file comes first for any launch, since whatever else it might be told depends on it.
		open := egress.Open
		_, err = admitFixture(t, cfg, repo, NetworkAdmission{InvocationMode: &open, Domains: []string{"example.com"}})
		refused(t, err)
	})
	t.Run("an offline posture", func(t *testing.T) {
		cfg, repo, root := admissionFixture(t)
		broken(t, repo)
		refused(t, heldAdmission(t, posture(t, cfg, "offline"), repo))
		nothingPublished(t, root)
	})
	t.Run("a runtime with no gateway", func(t *testing.T) {
		cfg, repo, root := admissionFixture(t)
		broken(t, repo)
		_, err := AdmitNetwork(cfg, runtime.Runtime{Name: "container"}, RunSpec{Repo: repo, Workdir: repo}, NetworkAdmission{ProjectBuild: DeferProjectBuild, HoldPending: true})
		refused(t, err)
		nothingPublished(t, root)
	})
	t.Run("a withdrawal", func(t *testing.T) {
		cfg, repo, root := admissionFixture(t)
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
		approveFixture(t, cfg, repo)
		review, err := ReviewProjectNetworkForget(repo)
		if err != nil {
			t.Fatal(err)
		}
		defer review.Close()
		if err := review.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		broken(t, repo)
		refused(t, heldAdmission(t, cfg, repo))
		nothingPublished(t, root)
	})
	// A filtered session sets the host up as any filtered launch does. Someone who runs open never asked
	// for that, so a broken file does not start it for them.
	settingUp := func(t *testing.T, cfg *config.Config, repo string, want bool) {
		t.Helper()
		var transcript strings.Builder
		_, err := AdmitNetwork(cfg, noSetupDocker(t), RunSpec{Repo: repo, Workdir: repo, Stderr: &transcript}, NetworkAdmission{ProjectBuild: DeferProjectBuild, HoldPending: true})
		refused(t, err)
		if (transcript.Len() != 0) != want {
			t.Fatalf("set the host up: %v, want %v: %q", transcript.Len() != 0, want, transcript.String())
		}
	}
	t.Run("a filtered session on a host with no setup", func(t *testing.T) {
		cfg, repo, _ := admissionFixture(t)
		broken(t, repo)
		settingUp(t, posture(t, cfg, "filtered"), repo, true)
	})
	t.Run("an open posture on a host with no setup", func(t *testing.T) {
		cfg, repo, _ := admissionFixture(t)
		broken(t, repo)
		settingUp(t, posture(t, cfg, "open"), repo, false)
	})
}

// A refusal of a broken file keeps its parts: the terminal draws a read failure in its own block, and
// escapes what yaml quotes as written.
func TestBrokenProjectFileKeepsItsCause(t *testing.T) {
	cfg, repo, _ := admissionFixture(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "elsewhere"), filepath.Join(repo, ".agent", "project.yaml")); err != nil {
		t.Fatal(err)
	}
	_, err := admitFixture(t, cfg, repo, NetworkAdmission{})
	var file *project.FileError
	if !errors.As(err, &file) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("admission = %v, want the read failure", err)
	}
}

// What HoldPending does not reach. Another caller still refuses a pending request. A withdrawal waits
// for a person with nothing started. Offline stays offline: a filtered holding child would give the
// session a network its posture denies. Each names the review as ever.
func TestHeldAdmissionStillRefuses(t *testing.T) {
	refused := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "cannot start because") || !strings.Contains(err.Error(), "Review it: coop approve") {
			t.Fatalf("admission = %v, want the refusal naming the review", err)
		}
	}
	t.Run("a caller that did not ask", func(t *testing.T) {
		cfg, repo, _ := admissionFixture(t)
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
		_, err := admitFixture(t, cfg, repo, NetworkAdmission{})
		refused(t, err)
	})
	t.Run("an offline posture", func(t *testing.T) {
		cfg, repo, _ := admissionFixture(t)
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
		t.Setenv("COOP_EGRESS", "offline")
		loaded, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		loaded.ConfigDir = cfg.ConfigDir
		refused(t, heldAdmission(t, loaded, repo))
	})
	// A holding child runs filtered, so where filtered cannot run nothing can hold the session. The
	// review still comes first: the person acts on it before the runtime matters.
	cannotHold := func(t *testing.T, cfg *config.Config, repo string, rt runtime.Runtime) {
		t.Helper()
		_, err := AdmitNetwork(cfg, rt, RunSpec{Repo: repo, Workdir: repo},
			NetworkAdmission{ExistingQualification: true, ProjectBuild: DeferProjectBuild, HoldPending: true})
		refused(t, err)
	}
	t.Run("a runtime with no gateway", func(t *testing.T) {
		cfg, repo, _ := admissionFixture(t)
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
		cannotHold(t, cfg, repo, runtime.Runtime{Name: "container"})
	})
	t.Run("a runtime with no gateway, on an open posture", func(t *testing.T) {
		cfg, repo, _ := admissionFixture(t)
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
		t.Setenv("COOP_EGRESS", "open")
		loaded, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		loaded.ConfigDir = cfg.ConfigDir
		cannotHold(t, loaded, repo, runtime.Runtime{Name: "container"})
	})
	t.Run("an image of the person's own", func(t *testing.T) {
		cfg, repo, _ := admissionFixture(t)
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
		cfg.ImageOverride = "my-image"
		cannotHold(t, cfg, repo, noSetupDocker(t))
	})
	t.Run("a withdrawn approval", func(t *testing.T) {
		cfg, repo, root := admissionFixture(t)
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), projectRuleYAML)
		approveFixture(t, cfg, repo)
		review, err := ReviewProjectNetworkForget(repo)
		if err != nil {
			t.Fatal(err)
		}
		defer review.Close()
		if err := review.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		refused(t, heldAdmission(t, cfg, repo)) // the file still asks, and the store still refuses
		writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  egress: filtered\n")
		err = heldAdmission(t, cfg, repo)
		refused(t, err)
		if !strings.Contains(err.Error(), "withdrawn") {
			t.Fatalf("the refusal hides the withdrawal: %v", err)
		}
		if published, _ := filepath.Glob(filepath.Join(root, "snapshot-*.json")); len(published) != 0 {
			t.Fatalf("a withdrawn project got a policy: %v", published)
		}
	})
}
