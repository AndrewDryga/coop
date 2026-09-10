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

// `check` and `explain` answer two deliberately different questions. `check` is
// a hypothetical: can a NEW run in this project reach this — or, with --run,
// could that recorded run have — decided from policy alone, sending no packet
// and resolving no name. `explain` reads a refusal that actually happened, with
// the policy of the day, and never reinterprets it under today's rules.

var netDiagnosticFlags = []string{"--run", "--json", "--protocol", "--port", "--icmp"}

type netDiagnosticOptions struct {
	run, query string
	json       bool
	policy     networkstate.PolicyQuery
}

func parseNetDiagnosticArgs(verb string, args []string) (netDiagnosticOptions, error) {
	var opts netDiagnosticOptions
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch {
		case name == "--run":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return opts, errors.New("--run needs a run — 'coop net runs' shows them")
				}
				i++
				value = args[i]
			}
			if opts.run != "" {
				return opts, fmt.Errorf("coop net %s takes --run once", verb)
			}
			opts.run = value
		case name == "--json":
			if inline {
				return opts, errors.New("--json takes no value")
			}
			opts.json = true
		case (name == "--protocol" || name == "--port") && verb == "check":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return opts, fmt.Errorf("%s needs a value", name)
				}
				i++
				value = args[i]
			}
			if name == "--protocol" {
				if opts.policy.Protocol != "" {
					return opts, errors.New("coop net check takes --protocol once")
				}
				opts.policy.Protocol = value
				continue
			}
			port, err := strconv.Atoi(value)
			if err != nil || opts.policy.Port != 0 {
				return opts, errors.New("--port needs one port number, once")
			}
			opts.policy.Port = port
		case name == "--icmp" && verb == "check":
			if inline {
				return opts, errors.New("--icmp takes no value")
			}
			if opts.policy.Protocol != "" {
				return opts, errors.New("--icmp and --protocol are the same choice; pass one")
			}
			opts.policy.Protocol = "icmp"
		case strings.HasPrefix(args[i], "-"):
			valid := netDiagnosticFlags
			if verb == "explain" {
				valid = valid[:2]
			}
			return opts, unknownErr("net "+verb+" flag", args[i], valid)
		case opts.query != "":
			return opts, fmt.Errorf("coop net %s takes one destination at a time", verb)
		default:
			opts.query = args[i]
		}
	}
	if opts.query == "" {
		if verb == "check" {
			return opts, errors.New("coop net check needs an https:// URL or a host")
		}
		return opts, errors.New("coop net explain needs the host that was blocked — 'coop net inspect' shows them")
	}
	if verb == "check" {
		return netCheckQuery(opts)
	}
	// An exact event id is kept for disambiguation and automation; it names
	// one run's refusal, so it needs that run. The ordinary argument is a host.
	if netEvidenceID(opts.query) {
		if opts.run == "" {
			return opts, errors.New("an event id names one run's refusal — add --run <run>")
		}
		return opts, nil
	}
	host, _, err := netQueryHost(opts.query)
	if err != nil {
		return opts, errors.New("coop net explain needs one host — the name a run tried to reach")
	}
	opts.query = host
	return opts, nil
}

