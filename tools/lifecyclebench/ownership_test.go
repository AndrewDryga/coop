package main

import (
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	boxFilter     = "label=coop=box"
	gatewayFilter = "label=coop.network.run"
)

// resource is one resource of the fake runtime's world and the label filter that lists it.
type resource struct{ name, filter string }

// fakeRuntime answers ps and volume ls from a fixed world and records what it was asked.
//
// Every stop number this tool publishes means "nothing the run owned is left". That claim is only as
// good as what the question covers, so the question itself is what these tests pin. appear joins the
// world once the file launched exists (a stand-in coop creates it), and a removal takes a resource
// out of it.
type fakeRuntime struct {
	containers, volumes []resource
	labels              map[string]labels // by name
	appear              [2][]resource     // containers, volumes
	launched            string
	onAppear            func(runArgs string) // called with the launched file's text when the world appears
	commands            [][]string
	removed             map[string]bool
}

func (f *fakeRuntime) command(_ time.Duration, name string, args ...string) (result, error) {
	if name != "docker" {
		return result{code: 1}, nil
	}
	f.commands = append(f.commands, slices.Clone(args))
	if f.launched != "" {
		if runArgs, err := os.ReadFile(f.launched); err == nil {
			if f.onAppear != nil {
				f.onAppear(string(runArgs))
			}
			f.containers = append(f.containers, f.appear[0]...)
			f.volumes = append(f.volumes, f.appear[1]...)
			f.launched = ""
		}
	}
	filter := ""
	if i := slices.Index(args, "--filter"); i >= 0 {
		filter = args[i+1]
	}
	verb, last := args[0], args[len(args)-1]
	if verb == "volume" {
		verb += " " + args[1]
	}
	listed := func(world []resource) result {
		var names []string
		for _, r := range world {
			if r.filter == filter {
				names = append(names, r.name)
			}
		}
		return result{stdout: strings.Join(names, "\n")}
	}
	switch verb {
	case "inspect", "volume inspect":
		l := f.labels[last]
		return result{stdout: l.host + "\t" + l.network + "\t" + l.sample + "\n"}, nil
	case "rm", "volume rm":
		if f.removed == nil {
			f.removed = map[string]bool{}
		}
		f.removed[last] = true
		gone := func(r resource) bool { return r.name == last }
		f.containers, f.volumes = slices.DeleteFunc(f.containers, gone), slices.DeleteFunc(f.volumes, gone)
		return result{}, nil
	case "ps":
		return listed(f.containers), nil
	case "volume ls":
		return listed(f.volumes), nil
	}
	return result{}, nil
}

// testBench is a bench on fake, its samples named "tok".
func testBench(fake *fakeRuntime) *bench {
	return &bench{runtime: "docker", command: fake.command, start: (*exec.Cmd).Start, token: func() string { return "tok" },
		log: io.Discard, caseTimeout: caseTimeout, goneTimeout: 300 * time.Millisecond, stopSteps: stopSteps}
}

func items(names ...string) set {
	s := set{}
	for _, name := range names {
		s[name] = true
	}
	return s
}

func TestOwnershipClaimsOnlyWhatItsLaunchMade(t *testing.T) {
	// The bench removes what its own launch made and nothing else: another run's boxes and volumes
	// (a loop in another repository, an editor session) are theirs, data included.
	for _, tc := range []struct {
		name   string
		labels map[string]labels
		heard  string
		items  []string
		want   []string
	}{{
		name: "its box, and the gateway volume of the run it printed",
		labels: map[string]labels{"mine": {host: "v1:ws:4242:t"}, "theirs": {host: "v1:ws:999:t"},
			"ipc": {network: "1a"}, "their-db": {network: "2b"}, "unknown": {}},
		heard: "coop net inspect 1a",
		items: []string{"c:mine", "c:theirs", "v:ipc", "v:their-db", "v:unknown"},
		want:  []string{"c:mine", "v:ipc"},
	}, {
		name:   "its box claims its gateway without the printed run",
		labels: map[string]labels{"mine": {host: "v1:ws:4242:t", network: "1a"}, "guard": {network: "1a"}, "other": {network: "2b"}},
		items:  []string{"c:mine", "c:guard", "c:other"},
		want:   []string{"c:mine", "c:guard"},
	}, {
		name:   "a pid inside another number is not its own",
		labels: map[string]labels{"theirs": {host: "v1:ws:14242:t"}},
		items:  []string{"c:theirs"},
	}, {
		// An editor session's boxes are started by its per-provider coop processes, not the one the
		// bench launched; they carry the sample's label all the same, and claim their gateways.
		name: "a box with the sample's label, whichever process started it",
		labels: map[string]labels{"child-box": {"v1:ws:5151:t", "3c", "tok"}, "its-ipc": {network: "3c"},
			"other-box": {"v1:ws:6161:t", "4d", "another-sample"}, "other-ipc": {network: "4d"}},
		items: []string{"c:child-box", "v:its-ipc", "c:other-box", "v:other-ipc"},
		want:  []string{"c:child-box", "v:its-ipc"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ours := testBench(&fakeRuntime{labels: tc.labels}).newOwnership()
			ours.pid = 4242
			ours.heard(tc.heard)
			if got, err := ours.mine(items(tc.items...)); err != nil || !maps.Equal(got, items(tc.want...)) {
				t.Errorf("mine = %v, %v; want %v", slices.Sorted(maps.Keys(got)), err, tc.want)
			}
		})
	}
}

