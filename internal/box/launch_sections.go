package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/ui"
)

// An interactive launch is narrated for the person watching the terminal: bold, unprefixed
// sections for the host-side work before the agent's output begins, one `coop:` line when the
// box stops, and the sealed network run after it. Every other embedding keeps its bounded log —
// a loop iteration owns a live bar, a doctor probe captures its output, an ACP child's stderr is
// an editor's log — so for those every method here is a no-op.

// interactive reports whether coop narrates this run for a person: not a batch iteration, not a
// quiet probe, not an ACP child.
func (spec RunSpec) interactive() bool { return !spec.Batch && !spec.Quiet && !spec.ForceNoTTY }

// launchSections is the narration of one interactive launch. subject is what the last section
// starts — the agent's product name, or the command a raw run was given.
type launchSections struct {
	on      bool
	subject string
	opened  bool // a section heading has been printed, so a failure nests under it
}

func newLaunchSections(spec RunSpec) *launchSections {
	return &launchSections{on: spec.interactive(), subject: launchSubject(spec)}
}

func launchSubject(spec RunSpec) string {
	if ag, ok := agents.Get(spec.Agent); ok {
		return ag.DisplayName()
	}
	if len(spec.Cmd) > 0 {
		return path.Base(spec.Cmd[0])
	}
	return "the box"
}

// box is the optional first section: what a person should hear about the image this run is
// about to use — a definition that drifted, an image a month old — as cautions, never as a
// `coop:` line. The cli renders the same heading when it repairs a definition mismatch before
// Run; a repaired image is current and fresh, so the two never both appear.
func (s *launchSections) box(nudges []string) {
	if !s.on || len(nudges) == 0 {
		return
	}
	s.opened = true
	ui.Section("Checking the Coop box")
	for _, nudge := range nudges {
		ui.Caution("%s", nudge)
	}
}

// secrets is how many secret-looking paths the mount plan hid. The count is the exact plan's;
// a checkout with nothing to hide says so instead of inventing one.
func (s *launchSections) secrets(hidden int) {
	if !s.on {
		return
	}
	s.opened = true
	ui.Section("Protecting secrets")
	if hidden == 0 {
		ui.Pass("No secret paths to hide")
		return
	}
	ui.Pass("%s hidden from the box", ui.Count(hidden, "secret path"))
}

// internet is the one stable section every mode shares. A filtered run lists what the frozen
// policy allows and only then claims the rest is blocked; an open run and an offline run get a
// warning under the same heading, never a green row.
func (s *launchSections) internet(cfg *config.Config, spec RunSpec, policy *egress.Snapshot) {
	if !s.on {
		return
	}
	s.opened = true
	ui.Section("Internet access")
	switch {
	case policy != nil:
		for _, row := range networkAllowances(*policy) {
			ui.Pass("%s", row)
		}
		ui.Pass("Everything else blocked")
	case cfg.Egress == "open":
		ui.Caution("Unrestricted — nothing is blocked")
	default:
		ui.Caution("Offline — %s", offlineText(spec))
	}
}

// networkAllowances lists what a filtered box may reach, one row per kind of allowance, in the
// order a person checks them: each selected provider's endpoints, the websites and services a
// human approved (the project's rules and this run's --allow-domain/--egress-rules), the MCP
// servers coop configured. Every grant lands in one of these rows — or in the catch-all last
// one — so the closing "Everything else blocked" is never claimed over an omitted allowance.
func networkAllowances(policy egress.Snapshot) []string {
	var vendors []string
	provider := func(name string) {
		if ag, ok := agents.Get(name); ok {
			name = ag.Vendor()
		}
		if name != "" && !slices.Contains(vendors, name) {
			vendors = append(vendors, name)
		}
	}
	// The selected bundles come first, lead before peers; a grant with a provider origin the
	// dependency list somehow lacks still gets its row rather than vanishing.
	for _, dependency := range policy.Dependencies {
		provider(dependency.Provider)
	}
	approved, servers := map[string]bool{}, map[string]bool{}
	other := 0
	for _, grant := range policy.Grants {
		represented := false
		for _, origin := range grant.Origins {
			switch origin.Kind {
			case "provider":
				provider(origin.Provider)
			case "project", "operator":
				approved[grant.ID] = true
			case "mcp":
				servers[origin.Name] = true
			default:
				continue
			}
			represented = true
		}
		if !represented {
			other++
		}
	}
	var rows []string
	for _, vendor := range vendors {
		rows = append(rows, vendor+" endpoints allowed")
	}
	if n := len(approved); n > 0 {
		rows = append(rows, ui.Count(n, "approved website/service", "approved websites/services")+" allowed")
	}
	if n := len(servers); n > 0 {
		rows = append(rows, ui.Count(n, "configured MCP service")+" allowed")
	}
	if other > 0 {
		rows = append(rows, ui.Count(other, "other destination")+" allowed")
	}
	return rows
}