// netCheckQuery decides which hypothetical the user asked for. A URL or name is
// TLS on its port — 443 unless the URL or --port says otherwise; an address
// carries no implied transport, so it requires the operator to say which one
// they mean, and is checked against a recorded run because a run's protected
// ranges are part of the answer.
func netCheckQuery(opts netDiagnosticOptions) (netDiagnosticOptions, error) {
	if address, err := netip.ParseAddr(opts.query); err == nil {
		if opts.policy.Protocol == "" {
			return opts, errors.New("an address needs the transport too: --protocol tcp|udp --port <n>, or --icmp")
		}
		if opts.run == "" {
			return opts, errors.New("an address is checked against a recorded run's rules — add --run <run>")
		}
		opts.policy.Address = address
		return opts, opts.policy.Validate()
	}
	if opts.policy.Protocol != "" && opts.policy.Protocol != "tls" {
		return opts, errors.New("a name is checked as TLS; --protocol tcp|udp and --icmp apply to an IP address")
	}
	host, port, err := netQueryHost(opts.query)
	if err != nil {
		return opts, errors.New("coop net check needs an https:// URL or one exact host — not a wildcard")
	}
	if opts.policy.Port != 0 {
		port = opts.policy.Port
	}
	if port == 0 {
		port = 443
	}
	opts.query, opts.policy = host, networkstate.PolicyQuery{Domain: host, Protocol: "tls", Port: port}
	return opts, opts.policy.Validate()
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
		runID, err := netResolveRun(page, opts.run)
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
	posture, err := box.ProjectNetworkPosture(context.Background(), a.cfg, repo)
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
	answer := netCurrentCheck(posture, opts.policy.Domain, opts.policy.Port, bundles)
	if opts.json {
		return 0, netWriteJSON(os.Stdout, netCheckJSON{Version: networkview.Version, Kind: "current", Host: opts.policy.Domain,
			Port: opts.policy.Port, Protocol: "tls", Allowed: answer.Allowed, Pending: answer.Pending, Agents: answer.Agents,
			Verdict: answer.Verdict, Cause: answer.Cause})
	}
	p := ui.For(os.Stdout)
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetCheck(b, p, answer.Allowed, answer.Verdict, answer.Cause) })
}

type netCheckAnswer struct {
	Allowed, Pending bool
	Verdict, Cause   string
	Agents           []string
}

