package box

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	"github.com/AndrewDryga/coop/internal/runtime"
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
	on            bool
	interactive   bool
	loop          bool
	agent         bool
	subject       string
	notice        string
	opened        bool // a section heading has been printed, so a failure nests under it
	nested        bool // a nested loop setup section has been printed
	current       string
	secretsOpened bool
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
	if spec.Subject != "" {
		return cleanLaunchText(spec.Subject)
	}
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
	s.prepareSecrets()
	if hidden == 0 {
		ui.Pass("No secret paths to hide")
		return
	}
	ui.Pass("%s hidden from the box", ui.Count(hidden, "secret path"))
}

// mountProtection says that this launch hides only the secrets that exist now. Kernel protection also
// hides one saved later, so a person who has it never needs the line.
func (s *launchSections) mountProtection() {
	if s.on {
		ui.Note("  Secrets saved after the box starts stay readable to it. `coop doctor` explains why this launch uses mount protection.")
	}
}

func (s *launchSections) prepareSecrets() {
	if s.on && !s.secretsOpened {
		s.section("Protecting secrets")
		s.secretsOpened = true
	}
}

func (s *launchSections) filesystemProgress(message string) {
	if s.on {
		ui.Note("  %s", message)
	}
}

// coopignoreComments warns about .coopignore entries with a comment after the pattern: such an
// entry is one pattern, comment and all, so it hides nothing its author meant to. Under Protecting
// secrets in a narrated launch; a plain warning elsewhere, unless the run is quiet.
func (s *launchSections) coopignoreComments(entries []string, quiet bool) {
	for _, entry := range entries {
		switch {
		case s.on:
			ui.Caution(".coopignore entry %q is one pattern, comment and all; put comments on their own line", entry)
		case !quiet:
			ui.Warn(".coopignore entry %q is one pattern, comment and all; put comments on their own line", entry)
		}
	}
}

// caseExposed warns about hidden files a box can still read under another letter case, which the
// launch's owner accepted through approval or the one-run override.
func (s *launchSections) caseExposed(paths []string, quiet bool) {
	if len(paths) == 0 {
		return
	}
	switch {
	case s.on:
		ui.Caution("Accepted case exposure: %d hidden files may be read under another letter case.", len(paths))
	case !quiet:
		ui.Warn("Accepted case exposure: %d hidden files may be read under another letter case.", len(paths))
	}
}

// accountRow is one account a launch connects: the provider, the account, and whether its API key
// is protected — kept on this computer — or it is a signed-in login. Names only: no credential value
// ever reaches presentation.
type accountRow struct {
	provider, account string
	protected         bool
}

// accounts is the launch's account section: one stable row per account the run selected, in the
// order the run uses them, then — once, and only when an API key is protected — what that means. A
// run without an account, like a raw command, prints no section.
func (s *launchSections) accounts(rows []accountRow) {
	if !s.on || len(rows) == 0 {
		return
	}
	plural := len(rows) > 1
	if plural {
		s.section("Connecting accounts")
	} else {
		s.section("Connecting account")
	}
	protected := false
	for _, row := range rows {
		state := "Signed in"
		if row.protected {
			state, protected = "API key protected", true
		}
		ui.Pass("%s (%s) · %s", strings.ToUpper(row.provider[:1])+row.provider[1:], row.account, state)
	}
	if !protected {
		return
	}
	ui.Note("")
	if plural {
		ui.Note("  Agents can use these accounts; the keys never leave this computer.")
	} else {
		ui.Note("  The agent can use this account; the key never leaves this computer.")
	}
}

