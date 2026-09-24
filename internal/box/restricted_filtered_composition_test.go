package box

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Read-only runs compose their restricted FILESYSTEM profile with the EXISTING filtered launch,
// which creates its agent box with runtime.CreateContainer. Bare shares the filesystem profile but
// stays refused by checkRestrictedSpec because it has no project for network admission.
//
// That path validates its options against a strict allowlist and refuses anything else outright. If
// the restricted profile contained one option the allowlist did not admit, the composition would be
// impossible without either weakening that allowlist or duplicating the launch — so this test pins
// the property the design depends on, before the design leans on it.
func TestRestrictedFilesystemProfileIsAdmissibleOnTheFilteredCreatePath(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node"}
	for _, mode := range []agents.ExecutionMode{agents.ModeReadOnly, agents.ModeBare} {
		t.Run(string(mode), func(t *testing.T) {
			profile := restrictedFilesystemArgs(cfg, mode)
			if len(profile) == 0 {
				t.Fatal("the restricted profile is empty; it is what makes the mode a sandbox")
			}
			if !runtime.ValidDockerCreateOptionsForTest(profile) {
				t.Errorf("the filtered create path refuses the restricted profile, so the two cannot be composed without weakening one of them:\n%v", profile)
			}
			// The two properties that make it a sandbox at all, spelled out so a future edit to the
			// profile cannot quietly drop one and still pass the allowlist check above.
			joined := strings.Join(profile, " ")
			if !strings.Contains(joined, "--read-only") {
				t.Error("the profile no longer makes the container root read-only")
			}
			if !strings.Contains(joined, "--tmpfs "+cfg.HomeInBox+":") || !strings.Contains(joined, "--tmpfs /tmp:") {
				t.Errorf("the profile no longer owns its scratch: %v", profile)
			}
			if mode == agents.ModeBare && !strings.Contains(joined, "--tmpfs "+BareWorkdir+":") {
				t.Errorf("bare no longer gets an empty owned cwd: %v", profile)
			}
		})
	}
}

// The second precondition, and the one that would fail silently: the filtered launch's agent
// user and restricted scratch ownership must come from the same host identity.
//
// If they ever diverge, a composed read-only/bare + filtered run still starts — and then the agent
// cannot write its own home or /tmp, which reads as a broken client rather than a mismatched
// sandbox. Cheaper to pin the equality than to debug it once.
func TestRestrictedScratchIsOwnedByTheFilteredAgentUser(t *testing.T) {
	id := boxAgentIdentity()
	profile := restrictedFilesystemArgs(&config.Config{HomeInBox: "/home/node"}, agents.ModeReadOnly)
	want := fmt.Sprintf("uid=%d,gid=%d", id.uid, id.gid)
	for i, arg := range profile {
		if arg == "--tmpfs" && i+1 < len(profile) && !strings.Contains(profile[i+1], want) {
			t.Errorf("scratch %q is not owned by the agent user (%s)", profile[i+1], want)
		}
	}
}

// The network the restricted assembly names, by posture. Small, but it is the decision that made
// the first attempt at this composition unable to start at all: the assembly named `--network none`
// while the launch appends `--network container:<controller>`, and Docker accepts that pair at
// create and then refuses to start it.
func TestRestrictedAssemblyUnderFilteredNamesNoNetwork(t *testing.T) {
	// The launch joins the controller's namespace itself. A --network here would leave the box with
	// both that and this one: Docker accepts the pair at create and then refuses to start, so the
	// composed run would die with an error about a network that does not exist.
	if got := restrictedAssemblyNetwork(&config.Config{Egress: "filtered"}); got != "" {
		t.Errorf("filtered assembles --network %q; the launch owns the namespace", got)
	}
	// And the other postures still fail closed / use the plain bridge exactly as before.
	if got := restrictedAssemblyNetwork(&config.Config{Egress: "none"}); got != "none" {
		t.Errorf("offline network = %q, want none", got)
	}
	if got := restrictedAssemblyNetwork(&config.Config{Egress: "open"}); got != "" {
		t.Errorf("open network = %q, want the plain bridge", got)
	}
}

