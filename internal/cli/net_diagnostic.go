package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkreport"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

// `check` and `blocked` answer two deliberately different questions. `check` is
// a hypothetical: can a NEW run in this project reach this — or, with --run,
// could that recorded run have — decided from policy alone, sending no packet
// and resolving no name. `blocked` reads a refusal that actually happened, with
// the policy of the day, and never reinterprets it under today's rules.

var netDiagnosticFlags = []string{"--run", "--json", "--protocol", "--port", "--icmp"}

type netDiagnosticOptions struct {
	run, query string
	json       bool
	policy     networkstate.PolicyQuery
}

func parseNetDiagnosticArgs(verb string, args []string) (netDiagnosticOptions, error) {
	var opts netDiagnosticOptions
	command := "coop net " + verb
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch {
		case name == "--run":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return opts, ui.MissingOptionValue("--run", command, command+" "+netDiagnosticExample(verb)+" --run e644f07a")
				}
				i++
				value = args[i]
			}
			if opts.run != "" {
				return opts, ui.RepeatedOption("--run", command)
			}
			opts.run = value
		case name == "--json":
			if inline {
				return opts, ui.InvalidOptionValue(value, "--json", command, "This option takes no value.", command+" "+netDiagnosticExample(verb)+" --json")
			}
			opts.json = true
		case (name == "--protocol" || name == "--port") && verb == "check":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return opts, ui.MissingOptionValue(name, command, "coop net check 10.0.0.1 --run e644f07a --protocol tcp --port 5432")
				}
				i++
				value = args[i]
			}
			if name == "--protocol" {
				if opts.policy.Protocol != "" {
					return opts, ui.RepeatedOption("--protocol", command)
				}
				opts.policy.Protocol = value
				continue
			}
			port, err := strconv.Atoi(value)
			if err != nil || port < 1 || port > 65535 {
				return opts, ui.InvalidOptionValue(value, "--port", command, "Use a port number from 1 to 65535.", "coop net check example.com --port 8443")
			}
			if opts.policy.Port != 0 {
				return opts, ui.RepeatedOption("--port", command)
			}
			opts.policy.Port = port
		case name == "--icmp" && verb == "check":
			if inline {
				return opts, ui.InvalidOptionValue(value, "--icmp", command, "This option takes no value.", "coop net check 10.0.0.1 --run e644f07a --icmp")
			}
			if opts.policy.Protocol != "" {
				return opts, ui.ConflictingOptions("--icmp", "--protocol", command)
			}
			opts.policy.Protocol = "icmp"
		case strings.HasPrefix(args[i], "-"):
			valid := netDiagnosticFlags
			if verb == "blocked" {
				valid = valid[:2]
			}
			return opts, unknownOptionErr(args[i], command, valid)
		case opts.query != "":
			return opts, ui.UnexpectedArgument(args[i], command, command+" "+netDiagnosticUsage(verb))
		default:
			opts.query = args[i]
		}
	}
	if opts.query == "" {
		name := "host"
		if verb == "check" {
			name = "address"
		}
		return opts, ui.MissingArgument(name, command, command+" "+netDiagnosticUsage(verb))
	}
	if verb == "check" {
		return netCheckQuery(opts)
	}
	// An exact event id is kept for disambiguation and automation; it names
	// one run's refusal, so it needs that run. The ordinary argument is a host.
	if netEvidenceID(opts.query) {
		if opts.run == "" {
			return opts, &ui.UsageError{
				Headline: "An event ID names one run's blocked attempt",
				Cause:    "Add the run it was recorded in.",
				Rows:     [][2]string{{"Usage:", "coop net blocked <event> --run <run>"}, {"Help:", ui.HelpCommand(command)}},
			}
		}
		return opts, nil
	}
	host, _, err := netQueryHost(opts.query)
	if err != nil {
		return opts, netInvalidAddress(command)
	}
	opts.query = host
	return opts, nil
}

// netDiagnosticUsage and netDiagnosticExample are each verb's own syntax, so a
// refusal shows the command the reader typed rather than a family template.
func netDiagnosticUsage(verb string) string {
	if verb == "check" {
		return "<url-or-host> [<options>]"
	}
	return "<host> [--run <run>] [--json]"
}

func netDiagnosticExample(verb string) string {
	if verb == "check" {
		return "example.com"
	}
	return "registry.npmjs.org"
}