// internet is the one stable section every mode shares. A filtered run lists what the frozen
// policy allows and only then claims the rest is blocked; an open run and an offline run get a
// warning under the same heading, never a green row. A filtered run nothing chose a mode for
// first says that the default applies and how to leave it.
// providers names those whose accounts the run connects and brokeredMCP the MCP servers it reaches
// through its credential broker: allowed like the rest, though the agent's own policy holds no grant
// for a brokered one.
func (s *launchSections) internet(cfg *config.Config, spec RunSpec, policy *egress.Snapshot, providers []string, brokeredMCP ...string) {
	if !s.on {
		return
	}
	// One heading in every mode: this section is coop APPLYING the selected network access, which
	// it does for unrestricted and offline runs too — not a promise that a filter is running.
	s.section("Configuring network access")
	switch {
	case policy != nil:
		if spec.CapturedEgress != nil && spec.CapturedEgress.Default {
			ui.Note("  Filtered by default. Use --egress open for unrestricted access.")
		}
		rows := networkAllowances(*policy, providers, brokeredMCP...)
		for _, row := range rows {
			ui.Pass("%s", row)
		}
		if len(rows) == 0 {
			ui.Pass("Everything blocked") // nothing above to be "else" to: a raw run with no rules
		} else {
			ui.Pass("Everything else blocked")
		}
	case cfg.Egress == "open":
		ui.Caution("Unrestricted: nothing is blocked")
	default:
		if s.loop && spec.AgentCommand {
			ui.Note("%s", ui.Red("  ⚠ Offline: "+offlineText(spec)))
		} else {
			ui.Caution("Offline: %s", offlineText(spec))
		}
	}
}

// projectRulesSetAside tells an open or offline run that the project's network rules are not part
// of it. Admission lets such a run start with rules only for a one-off --egress open or offline;
// any other way the file and the mode disagree is refused as pending before a box exists.
func (s *launchSections) projectRulesSetAside(rules int) {
	if !s.on || rules == 0 {
		return
	}
	ui.Note("  This project's network rules do not apply to this run.")
}

// offlineMCP names the remote MCP servers an offline box goes without, under its network section:
// they need internet, so the agent never sees them — and the person should not wonder where they went.
func (s *launchSections) offlineMCP(omitted []string) {
	if !s.on || len(omitted) == 0 {
		return
	}
	ui.Note("  MCP servers that need internet are left out: %s", strings.Join(omitted, ", "))
}

// openMCP says, under an open run's network section, which MCP servers keep their secrets outside
// the box, and why any other still carries its secret inside.
func (s *launchSections) openMCP(brokered, kept []string) {
	if !s.on {
		return
	}
	if len(brokered) > 0 {
		ui.Note("  MCP secrets stay outside the box: %s", strings.Join(brokered, ", "))
	}
	for _, reason := range kept {
		ui.Note("  %s", reason)
	}
}

// legacyMCP names the brokered servers still on the legacy SSE transport. Their route is wider than
// every other — POST anywhere on that one host — so the person who configured them is told, and
// told what to ask for instead.
func (s *launchSections) legacyMCP(servers []string) {
	if !s.on || len(servers) == 0 {
		return
	}
	ui.Note("  %s: legacy SSE uses a wider broker route. Ask for a streamable HTTP URL: %s",
		ui.Count(len(servers), "MCP server"), strings.Join(servers, ", "))
}

// brokerImage says why an open run's first start is slow: the helper that keeps its MCP secrets is
// built from Coop's own gateway sources on first use.
func (s *launchSections) brokerImage() {
	if s.on {
		ui.Note("  Building the image for Coop's MCP credential broker; the first build takes a few minutes")
	}
}

// NetworkAllowed is what a filtered policy lets a box reach, grouped in the order a person checks
// it: each selected provider's vendor, the network rules a human approved (the project's rules),
// the rules given for this run only (--allow-domain, --egress-rules), the MCP servers coop
// configured, and a count of anything else. Every grant lands in one group, so "everything else is
// blocked" never hides an allowance.
type NetworkAllowed struct {
	Vendors  []string
	Approved []string // one per approved rule, in policy order: what it reaches
	ThisRun  []string // one per rule given for this run only and not also approved
	Servers  []string // sorted
	Other    int
}

