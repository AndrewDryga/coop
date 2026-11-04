package box

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
)

func TestServiceSecretNoticePathsStayOnOneLine(t *testing.T) {
	got := safeServicePaths([]string{"safe.key", "evil\x1b[2J\n✓ forged\u202e.key"})
	if strings.ContainsAny(got, "\x1b\n\r") || strings.ContainsRune(got, '\u202e') ||
		!strings.Contains(got, `\u001B`) || !strings.Contains(got, `\u000A`) || !strings.Contains(got, `\u202E`) {
		t.Fatalf("secret notice can forge terminal output: %q", got)
	}
}

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
	for _, want := range []string{"startup failed: compose refused: unsafe bind", "could not confirm any available sibling service", "injected no service URL or forwarder", "coop up"} {
		if !strings.Contains(failed, want) {
			t.Errorf("degraded note lacks %q:\n%s", want, failed)
		}
	}
	if strings.Contains(failed, "localhost:") {
		t.Errorf("a failed start must not list an address as reachable:\n%s", failed)
	}
	partial := servicesNote(serviceLaunchOutcome{state: servicesFailed, err: errors.New("keycloak unhealthy")}, []ServicePort{db}, true, nil)
	for _, want := range []string{"startup was partial", "keycloak unhealthy", "Only the services observed below", "sidecar db: tcp://localhost:41234", "coop up"} {
		if !strings.Contains(partial, want) {
			t.Errorf("partial service note lacks %q:\n%s", want, partial)
		}
	}
	observed := servicesNote(serviceLaunchOutcome{state: servicesObserved}, []ServicePort{db}, true, nil)
	if !strings.Contains(observed, "startup was not requested") || !strings.Contains(observed, "sidecar db: tcp://localhost:41234") {
		t.Errorf("existing observed service note is not truthful:\n%s", observed)
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

// A filtered box publishes on its gateway controller, and the note reads that one decision: its
// agent hears exactly what an open box's agent hears, and the host is not probed a second time.
func TestFilteredNoteReportsTheControllersPublishDecision(t *testing.T) {
	spec := RunSpec{Repo: "/tmp/serve-plan", Serve: true, servePorts: []int{3000, 5173}}
	firstFree := func() func(int) bool {
		calls := 0
		return func(int) bool { calls++; return calls == 1 }
	}
	notConfigured := serviceLaunchOutcome{state: servicesNotConfigured}
	published, skipped := project.HostPort(spec.Repo, 3000), project.HostPort(spec.Repo, 5173)

	options, plan, _ := filteredPublish(&config.Config{Egress: "filtered"}, spec, firstFree())
	if bound := fmt.Sprintf("127.0.0.1:%d:3000", published); !slices.Equal(options, []string{"-p", bound}) {
		t.Fatalf("the controller was given %v, want only %s", options, bound)
	}
	got := launchServePlan(&config.Config{Egress: "filtered"}, spec, &filteredExecution{servePlan: plan}, func(int) bool {
		t.Fatal("a filtered launch decided publication already and must not probe the host again")
		return false
	})
	note := servicesNote(notConfigured, nil, false, got)
	for _, want := range []string{
		fmt.Sprintf("- your port 3000 is published: a browser on the host reaches it at http://localhost:%d", published),
		fmt.Sprintf("- your port 5173 is NOT published: host port %d is already in use by another process", skipped),
	} {
		if !strings.Contains(note, want) {
			t.Errorf("the filtered note is missing %q:\n%s", want, note)
		}
	}
	open := launchServePlan(&config.Config{Egress: "open"}, spec, nil, firstFree())
	if openNote := servicesNote(notConfigured, nil, false, open); note != openNote {
		t.Errorf("a filtered note differs from an open one:\n%s\nopen:\n%s", note, openNote)
	}

	// A filtered box that did not ask to publish has nothing to report, and an offline one decides
	// the way it always did.
	if got := launchServePlan(&config.Config{Egress: "filtered"}, spec, &filteredExecution{}, func(int) bool {
		t.Fatal("no publication was decided")
		return false
	}); got != nil {
		t.Fatalf("an unrequested filtered plan = %+v", got)
	}
	if got := launchServePlan(&config.Config{Egress: "none"}, spec, nil, func(int) bool { return true }); got != nil {
		t.Fatalf("an offline plan = %+v", got)
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
		if !strings.HasPrefix(string(data), "# base\n") || !strings.Contains(string(data), "startup failed: daemon down") {
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

// An editor session forwards the stable port to its live box, so the box binds a private port and is
// only told the stable address. Whether it can publish depends on the private port alone.
func TestServeBindSeparatesWhatABoxBindsFromItsAddress(t *testing.T) {
	cfg := &config.Config{Egress: "open"}
	spec := RunSpec{Repo: "/tmp/serve-plan", Serve: true, servePorts: []int{3000}, ServeBinds: map[int]int{3000: 41234}}
	var probed []int
	plan := servePublicationPlan(cfg, spec, func(host int) bool { probed = append(probed, host); return true })
	if len(plan) != 1 || !plan[0].Published || len(probed) != 1 || probed[0] != 41234 {
		t.Fatalf("plan = %+v after probing %v, want only the private port probed", plan, probed)
	}
	stable := plan[0].Host
	spec.servePlan = plan
	joined := strings.Join(appendPublish(nil, cfg, spec, nil), " ")
	if !strings.Contains(joined, "-p 127.0.0.1:41234:3000") || strings.Contains(joined, fmt.Sprintf("127.0.0.1:%d:", stable)) {
		t.Fatalf("publish args = %s, want the private port bound and the stable one left alone", joined)
	}
	if !strings.Contains(joined, fmt.Sprintf("COOP_SERVE_URL_3000=http://localhost:%d", stable)) {
		t.Fatalf("publish args = %s, want the stable URL announced", joined)
	}
	options, filtered, env := filteredPublish(cfg, spec, func(int) bool { return true })
	if strings.Join(options, " ") != "-p 127.0.0.1:41234:3000" || filtered[0].Host != stable {
		t.Fatalf("filtered publish = %v, %+v", options, filtered)
	}
	if !strings.Contains(strings.Join(env, " "), fmt.Sprintf("COOP_SERVE_URL_3000=http://localhost:%d", stable)) {
		t.Fatalf("filtered env = %v", env)
	}
}