// netInvalidAddress refuses something that is not a destination coop can reason
// about: a wildcard, a path, a scheme a filtered run cannot use.
func netInvalidAddress(command string) error {
	return &ui.UsageError{
		Headline: fmt.Sprintf("Invalid network address for %q", command),
		Cause:    "Use an HTTPS URL or an exact hostname, such as example.com.",
		Rows:     [][2]string{{"Help:", ui.HelpCommand(command)}},
	}
}

// netCheckQuery decides which hypothetical the user asked for. A URL or name is
// TLS on its port — 443 unless the URL or --port says otherwise; an address
// carries no implied transport, so it requires the operator to say which one
// they mean, and is checked against a recorded run because a run's protected
// ranges are part of the answer.
func netCheckQuery(opts netDiagnosticOptions) (netDiagnosticOptions, error) {
	if address, err := netip.ParseAddr(opts.query); err == nil {
		if opts.policy.Protocol == "" || opts.run == "" {
			return opts, &ui.UsageError{
				Headline: "An IP address needs a run and connection type",
				Rows: [][2]string{
					{"Usage:", "coop net check <ip> --run <run> --protocol <tcp|udp> --port <n>"},
					{ui.Continuation, "coop net check <ip> --run <run> --icmp"},
					{"Help:", "coop help net check"},
				},
			}
		}
		opts.policy.Address = address
		if err := opts.policy.Validate(); err != nil {
			return opts, netCheckValueErr()
		}
		return opts, nil
	}
	if opts.policy.Protocol != "" && opts.policy.Protocol != "tls" {
		return opts, &ui.UsageError{
			Headline: fmt.Sprintf("Cannot check %q as %s", opts.query, strings.ToUpper(opts.policy.Protocol)),
			Cause:    "A hostname is checked as TLS; --protocol tcp|udp and --icmp apply to an IP address.",
			Rows:     [][2]string{{"Example:", "coop net check " + opts.query}, {"Help:", "coop help net check"}},
		}
	}
	host, port, err := netQueryHost(opts.query)
	if err != nil {
		return opts, netInvalidAddress("coop net check")
	}
	if opts.policy.Port != 0 {
		port = opts.policy.Port
	}
	if port == 0 {
		port = 443
	}
	opts.query, opts.policy = host, networkstate.PolicyQuery{Domain: host, Protocol: "tls", Port: port}
	if err := opts.policy.Validate(); err != nil {
		return opts, netCheckValueErr()
	}
	return opts, nil
}

// netCheckValueErr refuses a query the policy validator will not evaluate — an
// address family it cannot check, a transport that does not go with it. The
// cause states the whole accepted shape, because which half was wrong is
// exactly what the reader is missing.
func netCheckValueErr() error {
	return &ui.UsageError{
		Headline: "Invalid network address for \"coop net check\"",
		Cause:    "Check a hostname as TLS, or an IPv4 address with --protocol tcp|udp --port <n>, or --icmp.",
		Rows:     [][2]string{{"Help:", "coop help net check"}},
	}
}

// netQueryHost reduces what a person pastes — an https:// URL, a host:port, a
// bare name — to the normalized hostname and the port it names (0 when it
// names none). Only https is a filtered destination: a box reaches TLS.
func netQueryHost(value string) (string, int, error) {
	host, port := value, 0
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
			return "", 0, errors.New("only https:// URLs name a destination a filtered run can reach")
		}
		host = parsed.Hostname()
		if text := parsed.Port(); text != "" {
			port, _ = strconv.Atoi(text)
		}
	} else if h, p, ok := networkreport.SplitPeer(value); ok {
		host = h
		port, _ = strconv.Atoi(p)
	}
	name, err := egress.NormalizeDomain(host, false)
	if err != nil {
		return "", 0, err
	}
	return name, port, nil
}

func netEvidenceID(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}

func (a *app) netDiagnostic(verb string, args []string) (int, error) {
	opts, err := parseNetDiagnosticArgs(verb, args)
	if err != nil {
		return 2, err
	}
	if verb == "check" && opts.run == "" {
		return a.netCheckCurrent(opts)
	}
	evidence, err := openNetRunEvidence()
	if err != nil {
		return 1, err
	}
	defer evidence.Close()
	page, err := netExecutionsOf(evidence)
	if err != nil {
		return 1, err
	}
	p := ui.For(os.Stdout)
	if verb == "check" {
		runID, err := netResolveRun(page, opts.run, "coop net check")
		if err != nil {
			return 1, err
		}
		// The local operator view: this is the host, not an outbound export, so
		// the name the caller just typed is not withheld from them.
		result, err := evidence.Why(runID, opts.policy, true)
		if err != nil {
			return 1, netRunErr(runID, err)
		}
		if opts.json {
			return 0, netWriteJSON(os.Stdout, result)
		}
		return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetHistoricalCheck(b, p, result) })
	}
	return a.netExplain(evidence, page, opts)
}