// NetworkAllowedBy groups policy's grants. providers names those whose accounts a run connects:
// Coop's broker reaches a brokered one's endpoints for it, so the policy holds no grant for them,
// and brokeredMCP names the MCP servers it reaches the same way.
func NetworkAllowedBy(policy egress.Snapshot, providers []string, brokeredMCP ...string) NetworkAllowed {
	var allowed NetworkAllowed
	provider := func(name string) {
		if ag, ok := agents.Get(name); ok {
			name = ag.Vendor()
		}
		if name != "" && !slices.Contains(allowed.Vendors, name) {
			allowed.Vendors = append(allowed.Vendors, name)
		}
	}
	// The run's accounts come first and then the selected bundles, lead before peers; a grant with a
	// provider origin the dependency list somehow lacks still gets its vendor rather than vanishing.
	for _, name := range providers {
		provider(name)
	}
	for _, dependency := range policy.Dependencies {
		provider(dependency.Provider)
	}
	servers := map[string]bool{}
	for _, name := range brokeredMCP {
		servers[name] = true
	}
	for _, grant := range policy.Grants {
		represented, project, operator := false, false, false
		for _, origin := range grant.Origins {
			switch origin.Kind {
			case "provider":
				provider(origin.Provider)
			case "project":
				project = true
			case "operator":
				operator = true // a launch flag, or `coop net setup`: this run's alone, never approved
			case "mcp":
				servers[origin.Name] = true
			default:
				continue
			}
			represented = true
		}
		switch {
		case project:
			allowed.Approved = append(allowed.Approved, destinationName(grant.Rule.To))
		case operator:
			allowed.ThisRun = append(allowed.ThisRun, destinationName(grant.Rule.To))
		}
		if !represented {
			allowed.Other++
		}
	}
	allowed.Servers = slices.Sorted(maps.Keys(servers))
	return allowed
}

// destinationName is what an approved rule reaches, in the words its YAML used.
func destinationName(to egress.Destination) string {
	switch {
	case to.Domain != "":
		return to.Domain
	case to.Service != "":
		return "the " + to.Service + " service"
	case to.IP != "":
		return to.IP
	case to.CIDR != "":
		return to.CIDR
	case to.Provider != "":
		if ag, ok := agents.Get(to.Provider); ok {
			return ag.Vendor() + " endpoints"
		}
		return to.Provider + " endpoints"
	}
	return "another destination"
}

