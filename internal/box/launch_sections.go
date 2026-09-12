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
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/ui"
)

// An interactive launch is narrated for the person watching the terminal: bold, unprefixed
// sections for the host-side work before the agent's output begins, one unprefixed sentence when
// the box has stopped, and the sealed network run after it. Loop attempts opt into a nested setup
// form that flows through their live bar. Other embeddings keep their own bounded log, so for
// those every method here is a no-op.

// interactive reports whether coop narrates this run for a person: not a batch iteration, not a
// quiet probe, not an ACP child.
func (spec RunSpec) interactive() bool { return !spec.Batch && !spec.Quiet && !spec.ForceNoTTY }

// launchSections is the narration of one interactive launch or batch loop attempt. subject is what
// the last section starts — the agent's product name, or the command a raw run was given.
type launchSections struct {
	on          bool
	interactive bool
	loop        bool
	agent       bool
	subject     string
	notice      string
	opened      bool // a section heading has been printed, so a failure nests under it
	nested      bool // a nested loop setup section has been printed
	current     string
}

func newLaunchSections(spec RunSpec) *launchSections {
	interactive := spec.interactive()
	return &launchSections{
		on: interactive || spec.LoopPresentation, interactive: interactive,
		loop: spec.LoopPresentation, agent: spec.AgentCommand,
		subject: launchSubject(spec), notice: spec.StartingNotice,
	}
}

// section opens one narration section. The first one leads the command's output, so it carries no
// blank line in front of it; every later one is separated from what came before.
func (s *launchSections) section(title string) {
	if s.loop {
		if !s.opened {
			ui.Heading("Preparing task environment")
			s.opened = true
		} else if s.current == title {
			return
		} else if s.nested {
			ui.Note("")
		}
		ui.Note("  %s", ui.Bold(title))
		s.nested = true
		s.current = title
		return
	}
	if s.opened {
		ui.Section(title)
		return
	}
	ui.Heading(title)
	s.opened = true
}

func launchSubject(spec RunSpec) string {
	if spec.LoopPresentation && !spec.AgentCommand && len(spec.Cmd) > 0 {
		return cleanLaunchText(strings.Join(spec.Cmd, " "))
	}
	if ag, ok := agents.Get(spec.Agent); ok {
		return ag.DisplayName()
	}
	if len(spec.Cmd) > 0 {
		return path.Base(spec.Cmd[0])
	}
	return "the box"
}