func offlineText(spec RunSpec) string {
	if ag, ok := agents.Get(spec.Agent); ok {
		return ag.DisplayName() + " cannot reach " + ag.Vendor()
	}
	return "nothing outside the box can be reached"
}

// starting is the last section. It has no result line of its own: what follows it is the
// agent's output, or the nested failure when the main process never started.
func (s *launchSections) starting() {
	if !s.on {
		return
	}
	s.opened = true
	ui.Section("Starting " + s.subject)
}

// stopping is the one lifecycle line an interactive box prints when its teardown begins. It
// carries the `coop:` anchor because it follows arbitrary agent output; it says "stopping", not
// "stopped", because cleanup has not run yet.
func (s *launchSections) stopping(reason string) {
	if !s.on {
		return
	}
	ui.Note("stopping the box — %s", reason)
}

// failed renders a launch that stopped before its main process as the nested failure of the
// section in progress and returns the error marked reported, so the dispatcher adds nothing on
// top. A cancellation is not explained: the person who pressed Ctrl-C knows why it stopped.
func (s *launchSections) failed(err error) error {
	if !s.on || !s.opened || err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ui.ErrReported) {
		// Before any section has opened there is nothing to nest under: a refusal by name reads
		// as the dispatcher's own plain ✗ line, exactly like a usage error.
		return err
	}
	ui.Fail("Could not start "+s.subject, err.Error(), "")
	return ui.Reported(err)
}

// explained marks a cancellation the stop line has already named — "interrupted by Ctrl-C" —
// as reported, so the dispatcher does not add a bare "✗ interrupted" beneath the run it just
// printed. The error itself is untouched: the exit status and errors.Is still see a cancellation.
// Any other failure of a started box keeps its full message, which the stop line only led with.
func (s *launchSections) explained(err error, interrupt *hostInterrupt) error {
	if !s.on || err == nil || interrupt.reason() == "" || !errors.Is(err, context.Canceled) {
		return err
	}
	return ui.Reported(err)
}

// stopReason is the truthful cause of a teardown: the host signal that cancelled the run, the
// exit status the main process returned, or the failure that ended it. Nothing is read into an
// exit status — a workload that exits 130 on its own exited 130.
func stopReason(code int, err error, interrupt *hostInterrupt) string {
	if reason := interrupt.reason(); reason != "" {
		return reason
	}
	if err == nil && code >= 125 && code <= 127 {
		// The client's own statuses: 125 is a run the daemon refused, 126 and 127 a command it
		// could not invoke or find. A main process can return them too, and nothing here can
		// tell the two apart, so neither is claimed — the runtime already printed its reason.
		return fmt.Sprintf("the runtime returned status %d (its own error, or the main process's)", code)
	}
	if err == nil && code >= 0 {
		return fmt.Sprintf("main process exited with status %d", code)
	}
	if err != nil {
		return strings.SplitN(err.Error(), "\n", 2)[0]
	}
	return "main process ended without an exit status"
}

// hostInterrupt turns the first SIGINT/SIGTERM into a cancellation the filtered cleanup can act
// on — a box's gateway, volumes and receipt are exact-owned, none of it is --rm, so a signal that
// killed coop where it stood would strand three containers — and REMEMBERS which signal it was,
// so teardown can name it instead of inferring it from an exit status. A second signal takes the
// default action, so a wedged teardown is still escapable.
type hostInterrupt struct {
	mu  sync.Mutex
	sig os.Signal
}

func newHostInterrupt() (context.Context, *hostInterrupt) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &hostInterrupt{}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-signals
		h.mu.Lock()
		h.sig = sig
		h.mu.Unlock()
		signal.Stop(signals)
		cancel()
	}()
	return ctx, h
}

// reason names the signal that arrived, or "" when none did. A nil recorder never saw one.
func (h *hostInterrupt) reason() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch h.sig {
	case nil:
		return ""
	case os.Interrupt:
		return "interrupted by Ctrl-C"
	case syscall.SIGTERM:
		return "stopped by SIGTERM"
	}
	return "stopped by signal " + h.sig.String()
}