// networkAllowances lists what a filtered box may reach, one row per kind of allowance, so the
// closing "Everything else blocked" is never claimed over an omitted allowance.
func networkAllowances(policy egress.Snapshot, providers []string, brokeredMCP ...string) []string {
	allowed := NetworkAllowedBy(policy, providers, brokeredMCP...)
	var rows []string
	for _, vendor := range allowed.Vendors {
		rows = append(rows, vendor+" endpoints allowed")
	}
	if n := len(allowed.Approved); n > 0 {
		// The count is of normalized approved network rules — not of expanded
		// addresses, and not of distinct sites a rule might cover.
		rows = append(rows, "Applied "+ui.Count(n, "approved network rule"))
	}
	if n := len(allowed.ThisRun); n > 0 {
		rows = append(rows, "Applied "+ui.Count(n, "network rule")+" for this run only")
	}
	if n := len(allowed.Servers); n > 0 {
		rows = append(rows, ui.Count(n, "configured MCP service")+" allowed")
	}
	if allowed.Other > 0 {
		rows = append(rows, ui.Count(allowed.Other, "other destination")+" allowed")
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
// whatever arbitrary output the agent or shell left above it. No reason means the main process
// never ran, and a box that never started has not stopped.
func (s *launchSections) stopped(reason string) {
	if !s.interactive || reason == "" {
		return
	}
	ui.Note("\nThe Coop box has stopped: %s.", reason)
}

// stopping announces cleanup as soon as the provider exits. A Ctrl-C handled
// inside the provider never reaches this point; stopped still requires proof.
func (s *launchSections) stopping() {
	if s.interactive {
		ui.Note("\nStopping %s", s.subject)
	}
}

// failed renders a launch that stopped before its main process as the nested failure of the
// section in progress and returns the error marked reported, so the dispatcher adds nothing on
// top. A cancellation is not explained: the person who pressed Ctrl-C knows why it stopped.
func (s *launchSections) failed(err error) error {
	var fallback *FilesystemFallbackError
	var unreadable UnreadableCoopignore
	if errors.As(err, &fallback) || errors.As(err, &unreadable) {
		// The CLI combines the selection reason with the mount refusal's remedy, and says how to
		// fix a .coopignore it cannot read. Printing here would say it twice, the first without the
		// fix, and expand every hidden path first.
		return err
	}
	if !s.on || !s.opened || err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ui.ErrReported) {
		// Before any section has opened there is nothing to nest under: a refusal by name reads
		// as the dispatcher's own plain ✗ line, exactly like a usage error.
		return err
	}
	ui.Fail("Could not start "+s.subject, err.Error(), "")
	return ui.Reported(err)
}

// ended settles a launch that returned result, once every cleanup has run: what the person is told,
// and the status coop exits with.
//
// A launch a person stopped before its main process ran is not a failure to explain. Cleanup removed
// what it made, so one line says the box never started, and the status is the shell's for a command
// a signal ended (130 for Ctrl-C). Whatever the stop broke on the way, a cancelled Docker call or
// Docker's own complaint, goes unsaid. When cleanup itself could not finish, the person needs the
// whole failure, so nothing is hidden and the status stays the launch's own.
func (s *launchSections) ended(started bool, interrupt *hostInterrupt, teardownErr error, exitCode int, result error) (int, error) {
	stopStatus := 0
	if teardownErr == nil {
		stopStatus = stoppedStatus(interrupt, result)
	}
	switch {
	case !started && result != nil && stopStatus != 0:
		if s.interactive {
			NoteStoppedBeforeStart()
			result = ui.Reported(result)
		}
	case !started:
		result = s.failed(result)
	case teardownErr == nil:
		result = s.explained(result, interrupt)
	}
	if result != nil && stopStatus != 0 && (!started || errors.Is(result, context.Canceled)) {
		exitCode = stopStatus
	}
	return exitCode, result
}

// NoteStoppedBeforeStart is the one line a launch ends with when a person stopped it before its main
// process ran, once cleanup has removed what it set up. Admission says it too, for a stop that lands
// there, so both read the same.
func NoteStoppedBeforeStart() { ui.Note("\nStopped before the box started.") }

// stoppedStatus is the exit status of a launch a person stopped, or 0 when nobody did. The stop
// reached coop as a host signal, or reached only the box: runtime.ErrDockerStartInterrupted is a
// Ctrl-C that Docker's client, which held the terminal, passed to the box's own first process.
func stoppedStatus(interrupt *hostInterrupt, err error) int {
	if status := interrupt.exitCode(); status != 0 {
		return status
	}
	if errors.Is(err, runtime.ErrDockerStartInterrupted) {
		return 128 + int(syscall.SIGINT)
	}
	return 0
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

// hostInterrupt turns the first SIGINT, SIGTERM or SIGHUP into a cancellation the filtered cleanup
// can act on — a box's gateway, volumes and receipt are exact-owned, none of it is --rm, so a
// signal that killed coop where it stood would strand three containers — and REMEMBERS which signal
// it was, so teardown can name it instead of inferring it from an exit status. SIGHUP is the
// terminal closing: coop used to die of it with the box still running. A second signal takes the
// default action, so a wedged teardown is still escapable.
type hostInterrupt struct {
	mu  sync.Mutex
	sig os.Signal
}

func newHostInterrupt() (context.Context, *hostInterrupt) {
	ctx, h, _ := listenForInterrupt(context.Background())
	return ctx, h
}

// listenForInterrupt is newHostInterrupt over parent, for a stretch of the launch that ends before
// the process does. Its stop gives the signals their default action back, and after it returns the
// recorder is final: a signal that landed while stop ran is recorded, so a caller reads one it
// would otherwise have lost.
func listenForInterrupt(parent context.Context) (context.Context, *hostInterrupt, func()) {
	ctx, cancel := context.WithCancel(parent)
	h := &hostInterrupt{}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	stopped, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case sig := <-signals:
			h.record(sig)
			signal.Stop(signals)
			cancel()
		case <-stopped:
		}
	}()
	var once sync.Once
	return ctx, h, func() {
		once.Do(func() {
			signal.Stop(signals)
			close(stopped)
			<-finished
			select {
			case sig := <-signals:
				h.record(sig)
			default:
			}
			cancel()
		})
	}
}

