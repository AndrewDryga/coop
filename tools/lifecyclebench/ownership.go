package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
)

// Everything a run can own. coop=box is the agent box; a filtered launch also creates a gateway
// guard and controller under coop.network.* (internal/box/filtered.go) plus volumes for their ipc
// and observations. A check that knows only the first reports "nothing left" while the second set
// is still there — which is precisely the failure this tool exists to catch, so it must not have it.
var ownedFilters = []string{"label=" + box.LabelKey + "=" + box.LabelBox, "label=" + box.LabelNetworkRun}

// sampleLabel is on every box a sample starts (through COOP_RUN_ARGS, which an editor session's
// per-provider coop processes inherit too), so cleanup can tell the sample's boxes from another run's.
const sampleLabel = "coop-bench.sample"

func newToken() string { return rand.Text() }

// networkRun is how a filtered launch names its network run at the end of its output.
var networkRun = regexp.MustCompile(`coop net inspect ([0-9a-f]+)`)

func networkRuns(output string) []string {
	var runs []string
	for _, match := range networkRun.FindAllStringSubmatch(output, -1) {
		runs = append(runs, match[1])
	}
	return runs
}

// set holds runtime resources as "c:<container>" and "v:<volume>".
type set map[string]bool

func (s set) minus(other set) set {
	out := set{}
	for item := range s {
		if !other[item] {
			out[item] = true
		}
	}
	return out
}

func (b *bench) ownedContainers() (set, error) {
	return b.list("c:", "ps", "-aq")
}

func (b *bench) ownedVolumes() (set, error) {
	return b.list("v:", "volume", "ls", "-q")
}

// owned is every container and volume a coop run could own, by the labels the product sets.
func (b *bench) owned() (set, error) {
	containers, err := b.ownedContainers()
	if err != nil {
		return nil, err
	}
	volumes, err := b.ownedVolumes()
	if err != nil {
		return nil, err
	}
	maps.Copy(containers, volumes)
	return containers, nil
}

// list asks the runtime for one kind of resource under every owned label. A listing that fails is an
// error, never an empty answer: read as "nothing left", it would end a stop's clock early.
func (b *bench) list(kind string, args ...string) (set, error) {
	found := set{}
	for _, filter := range ownedFilters {
		query := append(slices.Clip(args), "--filter", filter)
		res, err := b.executeCommand(30*time.Second, b.runtime, query...)
		if err == nil && res.code != 0 {
			err = fmt.Errorf("%s %s exited %d: %s", b.runtime, strings.Join(query, " "), res.code, firstOf(res.stderr))
		}
		if err != nil {
			return nil, err
		}
		for _, name := range strings.Fields(res.stdout) {
			found[kind+name] = true
		}
	}
	return found, nil
}

// labels decide a leftover's owner: the coop process supervising a box (coop.host,
// v2:<scope>:<host>:<pid>:<token>), the network run, and the sample label.
type labels struct{ host, network, sample string }

// labels inspects one resource; false when the runtime says it no longer has it. Any other failure
// is an error: read as "gone", it would end a stop's clock early, as a failed listing would.
func (b *bench) labels(item string) (labels, bool, error) {
	kind, name, _ := strings.Cut(item, ":")
	args := []string{"volume", "inspect", "--format", fmt.Sprintf("\t{{index .Labels %q}}\t", box.LabelNetworkRun), name}
	if kind == "c" {
		args = []string{"inspect", "--format", fmt.Sprintf("{{index .Config.Labels %q}}\t{{index .Config.Labels %q}}\t{{index .Config.Labels %q}}",
			box.LabelHost, box.LabelNetworkRun, sampleLabel), name}
	}
	res, err := b.executeCommand(30*time.Second, b.runtime, args...)
	switch {
	case err != nil:
		return labels{}, false, err
	case res.code != 0 && strings.Contains(strings.ToLower(res.stderr), "no such"):
		return labels{}, false, nil
	case res.code != 0:
		return labels{}, false, fmt.Errorf("%s inspect %s exited %d: %s", b.runtime, name, res.code, firstOf(res.stderr))
	}
	fields := append(strings.Split(strings.TrimRight(res.stdout, "\n"), "\t"), "", "")
	return labels{fields[0], strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])}, true, nil
}

