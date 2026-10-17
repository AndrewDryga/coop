package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func mountInventoryFixture(t *testing.T, ids []string, inspect string) (Runtime, string) {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", composeFixtureDaemon(t))
	recorder := filepath.Join(t.TempDir(), "commands")
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *\"ps -q\"*) printf '%b' " + strconv.Quote(strings.Join(ids, "\n")) + " ;;\n" +
		"  *\"inspect --type container --format\"*)\n" +
		"    while [ \"$1\" != --format ]; do shift; done\n    shift 2\n" + inspect + "\n;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return Runtime{Name: path}, recorder
}

// A box removed between the listing and its inspect mounts nothing, so it must not abort the sweep
// that asked: one vanished box used to cancel the whole day's temp cleanup. The inspect's wording is
// no proof (Docker 29 prints "error: no such object"), so only the same listing, run again and no
// longer showing the box, counts. A re-listing that fails, or still shows the id — the box itself,
// or another box sharing its short id — keeps the inspect's error.
func TestMountSourcesSkipsABoxThatVanishedBeforeItsInspect(t *testing.T) {
	for _, tc := range []struct {
		name, relist, detail string
		skipped              bool
	}{
		{name: "provably gone", relist: "echo live", skipped: true},
		{name: "re-listing failed", relist: "echo 'daemon query broke' >&2; exit 42", detail: "daemon query broke"},
		{name: "still listed", relist: "printf 'live\\ngone\\n'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cli := filepath.Join(dir, "docker")
			script := `#!/bin/sh
case "$*" in
"ps -q -a --filter label=coop=box")
	if [ -e "$COOP_TEST_LISTED" ]; then eval "$COOP_TEST_RELIST"; else : > "$COOP_TEST_LISTED"; printf 'live\ngone\n'; fi ;;
"inspect --format {{json .Mounts}} live") printf '%s\n' '[{"Source":"/live"}]' ;;
"inspect --format {{json .Mounts}} gone") printf '\nerror: no such object: gone\n' >&2; exit 1 ;;
*) exit 99 ;;
esac
`
			if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_TEST_LISTED", filepath.Join(dir, "listed"))
			t.Setenv("COOP_TEST_RELIST", tc.relist)

			sources, err := (Runtime{Name: cli}).MountSourcesByLabel(context.Background(), "coop", "box")
			if tc.skipped {
				if err != nil || !reflect.DeepEqual(sources, map[string]bool{"/live": true}) {
					t.Fatalf("mount sources = %v, %v; want the live box's mount and the vanished box skipped", sources, err)
				}
				return
			}
			if err == nil || sources != nil || !strings.Contains(err.Error(), "no such object") || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("an unproven disappearance = %v, %v; want the inspect failure and no sources", sources, err)
			}
		})
	}
}

// The daily sweep judges a run's folder by one daemon's word, so a runtime frozen to that daemon
// lists it alone. Every call goes to its socket whatever the current context says by then, so no
// `docker context use` and back mid-listing can slip another daemon's boxes in. Another daemon
// answering on that socket, before the listing or by its end, is an error, never a listing.
func TestFrozenMountSourcesListOneDaemon(t *testing.T) {
	replaced := filepath.Join(t.TempDir(), "daemon-replaced")
	t.Setenv("DOCKER_CONTEXT", "")
	endpoint := dockersock.Serve(t, func() (dockersock.Info, error) {
		id := "daemon-one"
		if _, err := os.Stat(replaced); err == nil {
			id = "daemon-two"
		}
		return dockersock.Info{ID: id, OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	})
	t.Setenv("DOCKER_HOST", endpoint)
	recorder := filepath.Join(t.TempDir(), "commands")
	cli := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\ncase \"$*\" in\n" +
		"*'ps -q -a --filter label=coop=box') echo live ;;\n" +
		"*'inspect --format {{json .Mounts}} live') echo '[{\"Source\":\"/live\"}]'\n" +
		"\t[ -z \"$COOP_TEST_REPLACE\" ] || : > " + strconv.Quote(replaced) + " ;;\nesac\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := (Runtime{Name: cli}).FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "unix:///other.sock")

	sources, err := rt.MountSourcesByLabel(t.Context(), "coop", "box")
	if err != nil || !reflect.DeepEqual(sources, map[string]bool{"/live": true}) {
		t.Fatalf("frozen mount sources = %v, %v; want the frozen daemon's box", sources, err)
	}
	commands, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(commands)), "\n") {
		if !strings.Contains(line, "--host "+endpoint+" ") {
			t.Fatalf("a frozen listing followed the current context: %s", line)
		}
	}
	t.Setenv("COOP_TEST_REPLACE", "1")
	if sources, err := rt.MountSourcesByLabel(t.Context(), "coop", "box"); err == nil || sources != nil {
		t.Fatalf("a listing another daemon finished = %v, %v; want an error", sources, err)
	}
	if sources, err := rt.MountSourcesByLabel(t.Context(), "coop", "box"); err == nil || sources != nil {
		t.Fatalf("a listing of a replaced daemon = %v, %v; want an error", sources, err)
	}
}