// The composed pair is accepted, each half alone is refused, and bare is refused because it has no
// project for a network policy to be about.
func TestComposedRestrictedFilteredAdmission(t *testing.T) {
	rt := runtime.Runtime{Name: "docker"}
	base := func() (*config.Config, RunSpec) {
		return &config.Config{HomeInBox: "/home/node", BaseImage: "coop-box:x"},
			RunSpec{Image: "coop-box:x", Repo: "/Users/dev/checkout"}
	}
	for _, tc := range []struct {
		name    string
		egress  string
		capture bool
		mode    agents.ExecutionMode
		want    string
	}{
		{"read-only composes with filtered", "filtered", true, agents.ModeReadOnly, ""},
		{"filtered without frozen rules", "filtered", false, agents.ModeReadOnly, "needs the rules admission froze for it"},
		{"frozen rules without filtered", "open", true, agents.ModeReadOnly, "not running filtered"},
		{"bare has no project for a policy", "filtered", true, agents.ModeBare, "use --readonly, or drop --egress filtered"},
		{"read-only still runs open", "open", false, agents.ModeReadOnly, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, spec := base()
			cfg.Egress = tc.egress
			if tc.capture {
				spec.CapturedEgress = &CapturedEgress{}
			}
			if tc.mode == agents.ModeBare {
				spec.Repo = ""
			}
			err := checkRestrictedSpec(cfg, rt, spec, tc.mode)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A read-only SESSION under the gateway must not hand the operator's real MCP secrets to the box.
//
// The session handoff resolves ${VAR} headers and bearer tokens from whatever environment it is
// given, and an OPEN run gives it the broker's stand-ins. Under filtered there is no open broker —
// so before this was fixed the handoff was given the real environment, and a filtered run ended up
// carrying secrets that a filtered run is precisely the mode that never carries: it brokers them or
// refuses the launch. This is the one axis on which the composed sandbox was wider than filtered
// alone, and the test exists because the composition is what made those sessions start at all.
func TestAFilteredReadOnlySessionHandsOverNoRealMCPSecrets(t *testing.T) {
	cfg, spec := readOnlySessionFixture(t)
	cfg.Egress = "filtered"
	spec.CapturedEgress = &CapturedEgress{}

	dir := t.TempDir()
	handoff := filepath.Join(dir, "handoff.json")
	t.Setenv(SessionMCPHandoffEnv, handoff)
	// The launch itself cannot complete here — preparing the gateway needs a real daemon — but the
	// handoff is written before that, which is exactly the artifact under test.
	_, _ = Run(cfg, readOnlySessionShim(t, filepath.Join(dir, "calls"), filepath.Join(dir, "box-env")), spec)

	body, err := os.ReadFile(handoff)
	if err != nil {
		t.Skipf("no session handoff was written: %v", err)
	}
	for _, secret := range []string{"docs-secret", "tickets-secret", "stream-secret"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the session list handed the box a real MCP secret (%s):\n%s", secret, body)
		}
	}
}

// A composed launch, driven for real against the filtered suite's fake daemon: the restricted
// profile has to survive the hand-off into the gateway's own container creation, and exactly one
// network may be named.
//
// The first attempt at this composition had a test that read the profile and passed while the
// composed box could not start at all. This one reads what was actually CREATED.
func TestComposedLaunchCreatesABoxWithBothBoundaries(t *testing.T) {
	f, d := filteredFixture(t)
	cfg := &config.Config{HomeInBox: "/home/node", BaseImage: "coop-box:x", Egress: "filtered"}
	options := restrictedFilesystemArgs(cfg, agents.ModeReadOnly)
	sections := newLaunchSections(RunSpec{Quiet: true})

	started, stopped := false, ""
	spec := RunSpec{Repo: t.TempDir(), Workdir: "/workspace", Quiet: true, Ctx: context.Background()}
	_, _ = launchRestrictedFiltered(f, spec, sections, options, []string{"true"},
		nil, io.Discard, io.Discard, &started, nil, &stopped)

	created, ok := d.containers[f.ref("agent").Name]
	if !ok {
		t.Fatalf("no agent container was created; the daemon saw %v", d.log)
	}
	// The gateway's boundary: the box joins the controller's namespace, and only that.
	if want := "container:" + f.ref("controller").ID; created.NetworkMode != want {
		t.Errorf("network mode = %q, want %q", created.NetworkMode, want)
	}
	// The restricted boundary, as the daemon received it.
	if !created.ReadonlyRootfs {
		t.Error("the composed box's root is writable; the restricted profile did not survive the hand-off")
	}
	for _, scratch := range []string{cfg.HomeInBox, "/tmp"} {
		if _, ok := created.Tmpfs[scratch]; !ok {
			t.Errorf("the composed box has no owned scratch at %s: %v", scratch, created.Tmpfs)
		}
	}
	if created.AutoRemove {
		t.Error("the composed box was created with --rm; the filtered launch owns its removal")
	}
}

// The refusal that keeps a composed box from being wider than a restricted one: a project's own
// image must never become the filesystem the profile is a contract about.
func TestComposedLaunchRefusesAProjectImage(t *testing.T) {
	sections := newLaunchSections(RunSpec{Quiet: true})
	spec := RunSpec{Repo: t.TempDir(), Quiet: true, Ctx: context.Background()}
	started, stopped := false, ""

	f, _ := filteredFixture(t)
	f.record.ProjectImage = "coop-myrepo:derived"
	_, err := launchRestrictedFiltered(f, spec, sections, nil, []string{"true"},
		nil, io.Discard, io.Discard, &started, nil, &stopped)
	if err == nil || !strings.Contains(err.Error(), "project's own box image") {
		t.Errorf("a project image reached a restricted box: %v", err)
	}

}

// The session handoff is where an operator's MCP secrets would reach a box, and a filtered run must
// resolve NOTHING from the real environment. This asserts the decision directly rather than through
// a launch — the previous version of this test went through `Run`, and when the gateway preparation
// moved earlier it silently began SKIPPING instead of asserting, which is how a security guard rots
// inside a green suite.
func TestAFilteredSessionResolvesNoRealMCPSecrets(t *testing.T) {
	cfg, spec := readOnlySessionFixture(t)

	// Open, no broker: the real values are what the servers need, and there is nothing to hide them
	// from — this is the baseline that makes the filtered case below a real difference.
	cfg.Egress = "open"
	if open := sessionStandIns(cfg, spec, nil); len(open.kept) == 0 {
		t.Fatal("the open baseline keeps nothing, so the filtered case below proves nothing")
	}

	// Filtered: nothing kept, so no ${VAR} header or bearer token can resolve to a real secret.
	cfg.Egress = "filtered"
	got := sessionStandIns(cfg, spec, nil)
	if len(got.kept) != 0 {
		t.Errorf("a filtered session would resolve %d real value(s) into the box's server list: %v", len(got.kept), got.kept)
	}
}

// The acceptance criterion this closes: "filtered mode keeps credentials outside the box".
//
// Asserted on the ENV FILE THE BOX RECEIVES, not on the plan that decides it. The first version of
// this test read the host-side env map and exempted ANTHROPIC_API_KEY by name, so it would have
// passed while the raw key rode into the container — the review caught that, and the difference is
// the whole point: the box gets a substitute and the broker's address, and the secret is absent.
func TestComposedRunKeepsABrokeredKeyOutOfTheBoxEnv(t *testing.T) {
	const secret = "raw-provider-secret"
	cfg, _ := brokerFixture(t, "ANTHROPIC_API_KEY="+secret+"\n")
	spec := RunSpec{Agent: "claude", AgentCommand: true, Homes: true, Mode: agents.ModeReadOnly}

	plan, err := selectCredentialPlan(cfg, spec)
	if err != nil || plan == nil || len(plan.routes) != 1 {
		t.Fatalf("the key was not brokered under the gateway: %+v, %v", plan, err)
	}

	f, _ := filteredFixture(t)
	f.broker = &credentialBrokerRun{plan: plan}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = f.runfiles
	if err := f.prepareCredentialBroker(artifacts); err != nil {
		t.Fatalf("prepare the broker: %v", err)
	}

	// The env file as assembled for the box, then rewritten by the broker.
	envFile, _, err := prepareBoxEnvFile(cfg, spec, artifacts, nil)
	if err != nil {
		t.Fatalf("assemble the box env: %v", err)
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), secret) {
		t.Fatal("the fixture's key is not in the assembled env, so the rewrite below proves nothing")
	}
	brokered, err := f.credentialBrokerEnv(artifacts, envFile)
	if err != nil {
		t.Fatalf("rewrite the env for the broker: %v", err)
	}
	body, err := os.ReadFile(brokered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Errorf("the raw key rode into the box:\n%s", body)
	}
	if !strings.Contains(string(body), "ANTHROPIC_API_KEY=") {
		t.Errorf("the box was left with no key at all, brokered or otherwise:\n%s", body)
	}
	if !strings.Contains(string(body), "ANTHROPIC_BASE_URL=http://") {
		t.Errorf("the box was not pointed at the broker:\n%s", body)
	}
}