func TestOwnershipRemembersWhatItLearned(t *testing.T) {
	fake := &fakeRuntime{labels: map[string]labels{"mine": {host: "v1:ws:4242:t", network: "5e"}, "ipc": {network: "5e"}}}
	ours := testBench(fake).newOwnership()
	ours.pid = 4242
	// A box seen while it lived still claims its gateway once it is gone.
	if got, _ := ours.mine(items("c:mine")); !maps.Equal(got, items("c:mine")) {
		t.Fatalf("its box: %v", got)
	}
	if got, _ := ours.mine(items("v:ipc")); !maps.Equal(got, items("v:ipc")) {
		t.Errorf("its gateway, after the box went: %v", got)
	}
	// Labels never change, so each resource is inspected once.
	for range 3 {
		_, _ = ours.mine(items("c:mine"))
	}
	inspected := 0
	for _, command := range fake.commands {
		if command[0] == "inspect" {
			inspected++
		}
	}
	if inspected != 1 {
		t.Errorf("the box was inspected %d times", inspected)
	}
}

func TestItReadsTheNetworkRunALaunchPrints(t *testing.T) {
	said := "Networking stats:\n  Allowed   no external connections\nFull details: coop net inspect 61727efd6e10631b31d8ae600cfa3a4d --json\n"
	if got := networkRuns(said); !slices.Equal(got, []string{"61727efd6e10631b31d8ae600cfa3a4d"}) {
		t.Errorf("networkRuns = %q", got)
	}
}

func TestOwnedSpansBoxesGatewaysAndVolumes(t *testing.T) {
	// A filtered launch owns more than the agent box, and a stop that only counts boxes lies: the
	// exact failure this tool exists to catch is the agent box gone and its gateway not.
	fake := &fakeRuntime{containers: []resource{{"box1", boxFilter}, {"guard1", gatewayFilter}},
		volumes: []resource{{"ipc1", gatewayFilter}}}
	got, err := testBench(fake).owned()
	if err != nil || !maps.Equal(got, items("c:box1", "c:guard1", "v:ipc1")) {
		t.Errorf("owned = %v, %v", got, err)
	}
	asked := map[string]bool{}
	for _, command := range fake.commands {
		asked[command[0]+" "+command[1]] = true
	}
	if !maps.Equal(asked, map[string]bool{"ps -aq": true, "volume ls": true}) {
		t.Errorf("asked %v; both containers AND volumes must be asked about", slices.Sorted(maps.Keys(asked)))
	}
}

func TestAFailedListingIsNotNothingLeft(t *testing.T) {
	// Read as an empty answer, a runtime that cannot answer would end a stop's clock early.
	b := testBench(&fakeRuntime{})
	b.command = func(time.Duration, string, ...string) (result, error) {
		return result{stderr: "Cannot connect to the Docker daemon", code: 1}, nil
	}
	if _, err := b.owned(); err == nil || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("owned error = %v", err)
	}
}

func TestOnlyTheRuntimesNoSuchAnswerMeansGone(t *testing.T) {
	// A resource that went between the listing and its inspection is gone; any other failed
	// inspection is an error, never "gone".
	for stderr, wantErr := range map[string]bool{"Error: No such object: box1": false, "Cannot connect to the Docker daemon": true} {
		b := testBench(&fakeRuntime{})
		b.command = func(time.Duration, string, ...string) (result, error) { return result{stderr: stderr, code: 1}, nil }
		mine, err := b.newOwnership().mine(items("c:box1"))
		if (err != nil) != wantErr || len(mine) != 0 {
			t.Errorf("%q: mine %v, err %v", stderr, mine, err)
		}
	}
}

func TestWaitUntilGoneCountsOnlyItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fake   *fakeRuntime
		before set
		gone   bool
	}{{
		// Containers clear immediately; the volume never does. A stop is not over until both are.
		name: "a surviving volume means not gone",
		fake: &fakeRuntime{volumes: []resource{{"ipc1", gatewayFilter}}, labels: map[string]labels{"ipc1": {network: "1a"}}},
	}, {
		name: "gone when nothing new remains",
		fake: &fakeRuntime{},
		gone: true,
	}, {
		// Another project's stopped containers are not evidence against this run.
		name:   "what was already there is not this run's leftover",
		fake:   &fakeRuntime{containers: []resource{{"someone-elses", boxFilter}}},
		before: items("c:someone-elses"),
		gone:   true,
	}, {
		name: "another run starting meanwhile is not waited for",
		fake: &fakeRuntime{containers: []resource{{"their-box", boxFilter}},
			labels: map[string]labels{"their-box": {"v1:ws:999:t", "", "theirs"}}},
		gone: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBench(tc.fake)
			ours := b.newOwnership()
			ours.heard("coop net inspect 1a")
			if gone, err := b.waitUntilGone(context.Background(), tc.before, ours); err != nil || gone != tc.gone {
				t.Errorf("gone = %v, %v; want %v", gone, err, tc.gone)
			}
		})
	}
}

func TestCleanupRemovesAVolumeAsAVolume(t *testing.T) {
	fake := &fakeRuntime{}
	if err := testBench(fake).remove(items("c:box1", "v:ipc1")); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"rm", "-f", "box1"}, {"volume", "rm", "-f", "ipc1"}}
	if !slices.EqualFunc(fake.commands, want, slices.Equal[[]string]) {
		t.Errorf("commands = %q", fake.commands)
	}
}