func mountRecord(id, mounts string) string {
	return `{"ID":"` + id + `","Status":"exited","Mounts":` + mounts + `}`
}

func TestDockerBindInventoryBatchesCompleteIDsAndFiltersMounts(t *testing.T) {
	ids := make([]string, 89)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i+1)
	}
	inspect := `for id do printf '{"ID":"%s","Status":"exited","Mounts":[{"Type":"bind","Source":"/z","RW":true},{"Type":"bind","Source":"/a","RW":false},{"Type":"volume","Source":"/ignored","RW":true}]}\n' "$id"; done`
	rt, recorder := mountInventoryFixture(t, ids, inspect)
	labels := map[string]string{"z": "two", "a": ""}
	for _, writable := range []bool{false, true} {
		sources, err := rt.bindSourcesByLabels(t.Context(), labels, true, writable)
		want := []string{"/a", "/z"}
		if writable {
			want = []string{"/z"}
		}
		if err != nil || !reflect.DeepEqual(sources, want) {
			t.Fatalf("writable=%v: %v, %v", writable, sources, err)
		}
	}
	commands, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(commands), "inspect --type container --format") != 6 || !strings.Contains(string(commands), "ps -q --no-trunc -a --filter label=a --filter label=z=two") {
		t.Fatalf("inventory did not use sorted filters and three bounded batches per call: %s", commands)
	}
}

func TestDockerBindInventoryRefusesIncompleteObservation(t *testing.T) {
	id := strings.Repeat("a", 64)
	valid := mountRecord(id, `[{"Type":"bind","Source":"/must-not-return","RW":true}]`)
	for _, test := range []struct {
		name    string
		ids     []string
		output  string
		command string
	}{
		{name: "missing", output: ""},
		{name: "duplicate", output: valid + "\n" + valid},
		{name: "wrong identity", output: mountRecord(strings.Repeat("b", 64), "[]")},
		{name: "malformed", output: `{`},
		{name: "trailing JSON", output: valid + `{}`},
		{name: "extra record", output: valid + "\n{}"},
		{name: "extra field", output: strings.TrimSuffix(valid, "}") + `,"Env":"private-canary"}`},
		{name: "missing mounts", output: `{"ID":"` + id + `","Status":"exited"}`},
		{name: "missing status", output: `{"ID":"` + id + `","Mounts":[]}`},
		{name: "unknown status", output: `{"ID":"` + id + `","Status":"unknown","Mounts":[]}`},
		{name: "malformed mounts", output: mountRecord(id, `{}`)},
		{name: "missing writable fact", output: mountRecord(id, `[{"Type":"bind","Source":"/unknown"}]`)},
		{name: "missing bind source", output: mountRecord(id, `[{"Type":"bind","RW":true}]`)},
		{name: "oversized", output: mountRecord(id, `[{"Type":"volume","Source":"`+strings.Repeat("x", 1<<20)+`"}]`)},
		{name: "partial command failure", output: valid, command: "exit 1"},
		{name: "short listed identity", ids: []string{"abc"}, output: valid},
		{name: "duplicate listed identity", ids: []string{id, id}, output: valid},
	} {
		t.Run(test.name, func(t *testing.T) {
			ids := test.ids
			if ids == nil {
				ids = []string{id}
			}
			inspect := "printf '%b\\n' " + strconv.Quote(test.output) + "\nprintf '%s\\n' private-canary >&2\n" + test.command
			rt, _ := mountInventoryFixture(t, ids, inspect)
			sources, err := rt.BindSourcesByLabels(t.Context(), nil)
			if err == nil || sources != nil || strings.Contains(err.Error(), "private-canary") {
				t.Fatalf("unavailable inventory returned data or private diagnostics: %v, %v", sources, err)
			}
		})
	}
}

func TestDockerBindInventoryAllowsEmptyAndNullMounts(t *testing.T) {
	for _, ids := range [][]string{nil, {strings.Repeat("a", 64)}} {
		rt, _ := mountInventoryFixture(t, ids, `for id do printf '{"ID":"%s","Status":"exited","Mounts":null}\n' "$id"; done`)
		if sources, err := rt.BindSourcesByLabels(t.Context(), nil); err != nil || len(sources) != 0 {
			t.Fatalf("complete empty inventory = %v, %v", sources, err)
		}
	}
}