// ownership is what one sample's launch made, and nothing else: its boxes (they carry the sample's
// label, or name the coop process it started in coop.host) and the gateway containers and volumes
// of a network run those boxes carry or the launch printed. A resource another run created
// meanwhile is never touched. Labels never change, so each resource is inspected once, and a box
// seen while it lived still claims its gateway after it is gone.
type ownership struct {
	b      *bench
	token  string
	pid    int
	runs   map[string]bool
	labels map[string]labels
}

func (b *bench) newOwnership() *ownership {
	return &ownership{b: b, token: b.token(), runs: map[string]bool{}, labels: map[string]labels{}}
}

func (o *ownership) heard(output string) {
	for _, run := range networkRuns(output) {
		o.runs[run] = true
	}
}

func (o *ownership) mine(items set) (set, error) {
	for item := range items {
		if _, known := o.labels[item]; known {
			continue
		}
		// One that went between the listing and this question is not remembered as someone else's.
		if found, present, err := o.b.labels(item); err != nil {
			return nil, err
		} else if present {
			o.labels[item] = found
		}
	}
	mark := ""
	if o.pid != 0 {
		mark = fmt.Sprintf(":%d:", o.pid)
	}
	boxes := set{}
	for item := range items {
		if l := o.labels[item]; l.sample == o.token || mark != "" && strings.Contains(l.host, mark) {
			boxes[item] = true
			if l.network != "" {
				o.runs[l.network] = true
			}
		}
	}
	mine := set{}
	for item := range items {
		if boxes[item] || o.runs[o.labels[item].network] {
			mine[item] = true
		}
	}
	return mine, nil
}

// left is what this sample's launch made that the runtime still has.
func (b *bench) left(before set, ours *ownership) (set, error) {
	now, err := b.owned()
	if err != nil {
		return nil, err
	}
	return ours.mine(now.minus(before))
}

// cleanUp removes what this sample's launch left behind, and only that. The tool's own wreckage must
// not be mistaken for the product's, and must not poison the next sample's before-set.
func (b *bench) cleanUp(before set, ours *ownership) {
	now, err := b.owned()
	if err == nil {
		leftovers := now.minus(before)
		var mine set
		if mine, err = ours.mine(leftovers); err == nil {
			err = b.remove(mine)
			if others := len(leftovers) - len(mine); others > 0 {
				fmt.Fprintf(b.log, "lifecyclebench: left %d resource(s) this sample did not create\n", others)
			}
		}
	}
	if err != nil {
		fmt.Fprintf(b.log, "lifecyclebench: could not clean up after a sample: %v\n", err)
	}
}

// remove takes items out of the runtime. Containers go first ("c:" sorts before "v:"): a volume
// cannot be removed while a container uses it.
func (b *bench) remove(items set) error {
	var errs []error
	for _, item := range slices.Sorted(maps.Keys(items)) {
		kind, name, _ := strings.Cut(item, ":")
		args := []string{"rm", "-f", name}
		if kind == "v" {
			args = []string{"volume", "rm", "-f", name}
		}
		if _, err := b.executeCommand(60*time.Second, b.runtime, args...); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// waitUntilGone polls the containers, then confirms the volumes, until nothing of this sample's own
// is left or the gone timeout passes.
//
// Every runtime query costs ~20 ms, and a stop interval here is a few hundred milliseconds, so
// asking four questions per iteration would measure this loop instead of the product. Volumes are
// created and destroyed with the gateway that owns them, so the cheap poll watches containers and
// the volumes are checked when those are clear. Only the sample's own resources count: one another
// run created meanwhile is not this run's to wait for.
func (b *bench) waitUntilGone(ctx context.Context, before set, ours *ownership) (bool, error) {
	deadline := time.Now().Add(b.goneTimeout)
	phases := []struct {
		poll  func() (set, error)
		every time.Duration
	}{{b.ownedContainers, 10 * time.Millisecond}, {b.ownedVolumes, 50 * time.Millisecond}}
	for _, phase := range phases {
		for {
			// Asked before the deadline is checked: a stall must not make "gone" go unasked.
			found, err := phase.poll()
			if err != nil {
				return false, err
			}
			mine, err := ours.mine(found.minus(before))
			if err != nil {
				return false, err
			}
			if len(mine) == 0 {
				break
			}
			if !time.Now().Before(deadline) {
				return false, nil
			}
			if err := sleep(ctx, phase.every); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// sleep waits d, or until a person's Ctrl-C.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errInterrupted
	case <-timer.C:
		return nil
	}
}
