package box

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// The facts an agent needs about its sidecars are all known before the box starts, so they are
// stated rather than discovered by failing to connect. Each case here is one thing the box may
// have got; the degraded case is the whole point.
func TestServicesNoteStatesWhatTheBoxGot(t *testing.T) {
	web := ServicePort{Service: "web", ContainerPort: 80, HostPort: 48732, Scheme: "http"}
	db := ServicePort{Service: "db", ContainerPort: 5432, HostPort: 41234, Scheme: "tcp"}
	published := []servePublication{{Port: 3000, Host: 42076, Published: true}}
	skipped := []servePublication{{Port: 5173, Host: 56119, Published: false}}

	if got := servicesNote(serviceLaunchOutcome{state: servicesNotConfigured}, nil, true, nil); got != "" {
		t.Fatalf("a repo with no compose file and nothing to publish must say nothing, got %q", got)
	}
	got := servicesNote(serviceLaunchOutcome{state: servicesRunning}, []ServicePort{web, db}, true, published)
	for _, want := range []string{
		"# Services and ports (coop)",
		"sidecar db: tcp://localhost:41234 from this box (its own port 5432; also db:5432 by name on the services network)",
		"sidecar web: http://localhost:48732 from this box (its own port 80; also web:80 by name on the services network)",
		"your port 3000 is published: a browser on the host reaches it at http://localhost:42076",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("note lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "sidecar db") > strings.Index(got, "sidecar web") {
		t.Errorf("sidecars are not listed in a stable order:\n%s", got)
	}
	if strings.Contains(got, "COOP_FORWARD") {
		t.Errorf("the note restates plumbing instead of addresses:\n%s", got)
	}

	failed := servicesNote(serviceLaunchOutcome{state: servicesFailed, err: errors.New("compose refused: unsafe bind")}, nil, true, nil)
	for _, want := range []string{"did NOT start: compose refused: unsafe bind", "runs without them", "coop up"} {
		if !strings.Contains(failed, want) {
			t.Errorf("degraded note lacks %q:\n%s", want, failed)
		}
	}
	if strings.Contains(failed, "localhost:") {
		t.Errorf("a failed start must not list an address as reachable:\n%s", failed)
	}

	unjoined := servicesNote(serviceLaunchOutcome{state: servicesRunning}, []ServicePort{web}, false, nil)
	if !strings.Contains(unjoined, "not on their network") || strings.Contains(unjoined, "localhost:48732") {
		t.Errorf("a box off the services network must not be told an address it cannot reach:\n%s", unjoined)
	}

	if got := servicesNote(serviceLaunchOutcome{state: servicesRunning}, nil, true, nil); !strings.Contains(got, "none exposes a port") {
		t.Errorf("services with no exposed port need saying:\n%s", got)
	}
	for name, outcome := range map[string]serviceLaunchOutcome{
		"disabled": {state: servicesUnknown},
		"live box": {state: servicesSkipped, err: errors.New("another box is running")},
	} {
		got := servicesNote(outcome, []ServicePort{web}, true, nil)
		if !strings.Contains(got, "availability was not checked") || strings.Contains(got, "services are running") || strings.Contains(got, "sidecar web:") {
			t.Errorf("%s service note overclaimed availability:\n%s", name, got)
		}
	}
	if got := servicesNote(serviceLaunchOutcome{state: servicesNotConfigured}, nil, true, skipped); !strings.Contains(got, "port 5173 is NOT published: host port 56119 is already in use") {
		t.Errorf("a skipped publish must be stated as such:\n%s", got)
	}
}

// One decision feeds both the agent's note and the runtime's publish arguments, so they cannot
// disagree about a port; the stable URL is announced even when this box could not bind it.
func TestServePublicationIsDecidedOnce(t *testing.T) {
	cfg := &config.Config{Egress: "open"}
	spec := RunSpec{Repo: "/tmp/serve-plan", Serve: true, servePorts: []int{3000, 5173}}
	calls := 0
	plan := servePublicationPlan(cfg, spec, func(host int) bool { calls++; return calls == 1 })
	if len(plan) != 2 || !plan[0].Published || plan[1].Published || calls != 2 {
		t.Fatalf("plan = %+v after %d checks", plan, calls)
	}
	spec.servePlan = plan
	args := appendPublish(nil, cfg, spec, func(int) bool { t.Fatal("a planned publish must not re-check the port"); return false })
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "COOP_SERVE_URL_3000=") || !strings.Contains(joined, "COOP_SERVE_URL_5173=") {
		t.Fatalf("both stable URLs must be announced: %s", joined)
	}
	if strings.Count(joined, "-p 127.0.0.1:") != 1 || !strings.Contains(joined, ":3000") {
		t.Fatalf("exactly the published port is bound: %s", joined)
	}
	if got := servePublicationPlan(&config.Config{Egress: "none"}, spec, func(int) bool { return true }); got != nil {
		t.Fatalf("an offline box publishes nothing, got %+v", got)
	}
	spec.Serve = false
	if got := requestedServePublicationPlan(cfg, spec, func(int) bool {
		t.Fatal("a run that did not request host publication must not inspect host ports")
		return true
	}); got != nil {
		t.Fatalf("a run without Serve publishes nothing or claims a host URL, got %+v", got)
	}
}

// The note reaches every agent's instruction file, appended after the files were assembled,
// because sidecars start after the box's files are laid out.
func TestAppendInstructionNoteReachesEveryAgent(t *testing.T) {
	dir := t.TempDir()
	var mounts []extraMount
	for _, agent := range []string{"claude", "codex"} {
		p := filepath.Join(dir, agent+".md")
		if err := os.WriteFile(p, []byte("# base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mounts = append(mounts, extraMount{p, "/home/node/." + agent + "/INSTRUCTIONS.md"})
	}
	note := servicesNote(serviceLaunchOutcome{state: servicesFailed, err: errors.New("daemon down")}, nil, true, nil)
	if err := appendInstructionNote(mounts, note); err != nil {
		t.Fatal(err)
	}
	for _, m := range mounts {
		data, err := os.ReadFile(m.host)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), "# base\n") || !strings.Contains(string(data), "did NOT start: daemon down") {
			t.Errorf("%s = %q", m.host, data)
		}
	}
	if err := appendInstructionNote(mounts, ""); err != nil {
		t.Fatalf("an empty note must be a no-op, got %v", err)
	}
	if err := appendInstructionNote([]extraMount{{filepath.Join(dir, "missing.md"), "/x"}}, note); err == nil {
		t.Fatal("a missing instruction file must surface, not be skipped")
	}
}