func TestDockerBindInventoryStreamsMultipleBoundedRecords(t *testing.T) {
	ids := make([]string, 9)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i+1)
	}
	// More than the control-call aggregate bound, but each discarded mount record is bounded.
	format := `{"ID":"%s","Status":"exited","Mounts":[{"Type":"volume","Source":"` + strings.Repeat("x", 512<<10) + `"}]}\n`
	rt, _ := mountInventoryFixture(t, ids, "for id do printf '"+format+"' \"$id\"; done")
	if sources, err := rt.BindSourcesByLabels(t.Context(), nil); err != nil || len(sources) != 0 {
		t.Fatalf("bounded records were incorrectly limited in aggregate: %v, %v", sources, err)
	}
}

func TestDockerBindInventoryCancellationReturnsNoPartialSources(t *testing.T) {
	id := strings.Repeat("a", 64)
	rt, _ := mountInventoryFixture(t, []string{id}, "printf '%s\\n' '"+mountRecord(id, `[{"Type":"bind","Source":"/partial","RW":true}]`)+"'\nexec sleep 30")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if sources, err := rt.BindSourcesByLabels(ctx, nil); err == nil || sources != nil {
		t.Fatalf("cancelled inventory = %v, %v", sources, err)
	}
}

func TestDockerBindInventoryRefusesDaemonReplacementDuringObservation(t *testing.T) {
	for _, stage := range []string{"empty enumeration", "enumeration", "inspect"} {
		t.Run(stage, func(t *testing.T) {
			id := strings.Repeat("a", 64)
			rt, _ := mountInventoryFixture(t, nil, "")
			marker := filepath.Join(t.TempDir(), "daemon-replaced")
			t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
				identity := "daemon-one"
				if _, err := os.Stat(marker); err == nil {
					identity = "daemon-two"
				}
				return dockersock.Info{ID: identity, OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
			}))
			ps := "printf '%s\\n' '" + id + "'"
			inspect := "printf '%s\\n' '" + mountRecord(id, `[{"Type":"bind","Source":"/unproven","RW":true}]`) + "'"
			replace := "touch " + strconv.Quote(marker) + "\n"
			if stage == "inspect" {
				inspect += "\n" + replace
			} else {
				ps = replace + ps
				if stage == "empty enumeration" {
					ps = replace
				}
			}
			script := "#!/bin/sh\ncase \"$*\" in\n*\"ps -q\"*)\n" + ps + "\n;;\n*\"inspect --type container --format\"*)\n" + inspect + "\n;;\nesac\n"
			if err := os.WriteFile(rt.Name, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if sources, err := rt.BindSourcesByLabels(t.Context(), nil); err == nil || sources != nil || !strings.Contains(err.Error(), "daemon identity changed") {
				t.Fatalf("mixed-daemon inventory accepted: %v, %v", sources, err)
			}
		})
	}
}

func TestDockerBindInventoryCoalescesStoppedEvidenceConservatively(t *testing.T) {
	ids := make([]string, 33)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i+1)
	}
	for _, status := range []string{"created", "exited", "running", "paused", "restarting", "dead"} {
		for _, position := range []int{0, 32} {
			t.Run(fmt.Sprintf("%s-%d", status, position), func(t *testing.T) {
				inspect := `for id do status=exited; if [ "$id" = '` + ids[position] + `' ]; then status=` + status + `; fi; printf '{"ID":"%s","Status":"%s","Mounts":[{"Type":"bind","Source":"/shared","RW":false}]}\n' "$id" "$status"; done`
				rt, _ := mountInventoryFixture(t, ids, inspect)
				mounts, err := rt.BindMountsByLabels(t.Context(), nil, true, false)
				stopped := status == "created" || status == "exited"
				if err != nil || len(mounts) != 1 || mounts[0].Source != "/shared" || mounts[0].Stopped != stopped {
					t.Fatalf("coalesced mount custody = %v, %v", mounts, err)
				}
			})
		}
	}
}

func TestDockerBindInventoryLateBatchFailureReturnsNoSources(t *testing.T) {
	ids := make([]string, 33)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i+1)
	}
	inspect := `for id do [ "$id" != '` + ids[32] + `' ] || continue; printf '{"ID":"%s","Status":"exited","Mounts":[{"Type":"bind","Source":"/partial","RW":true}]}\n' "$id"; done`
	rt, _ := mountInventoryFixture(t, ids, inspect)
	if sources, err := rt.BindSourcesByLabels(t.Context(), nil); err == nil || sources != nil {
		t.Fatalf("late incomplete batch returned partial authority: %v, %v", sources, err)
	}
}