// ----------------------------------------------------------------- check ----

// netCheckJSON is the machine form of a current-policy answer. It keeps the
// full identity of what was decided and how, so a script never parses prose.
type netCheckJSON struct {
	Version  int      `json:"version"`
	Kind     string   `json:"kind"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Protocol string   `json:"protocol"`
	Allowed  bool     `json:"allowed_by_policy"`
	Pending  bool     `json:"approval_pending"`
	Agents   []string `json:"agents,omitempty"`
	Verdict  string   `json:"verdict"`
	Cause    string   `json:"cause"`
}

// netCheckCurrent answers for a normal new run in this project: the approved
// project rules first, then the provider access each agent brings with it.
// Operator flags and MCP servers are per-launch inputs and are not assumed.
func (a *app) netCheckCurrent(opts netDiagnosticOptions) (int, error) {
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return 1, errors.New("coop net check answers for a project — run it inside one, or pass --run <run> for a recorded run")
	}
	access, err := box.ProjectNetworkAccess(context.Background(), a.cfg, repo)
	if err != nil {
		return 1, err
	}
	bundles := map[string]egress.Bundle{}
	for _, name := range agents.Names() {
		agent, ok := agents.Get(name)
		if !ok {
			continue
		}
		// An agent without a qualified provider bundle brings no access; it
		// simply does not answer here.
		if bundle, err := agent.NetworkBundle(agents.NetworkBundleInput{Client: egress.ClientCLI}); err == nil {
			bundles[name] = bundle
		}
	}
	answer := netCurrentCheck(access, opts.policy.Domain, opts.policy.Port, bundles)
	if opts.json {
		return 0, netWriteJSON(os.Stdout, netCheckJSON{Version: networkview.Version, Kind: "current", Host: opts.policy.Domain,
			Port: opts.policy.Port, Protocol: "tls", Allowed: answer.Allowed, Pending: answer.Pending, Agents: answer.Agents,
			Verdict: answer.Verdict, Cause: answer.Cause})
	}
	p := ui.For(os.Stdout)
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetCheck(b, p, answer) })
}

type netCheckAnswer struct {
	Allowed, Pending bool
	Verdict, Cause   string
	Agents           []string
	// Rule is the draft that would allow this destination, set only when nothing allows it today.
	// It is a rule to review, never a grant: `coop net approve` is what turns one into authority.
	Rule egress.Rule
}

func netCurrentCheck(access box.NetworkAccess, host string, port int, bundles map[string]egress.Bundle) netCheckAnswer {
	target := host + ":" + strconv.Itoa(port)
	if access.Pending != nil {
		// Answering against a policy that cannot launch would be a fiction.
		return netCheckAnswer{Pending: true, Verdict: netPendingHeadline,
			Cause: "Review the requested changes: coop net approve"}
	}
	switch access.Mode {
	case egress.Open:
		return netCheckAnswer{Allowed: true, Verdict: target + " is allowed — internet access is unrestricted."}
	case egress.None:
		return netCheckAnswer{Verdict: target + " is blocked — internet access is disabled."}
	}
	if access.Approval != nil && netRulesAllow(access.Approval.Envelope, host, port) {
		return netCheckAnswer{Allowed: true, Verdict: target + " is allowed by an approved project rule."}
	}
	var names []string
	for name, bundle := range bundles {
		if netRulesAllow(bundle.Core, host, port) {
			names = append(names, titleName(name))
		}
	}
	slices.Sort(names)
	switch len(names) {
	case 0:
		// Telling someone to "add a rule" without the rule leaves them to derive the YAML from the
		// page. `coop net blocked` prints the smallest rule that would have allowed the destination;
		// a check for the same destination knows exactly as much, so it prints the same draft.
		return netCheckAnswer{Verdict: target + " is blocked — no approved rule allows it.",
			Rule: egress.Rule{To: egress.Destination{Domain: host}, Protocol: "tls", Ports: []int{port}}}
	case 1:
		return netCheckAnswer{Allowed: true, Agents: names, Verdict: target + " is allowed by " + names[0] + "'s provider access."}
	}
	return netCheckAnswer{Allowed: true, Agents: names,
		Verdict: target + " is allowed for " + ui.List(names, "and") + ".",
		Cause:   "Their provider access is included automatically."}
}

// netRulesAllow is the domain match the policy compiler performs, on the rules
// a person can see: exact or wildcard TLS names on the ports the rule names.
func netRulesAllow(rules []egress.Rule, host string, port int) bool {
	for _, rule := range rules {
		if rule.To.Domain != "" && rule.Protocol == "tls" && egress.MatchesDomain(rule.To.Domain, host) && slices.Contains(rule.Ports, port) {
			return true
		}
	}
	return false
}

// writeNetCheck is the whole answer: one sentence, and a second line only where
// the design gives one — the action for a destination no rule allows, or the
// shared note that several providers bring their own access. Nothing else is
// appended: no disclaimer, no fingerprint, no reason code; the command is
// documented as read-only and --json carries the technical detail.
func writeNetCheck(w io.Writer, p ui.Palette, answer netCheckAnswer) {
	glyph := p.Red("✗")
	if answer.Allowed {
		glyph = p.Green("✓")
	}
	fmt.Fprintf(w, "%s %s\n", glyph, answer.Verdict)
	if answer.Cause != "" {
		fmt.Fprintf(w, "  %s\n", answer.Cause)
	}
	if rule := netRuleYAML(answer.Rule); rule != "" {
		fmt.Fprintf(w, "\nAdd this rule under box.egress_rules in .agent/project.yaml:\n\n%s\nThen run:\n  %s\n", rule, p.Cyan("coop net approve"))
	}
}

// writeNetHistoricalCheck names the run whose frozen rules answered, because
// that is what makes the answer different from a current one. It is the same
// one sentence: what the destination was, what decided it, and which run.
func writeNetHistoricalCheck(w io.Writer, p ui.Palette, result networkstate.PolicyExplanation) {
	destination := "the withheld destination"
	switch {
	case !result.Withheld && result.Domain != "":
		destination = result.Domain
	case !result.Withheld && result.Peer != "":
		destination = result.Peer
	}
	transport := ""
	switch {
	case result.Port != 0:
		destination += ":" + strconv.Itoa(result.Port)
		if result.Protocol != "tls" {
			transport = " over " + strings.ToUpper(result.Protocol)
		}
	case result.Protocol == "icmp":
		transport = " with an ICMP echo request"
	}
	run := " in run " + networkreport.ShortID(result.RunID) + "."
	subject := destination + transport
	if result.Allowed {
		writeNetCheck(w, p, netCheckAnswer{Allowed: true, Verdict: subject + " was allowed by " + netCheckOrigin(result) + run})
		return
	}
	// A refusal with a compact name for its boundary says it in the verdict; any
	// other reason keeps its own retained sentence on the line beneath.
	if clause := netBlockedClause(result.Reason); clause != "" {
		writeNetCheck(w, p, netCheckAnswer{Verdict: subject + " was blocked as " + clause + run})
		return
	}
	writeNetCheck(w, p, netCheckAnswer{Verdict: subject + " was blocked" + run, Cause: netSentence(result.Message)})
}

// netBlockedClause names the boundary that refused a recorded attempt as a noun
// phrase, for the one-sentence verdict. Only reasons with an exact short name
// are here; everything else keeps its full retained sentence, which is the
// point — a guessed clause would be a worse answer than the reason itself.
func netBlockedClause(reason string) string {
	switch reason {
	case "protected_destination", "unsafe_dns_answer":
		return "a protected address"
	case "unapproved_name":
		return "an unapproved destination"
	case "protocol_not_allowed":
		return "an unapproved connection type"
	case "port_not_allowed":
		return "an unapproved port"
	}
	return ""
}

// netCheckOrigin says where the grant came from in the operator's own words —
// their flag, the project's approved request, or a provider's bundle.
func netCheckOrigin(result networkstate.PolicyExplanation) string {
	for _, origin := range result.Origins {
		return netOriginText(origin)
	}
	if result.Rule != nil {
		return "a rule that run started with"
	}
	return "that run's rules"
}

// netOriginText says where a grant came from in the operator's own vocabulary —
// their flag, the repo's request, or a maintained provider bundle. A bundle
// version is technical detail: it belongs in --json, not in the sentence.
func netOriginText(origin networkstate.PolicyOrigin) string {
	switch origin.Kind {
	case "operator":
		return "your " + origin.Name + " flag"
	case "project":
		return "an approved project rule"
	case "provider":
		return titleName(origin.Provider) + "'s provider access"
	case "mcp":
		return "the MCP server " + origin.Name + " coop set up for that box"
	default:
		return origin.Kind
	}
}

func netSentence(text string) string {
	if text == "" {
		return text
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.HasSuffix(text, ".") {
		text += "."
	}
	return text
}

// --------------------------------------------------------------- blocked ----

// netExplanation is one blocked host as a person asks about it: the run it was
// found in, and each materially different reason it was blocked there, with
// how often each repeated.
type netExplanation struct {
	Version int               `json:"version"`
	RunID   string            `json:"run_id"`
	Host    string            `json:"host"`
	Causes  []netExplainCause `json:"causes"`
}

type netExplainCause struct {
	Count       int                           `json:"count"`
	Explanation networkstate.EventExplanation `json:"explanation"`
}

func (a *app) netExplain(evidence *networkstate.Evidence, page networkstate.ExecutionPage, opts netDiagnosticOptions) (int, error) {
	p := ui.For(os.Stdout)
	if netEvidenceID(opts.query) {
		runID, err := netResolveRun(page, opts.run, "coop net blocked")
		if err != nil {
			return 1, err
		}
		result, err := evidence.Explain(runID, opts.query, true)
		if err != nil {
			if errors.Is(err, networkstate.ErrEventNotRetained) {
				return 1, err // the message already names the run and the way to list what is kept
			}
			return 1, netRunErr(runID, err)
		}
		explanation := netExplanation{Version: networkview.Version, RunID: runID, Host: networkreport.Destination(result.Event.Name, result.Event.Peer, result.Event.DestinationID),
			Causes: []netExplainCause{{Count: 1, Explanation: result}}}
		if opts.json {
			return 0, netWriteJSON(os.Stdout, explanation)
		}
		return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetExplanation(b, p, time.Now(), explanation) })
	}
	// One run when named; otherwise this project's runs newest first, so the
	// answer is the most recent time this host was blocked here — never a
	// refusal from some other project's box.
	var runs []string
	if opts.run != "" {
		runID, err := netResolveRun(page, opts.run, "coop net blocked")
		if err != nil {
			return 1, err
		}
		runs = []string{runID}
	} else {
		repo, err := netProject(a.cfg.RepoOverride)
		if err != nil {
			return 1, &ui.UsageError{
				Headline: "Could not select a network run",
				Cause:    "Run this inside a project, or name the run it was blocked in.",
				Rows:     [][2]string{{"Usage:", "coop net blocked <host> --run <run>"}, {"Help:", "coop help net blocked"}},
			}
		}
		for _, run := range netFilterProject(page.Executions, netResolvedPath(repo)) {
			runs = append(runs, run.ID)
		}
	}
	for _, runID := range runs {
		explanation, err := netExplainHost(evidence, runID, opts.query)
		if err != nil {
			return 1, netRunErr(runID, err)
		}
		if len(explanation.Causes) == 0 {
			continue
		}
		if opts.json {
			return 0, netWriteJSON(os.Stdout, explanation)
		}
		return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetExplanation(b, p, time.Now(), explanation) })
	}
	if opts.json {
		return 0, netWriteJSON(os.Stdout, netExplanation{Version: networkview.Version, Host: opts.query, Causes: []netExplainCause{}})
	}
	// Absence is stated only over a complete search: a partial read means the
	// block may simply be in a record coop could not open.
	scope := "in this project's runs"
	if opts.run != "" {
		scope = "in run " + networkreport.ShortID(runs[0])
	}
	complete := !page.Incomplete && page.Unreadable == 0
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetNoBlock(b, p, opts.query, scope, complete) })
}

// writeNetNoBlock answers "nothing blocked this here" — and only says it as an
// absence when the search was complete.
func writeNetNoBlock(w io.Writer, p ui.Palette, host, scope string, complete bool) {
	if !complete {
		fmt.Fprintf(w, "%s %s\n\n", p.Yellow("⚠"), p.Yellow(netIncompleteRecording))
	}
	fmt.Fprintf(w, "No recorded block for %s %s.\n", host, scope)
	fmt.Fprintf(w, "\nCheck current access\n  %s\n", p.Cyan("coop net check "+host))
}

// netIncompleteRecording is the one warning that goes before any next action:
// what follows is a lower bound, not the whole story.
const netIncompleteRecording = "Recording incomplete — some blocked attempts may be missing"

// netExplainHost reads one run for every refusal of one host and groups the
// repeats by what actually differed — the boundary, the reason, the port — so
// a name refused forty times reads as one cause, and two different refusals
// of it read as two.
func netExplainHost(evidence *networkstate.Evidence, runID, host string) (netExplanation, error) {
	inspection, err := evidence.Inspect(runID, time.Now(), true)
	if err != nil {
		return netExplanation{}, err
	}
	out := netExplanation{Version: networkview.Version, RunID: runID, Host: host}
	type group struct {
		newest networkview.Denial
		count  int
	}
	index := map[string]*group{}
	var order []string
	for _, denial := range inspection.Observed.Denials {
		if denial.Name != host {
			continue
		}
		key := networkreport.RefusalLabel(denial)
		if index[key] == nil {
			index[key] = &group{newest: denial}
			order = append(order, key)
		}
		index[key].count++
		if denial.At.After(index[key].newest.At) {
			index[key].newest = denial
		}
	}
	for _, key := range order {
		result, err := evidence.Explain(runID, index[key].newest.ID, true)
		if err != nil {
			return netExplanation{}, err
		}
		out.Causes = append(out.Causes, netExplainCause{Count: index[key].count, Explanation: result})
	}
	return out, nil
}

// writeNetExplanation states the blocked destination, which run and when, and
// each cause in plain words. When the retained evidence itself proves the
// hostname, transport and port, the smallest rule that would allow it is shown
// ready to paste; DNS-only, protected or thin evidence gets no rule at all —
// never an invented one — and incomplete recording is said before any action.
func writeNetExplanation(w io.Writer, p ui.Palette, now time.Time, explanation netExplanation) {
	first := explanation.Causes[0].Explanation.Event
	title := explanation.Host
	if first.Port != nil && first.Name != "" {
		title += ":" + strconv.Itoa(*first.Port)
	}
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan(title+" "+netBlockedVerb(first.Reason))))
	fmt.Fprintf(w, "  %s\n", p.Dim("Run "+networkreport.ShortID(explanation.RunID)+" · "+networkreport.WhenAt(now, first.At)))
	var candidate *networkview.Candidate
	if len(explanation.Causes) == 1 {
		cause := explanation.Causes[0]
		line := netBlockedCause(cause.Explanation, false)
		if cause.Count > 1 {
			line += " It happened " + strconv.Itoa(cause.Count) + " times."
		}
		fmt.Fprintf(w, "\n%s\n", line)
		candidate = cause.Explanation.Event.Candidate
	} else {
		// Two different refusals of one name are two facts: each says what was
		// tried, how often, and why, under its own row.
		fmt.Fprintln(w)
		for _, cause := range explanation.Causes {
			event := cause.Explanation.Event
			fmt.Fprintf(w, "  %s\n    %s\n", netCauseLabel(event, cause.Count), netBlockedCause(cause.Explanation, true))
			if candidate == nil && event.Candidate != nil {
				candidate = event.Candidate
			}
		}
	}
	// Incomplete evidence is a warning about everything above AND a reason not
	// to print a rule: a record that lost detail cannot prove the smallest one.
	if explanation.Causes[0].Explanation.DetailTruncated {
		fmt.Fprintf(w, "\n%s %s\n", p.Yellow("⚠"), p.Yellow(netIncompleteRecording))
		return
	}
	if candidate != nil {
		fmt.Fprintf(w, "\nAdd this rule under box.egress_rules in .agent/project.yaml:\n\n%s\nThen run:\n  %s\n", netRuleYAML(candidate.Rule), p.Cyan("coop net approve"))
		return
	}
	if reason := netNoDraftReason(first); reason != "" {
		fmt.Fprintf(w, "\n%s\n", reason)
	}
}

// netBlockedCause is the sentence one refusal reads as. The retained reason
// mapping in internal/networkstate answers for every code; the cases here are
// the ones this view says more precisely, because it knows what was tried —
// a DNS lookup and a TLS connection refused for the same reason are not the
// same sentence, and under a cause row the kind is already in the row above.
// grouped selects that shorter form.
func netBlockedCause(explanation networkstate.EventExplanation, grouped bool) string {
	event := explanation.Event
	switch event.Reason {
	case "unapproved_name":
		what := "connection"
		if netCauseKind(event.Kind) == "dns" {
			what = "lookup"
		}
		if grouped {
			return "No approved rule allowed the " + what + "."
		}
		if what == "lookup" {
			return "No approved rule allowed this DNS lookup."
		}
		return "No approved rule allowed this TLS connection."
	case "protected_destination", "unsafe_dns_answer":
		return "This address is protected. Project rules cannot allow it."
	case "tls_ech_unsupported":
		return "This connection uses encrypted TLS names, which filtered networking does not support."
	case "upstream_unreachable":
		return "The upstream connection failed."
	case "observation_unavailable":
		return "Coop could not record enough information to explain this attempt."
	}
	return netSentence(explanation.Message)
}

// netBlockedVerb keeps three different things apart in the title: a policy
// refusal, an attempt that never reached its destination, and an attempt coop
// could not record enough about to explain.
func netBlockedVerb(reason string) string {
	switch reason {
	case "observation_unavailable", "unknown_reason":
		return "could not be checked"
	case "tls_ech_unsupported", "tls_malformed", "tls_hello_too_large", "tls_inspection_timeout",
		"tls_inspection_unavailable", "upstream_unreachable", "unsupported_capability",
		"gateway_connection_capacity", "gateway_lease_capacity", "gateway_lease_refused", "gateway_unavailable",
		"enforcement_unavailable", "clock_unavailable", "dns_unavailable", "dns_no_address",
		"dns_ttl_expired", "dns_capacity_exceeded", "dns_upstream_invalid":
		return "could not be reached"
	}
	return "was blocked"
}

// netCauseLabel is one cause's own row: what was tried, on which port when the
// evidence recorded one, and how many attempts it groups.
func netCauseLabel(event networkview.Denial, count int) string {
	label := networkreport.TransportLabel(netCauseKind(event.Kind))
	if event.Port != nil {
		label += " on port " + strconv.Itoa(*event.Port)
	}
	return label + " · " + ui.Count(count, "attempt")
}

func netCauseKind(kind string) string {
	switch kind {
	case "dns_denied":
		return "dns"
	case "tls_denied":
		return "tls"
	case "direct_tcp_attempt":
		return "direct connection"
	case "admission_failed":
		return "admission"
	}
	return kind
}

// netRuleYAML is the smallest valid `egress_rules` list item for one rule, in
// the shape a person pastes under the named key. It is a draft to review, never
// a grant: only `coop net approve` turns it into authority.
func netRuleYAML(rule egress.Rule) string {
	var b strings.Builder
	switch {
	case rule.To.Domain != "":
		fmt.Fprintf(&b, "  - to: {domain: %q}\n", rule.To.Domain)
	case rule.To.IP != "":
		fmt.Fprintf(&b, "  - to: {ip: %q}\n", rule.To.IP)
	case rule.To.CIDR != "":
		fmt.Fprintf(&b, "  - to: {cidr: %q}\n", rule.To.CIDR)
	case rule.To.Service != "":
		fmt.Fprintf(&b, "  - to: {service: %q}\n", rule.To.Service)
	}
	if rule.Protocol != "" {
		fmt.Fprintf(&b, "    protocol: %s\n", rule.Protocol)
	}
	if len(rule.Ports) != 0 {
		fmt.Fprintf(&b, "    ports: [%s]\n", strings.ReplaceAll(netPortList(rule.Ports), ",", ", "))
	}
	if len(rule.Types) != 0 {
		fmt.Fprintf(&b, "    types: [%s]\n", strings.Join(rule.Types, ", "))
	}
	return b.String()
}

// netNoDraftReason says why this refusal comes with no rule to copy, and says
// it ONLY where a rule is the kind of thing that could have helped. A protected
// address, an unsupported capability or a connection that failed on its own
// already said so in its cause; repeating it as rule advice would imply the
// destination should be allowed. What is left is the honest case: the record
// names no transport or port, so there is nothing exact to write down.
func netNoDraftReason(event networkview.Denial) string {
	if netBlockedVerb(event.Reason) != "was blocked" || event.Port != nil {
		return ""
	}
	switch event.Reason {
	case "protected_destination", "unsafe_dns_answer", "tls_name_missing", "tls_name_invalid":
		return ""
	}
	return "The record has no connection type or port to use in a rule."
}