func cleanLaunchText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
					j++
				}
				if j < len(s) {
					j++
				}
				i = j
				continue
			}
			if j < len(s) {
				i = j + 1
				continue
			}
			break
		}
		if s[i] < 0x20 || s[i] == 0x7f {
			b.WriteByte(' ')
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// box is the optional first section: what a person should hear about the image this run is
// about to use — a definition that drifted, an image a month old — as cautions, never as a
// `coop:` line. The cli renders the same heading when it repairs a definition mismatch before
// Run; a repaired image is current and fresh, so the two never both appear.
func (s *launchSections) box(nudges []string) {
	if !s.on || len(nudges) == 0 {
		return
	}
	s.section("Checking the Coop box")
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
	s.section("Protecting secrets")
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
	// One heading in every mode: this section is coop APPLYING the selected network access, which
	// it does for unrestricted and offline runs too — not a promise that a filter is running.
	s.section("Configuring network access")
	switch {
	case policy != nil:
		for _, row := range networkAllowances(*policy) {
			ui.Pass("%s", row)
		}
		ui.Pass("Everything else blocked")
	case cfg.Egress == "open":
		ui.Caution("Unrestricted — nothing is blocked")
	default:
		if s.loop && spec.AgentCommand {
			ui.Note("%s", ui.Red("  ⚠ Offline — "+offlineText(spec)))
		} else {
			ui.Caution("Offline — %s", offlineText(spec))
		}
	}
}

// networkAllowances lists what a filtered box may reach, one row per kind of allowance, in the
// order a person checks them: each selected provider's endpoints, the network rules a human
// approved (the project's rules and this run's --allow-domain/--egress-rules), the MCP servers
// coop configured. Every grant lands in one of these rows — or in the catch-all last one — so
// the closing "Everything else blocked" is never claimed over an omitted allowance.
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
		// The count is of normalized approved network rules — not of expanded
		// addresses, and not of distinct sites a rule might cover.
		rows = append(rows, "Applied "+ui.Count(n, "approved network rule"))
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
	if spec.LoopPresentation && !spec.AgentCommand {
		return "nothing outside the box can be reached"
	}
	if ag, ok := agents.Get(spec.Agent); ok {
		name := ag.DisplayName()
		if spec.LoopPresentation {
			name = spec.Agent
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		return name + " cannot reach " + ag.Vendor()
	}
	return "nothing outside the box can be reached"
}

// starting is the last section. It has no result line of its own: what follows it is the
// agent's output, or the nested failure when the main process never started.
func (s *launchSections) starting() {
	if !s.on {
		return
	}
	if s.loop {
		// Registered loop providers announce their resolved identity at the decoder's init
		// boundary. A custom command has no such event, so the runtime boundary names it here.
		if s.agent {
			return
		}
		lines := loopStartingLines(s.subject, loopLaunchWidth())
		ui.Section(lines[0])
		for _, line := range lines[1:] {
			ui.Note("%s", ui.Bold(line))
		}
		return
	}
	s.section("Starting " + s.subject)
	if s.notice != "" {
		ui.Note("%s", s.notice)
	}
}

func loopStartingLines(subject string, width int) []string {
	return ui.PrefixedLines("Starting ", cleanLaunchText(subject), width)
}

func loopLaunchWidth() int {
	w := ui.TermWidth(os.Stderr)
	if w < 2 {
		return 1
	}
	return w - 1
}

// stopped is the one lifecycle sentence an interactive box prints, and it is a CLAIM: the box has
// stopped. So it waits until the stop is confirmed — the main process returned AND whatever
// teardown this process owns has removed the box — and never announces a stop still in progress.
// The reason is the truthful one stopReason derived; a blank line sets the sentence off from
// whatever arbitrary output the agent or shell left above it.
func (s *launchSections) stopped(reason string) {
	if !s.interactive {
		return
	}
	ui.Note("\nThe Coop box has stopped — %s.", reason)
}

// stopSlow is how long a teardown may run before a person watching a silent terminal deserves to
// be told what it is waiting on. A var so a test can drop it to zero.
var stopSlow = 2 * time.Second

// stopping notes that shutdown is underway, but ONLY when it outlasts stopSlow: a fast teardown
// needs no narration, and the completed sentence follows either way. It returns the stop for the
// timer, which the caller defers so the notice can never print after the box is already gone.
func (s *launchSections) stopping() func() {
	if !s.interactive {
		return func() {}
	}
	timer := time.AfterFunc(stopSlow, func() { ui.Note("\nStopping the Coop box…") })
	return func() { timer.Stop() }
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
	if !s.interactive || err == nil || interrupt.reason() == "" || !errors.Is(err, context.Canceled) {
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

// services narrates the project's sibling services: the section, each service Compose actually
// resolved, and the result. The names are the resolved ones, so a section never lists a service
// this project does not declare.
func (s *launchSections) services(names []string) {
	if !s.on {
		return
	}
	title := "Starting project services"
	if s.loop {
		title = "Starting services"
	}
	s.section(title)
	for _, name := range names {
		ui.Note("  %s", cleanLaunchText(name))
	}
	if s.loop {
		ui.Pass("Services are running")
		return
	}
	ui.Pass("Services started")
}

func (s *launchSections) servicesPreparing() {
	if s.loop {
		s.section("Starting services")
	}
}

func (s *launchSections) serviceSecrets(hidden []string, composeFile string) {
	if !s.loop || len(hidden) == 0 {
		return
	}
	s.section("Starting services")
	pathNoun := "a secret path"
	if len(hidden) > 1 {
		pathNoun = fmt.Sprintf("%d secret paths", len(hidden))
	}
	ui.Note("  %s Services received an empty file for %s", ui.Yellow("⚠"), pathNoun)
	for _, name := range hidden {
		loopLaunchDetail(name, 4)
	}
	file := cleanLaunchText(path.Base(composeFile))
	loopLaunchDetail("To allow the real file, run coop up and approve "+file+".", 4)
	loopLaunchDetail("Approval lasts until "+file+" changes.", 4)
}

func (s *launchSections) servicesRefused(cause string) {
	if !s.loop {
		return
	}
	s.section("Starting services")
	ui.Note("  %s", ui.Red("✗ Services were not started"))
	ui.Note("")
	loopLaunchDetail(cause, 8)
	ui.Note("")
	loopLaunchDetail("Review the service approval or Compose file, then start the loop again.", 4)
}

// servicesFailed is a launch that CONTINUES without the services it could not start. It is a
// warning, not a failure: the box is about to run, and the person needs the compose error and the
// command that retries it. A review launch refuses the whole launch instead and never reaches here.
func (s *launchSections) servicesFailed(cause string) {
	if !s.on {
		return
	}
	if !s.loop {
		ui.Warning("Project services could not start", cause, "Run coop up to retry.")
		return
	}
	s.section("Starting services")
	ui.Note("  %s Services failed to start%s", ui.Yellow("⚠"), serviceExitSummary(cause))
	for _, line := range serviceCauseDetails(cause) {
		if line != "" {
			loopLaunchDetail(line, 4)
		}
	}
	ui.Note("    Continuing without sibling services.")
	ui.Note("    To retry: coop up")
}

// servicesSkipped is a launch that did not even attempt them, with the reason it did not. It never
// claims services already running are unavailable — only that none were started now.
func (s *launchSections) servicesSkipped(cause string) {
	if !s.on {
		return
	}
	if !s.loop {
		ui.Warning("Project services were not started", cause, "Stop that box, then run coop up.")
		return
	}
	s.section("Starting services")
	ui.Note("  %s Services were not started", ui.Yellow("⚠"))
	loopLaunchDetail(cause, 4)
	ui.Note("    To retry: coop up")
}

// servicesHeldByLiveBox is the deliberate no-start path: another box owns the project lifecycle.
// It does not claim the services are healthy, unavailable, or safe to restart under that owner.
func (s *launchSections) servicesHeldByLiveBox(cause string) {
	if !s.on {
		return
	}
	if !s.loop {
		ui.Warning("Project service startup was skipped", cause, "Stop that box, then run coop up.")
		return
	}
	s.section("Starting services")
	ui.Note("  %s Service startup skipped", ui.Yellow("⚠"))
	loopLaunchDetail(cause, 4)
	loopLaunchDetail("Service availability was not checked; Coop will not restart services while that box is active.", 4)
}

func loopLaunchDetail(text string, indent int) {
	width := loopLaunchWidth() - indent
	for _, line := range ui.WrapLines(cleanLaunchText(text), width) {
		ui.Note("%s%s", strings.Repeat(" ", indent), line)
	}
}

func serviceExitSummary(cause string) string {
	lower := strings.ToLower(cause)
	for _, marker := range []string{"exit status ", "exited with status "} {
		i := strings.Index(lower, marker)
		if i < 0 {
			continue
		}
		rest := cause[i+len(marker):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end > 0 {
			return " · exit " + rest[:end]
		}
	}
	return ""
}

func serviceCauseDetails(cause string) []string {
	trimmed := strings.TrimSpace(cause)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"compose up exited with status ", "exit status "} {
		if rest, ok := strings.CutPrefix(lower, prefix); ok && rest != "" {
			onlyDigits := true
			for _, r := range rest {
				if r < '0' || r > '9' {
					onlyDigits = false
					break
				}
			}
			if onlyDigits {
				return nil
			}
		}
	}
	return strings.Split(trimmed, "\n")
}

// boundedCause is the bounded reason a warning or failure carries: what the tool itself printed,
// then the error, trimmed to the first few lines so a launch never dumps a screen of Compose
// output into the middle of its narration. Empty tool output falls back to the error alone.
func boundedCause(output string, err error) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if err != nil {
		lines = append(lines, err.Error())
	}
	kept := make([]string, 0, 4)
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
		if len(kept) == 4 {
			break
		}
	}
	return strings.Join(kept, "\n")
}