func netCurrentCheck(posture box.NetworkPosture, host string, port int, bundles map[string]egress.Bundle) netCheckAnswer {
	target := host + ":" + strconv.Itoa(port)
	if posture.Pending != nil {
		// Answering against a policy that cannot launch would be a fiction.
		return netCheckAnswer{Pending: true, Verdict: "No new run can start until this project's network request is approved",
			Cause: "Review it: coop net approve"}
	}
	switch posture.Mode {
	case egress.Open:
		return netCheckAnswer{Allowed: true, Verdict: "New runs can reach " + target, Cause: "This project's network access is unrestricted."}
	case egress.None:
		return netCheckAnswer{Verdict: "New runs cannot reach " + target, Cause: "This project's runs are offline."}
	}
	if posture.Approval != nil && netRulesAllow(posture.Approval.Envelope, host, port) {
		return netCheckAnswer{Allowed: true, Verdict: "New filtered runs can reach " + target, Cause: "Allowed by an approved project rule."}
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
		return netCheckAnswer{Verdict: "No new run can reach " + target,
			Cause: "No approved project rule allows it — add one under box.egress_rules in .agent/project.yaml, then run 'coop net approve'."}
	case 1:
		return netCheckAnswer{Allowed: true, Agents: names, Verdict: names[0] + " can reach " + target,
			Cause: "Provider access for " + names[0] + " is included automatically."}
	}
	return netCheckAnswer{Allowed: true, Agents: names,
		Verdict: strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " can reach " + target,
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

// writeNetCheck is the whole answer: a verdict and its one cause. Nothing is
// appended after them — no disclaimer, no fingerprint, no reason code; the
// command is documented as read-only and --json carries the technical detail.
func writeNetCheck(w io.Writer, p ui.Palette, allowed bool, verdict, cause string) {
	glyph := p.Red("✗")
	if allowed {
		glyph = p.Green("✓")
	}
	fmt.Fprintf(w, "%s %s\n  %s\n", glyph, verdict, cause)
}

// writeNetHistoricalCheck names the run whose frozen rules answered, because
// that is what makes the answer different from a current one.
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
		transport = " with an ICMP echo-request"
	}
	run := "Run " + networkreport.ShortID(result.RunID)
	verdict, cause := run+" could not reach "+destination+transport, netSentence(result.Message)
	if result.Allowed {
		verdict, cause = run+" could reach "+destination+transport, "Allowed by "+netCheckOrigin(result)+"."
	}
	writeNetCheck(w, p, result.Allowed, verdict, cause)
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
// their flag, the repo's request, or a maintained provider bundle.
func netOriginText(origin networkstate.PolicyOrigin) string {
	switch origin.Kind {
	case "operator":
		return "your " + origin.Name + " flag"
	case "project":
		return "an approved project rule"
	case "provider":
		return titleName(origin.Provider) + "'s provider access (bundle " + origin.BundleVersion + ")"
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

// --------------------------------------------------------------- explain ----

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
		runID, err := netResolveRun(page, opts.run)
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
		runID, err := netResolveRun(page, opts.run)
		if err != nil {
			return 1, err
		}
		runs = []string{runID}
	} else {
		repo, err := netProject(a.cfg.RepoOverride)
		if err != nil {
			return 1, errors.New("coop net explain searches this project's runs — run it inside one, or pass --run <run>")
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
	scope := "this project's recorded runs"
	if opts.run != "" {
		scope = "run " + networkreport.ShortID(runs[0])
	}
	ui.Note("nothing blocked %s in %s — 'coop net check %s' says whether a new run can reach it", opts.query, scope, opts.query)
	return 0, nil
}

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

// writeNetExplanation states the blocked host, which run and when, and each
// cause in plain words. When the retained evidence itself proves the hostname,
// transport and port, the smallest rule that would allow it is shown ready to
// paste; DNS-only, protected or thin evidence gets one sentence saying why
// there is nothing to copy — never an invented rule.
func writeNetExplanation(w io.Writer, p ui.Palette, now time.Time, explanation netExplanation) {
	first := explanation.Causes[0].Explanation.Event
	title := explanation.Host
	if first.Port != nil && first.Name != "" {
		title += ":" + strconv.Itoa(*first.Port)
	}
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan(title+" was blocked")))
	fmt.Fprintf(w, "  %s\n", p.Dim("Run "+networkreport.ShortID(explanation.RunID)+" · "+networkreport.When(now, first.At)))
	var candidate *networkview.Candidate
	for _, cause := range explanation.Causes {
		event := cause.Explanation.Event
		line := netSentence(cause.Explanation.Message)
		if len(explanation.Causes) > 1 {
			line = networkreport.RefusalLabel(event)
			if cause.Count > 1 {
				line += " ×" + strconv.Itoa(cause.Count)
			}
			line += " — " + netSentence(cause.Explanation.Message)
		} else if cause.Count > 1 {
			line += " " + p.Dim("(×"+strconv.Itoa(cause.Count)+")")
		}
		fmt.Fprintf(w, "\n%s\n", line)
		if candidate == nil && event.Candidate != nil {
			candidate = event.Candidate
		}
	}
	if explanation.Causes[0].Explanation.DetailTruncated {
		fmt.Fprintf(w, "\n%s\n", p.Dim("This run kept only part of its detail, so other blocked attempts may be missing."))
	}
	if candidate != nil {
		fmt.Fprintf(w, "\nAdd this rule under box.egress_rules in .agent/project.yaml:\n\n%s\nThen run:\n  %s\n", netRuleYAML(candidate.Rule), p.Cyan("coop net approve"))
		return
	}
	fmt.Fprintf(w, "\n%s\n", netNoDraftReason(first.Reason))
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

// netNoDraftReason says why this refusal comes with no rule to copy. The cases
// are different and must not be blurred: a boundary no rule can cross, a
// failure that was not a refusal, and a record too thin to name a transport.
// None is a hint that the destination should be allowed.
func netNoDraftReason(reason string) string {
	switch reason {
	case "protected_destination", "unsafe_dns_answer", "tls_ech_unsupported", "unsupported_capability",
		"tls_name_missing", "tls_name_invalid":
		return "No rule can allow this: it is a protected or unsupported destination, not a gap in the project's rules."
	case "upstream_unreachable", "observation_unavailable":
		return "This was not blocked by a rule — the connection failed on its own, and another rule would not change that."
	default:
		return "There is no rule to copy: a blocked DNS lookup does not say whether the destination is TLS, or on which port. Decide that, then add the rule yourself."
	}
}