// UntilInterrupted runs step under a context the first SIGINT, SIGTERM or SIGHUP cancels, as a
// launch does once it is running, for the work before one that a signal used to kill coop in: the
// first filtered launch on a host qualifies it there, which takes seconds and runs a box of its own.
// It returns the status a shell gives a command that signal ended, 130 for Ctrl-C, or 0 when none
// arrived. The signals have their default action again by then, and one that landed after step
// finished still counts: it was this launch's to stop.
func UntilInterrupted(step func(context.Context) error) (status int, err error) {
	ctx, interrupt, stop := listenForInterrupt(context.Background())
	defer stop()
	err = step(ctx)
	stop()
	return interrupt.exitCode(), err
}

func (h *hostInterrupt) record(sig os.Signal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sig == nil {
		h.sig = sig
	}
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
	case syscall.SIGHUP:
		return "stopped by SIGHUP"
	}
	return "stopped by signal " + h.sig.String()
}

// exitCode is the status a shell gives a command the signal ended, 128 plus its number: 130 for
// Ctrl-C. A nil recorder, or one no signal reached, has none: 0.
func (h *hostInterrupt) exitCode() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if sig, ok := h.sig.(syscall.Signal); ok {
		return 128 + int(sig)
	}
	return 0
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

// servicesWithBox names the services a filtered launch starts just before its box, ahead of the
// box's own section. The services start once the box's gateway runs, so a failure to start them is
// the box's failure. A loop narrates them as they start (servicesPreparing, services).
func (s *launchSections) servicesWithBox(names []string) {
	if !s.on || s.loop || len(names) == 0 {
		return
	}
	s.section("Starting project services")
	for _, name := range names {
		ui.Note("  %s", cleanLaunchText(name))
	}
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
func (s *launchSections) servicesFailed(cause string, available ...ServicePort) {
	if !s.on {
		return
	}
	names := servicePortNames(available)
	if !s.loop {
		ui.Note("")
		if len(names) > 0 {
			ui.Warning("Some project services could not start", cause,
				"Available now: "+strings.Join(names, ", ")+".\nRun coop up to retry.")
		} else {
			ui.Warning("Project services could not start", cause, "Run coop up to retry.")
		}
		return
	}
	s.section("Starting services")
	if len(names) > 0 {
		ui.Note("  %s Some services failed to start%s", ui.Yellow("⚠"), serviceExitSummary(cause))
	} else {
		ui.Note("  %s Services failed to start%s", ui.Yellow("⚠"), serviceExitSummary(cause))
	}
	for _, line := range serviceCauseDetails(cause) {
		if line != "" {
			loopLaunchDetail(line, 4)
		}
	}
	if len(names) > 0 {
		ui.Note("    Continuing with the running services: %s.", strings.Join(names, ", "))
	} else {
		ui.Note("    Continuing without project services.")
	}
	ui.Note("    To retry: coop up")
}

func (s *launchSections) servicesObserved(ports []ServicePort) {
	if !s.on || len(ports) == 0 {
		return
	}
	names := strings.Join(servicePortNames(ports), ", ")
	if !s.loop {
		ui.Note("")
		ui.Note("Project services already running: %s", names)
		return
	}
	s.section("Starting services")
	ui.Note("  %s Running services available: %s", ui.Green("✓"), names)
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
