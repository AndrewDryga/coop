package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

// `why` and `explain` answer two deliberately different questions. `why` is a
// hypothetical check against a run's CAPTURED policy — it sends no packet and
// resolves no name. `explain` reads one decision that actually happened, with
// the policy of the day, and never reinterprets it under today's rules.

var netDiagnosticFlags = []string{"--run", "--json"}

type netDiagnosticOptions struct {
	run, query string
	json       bool
}

func parseNetDiagnosticArgs(verb string, args []string) (netDiagnosticOptions, error) {
	var opts netDiagnosticOptions
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch {
		case name == "--run":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return opts, errors.New("--run needs the run id shown by 'coop net ls'")
				}
				i++
				value = args[i]
			}
			if opts.run != "" {
				return opts, fmt.Errorf("net %s accepts --run once", verb)
			}
			opts.run = value
		case name == "--json":
			if inline {
				return opts, errors.New("--json takes no value")
			}
			opts.json = true
		case strings.HasPrefix(args[i], "-"):
			return opts, unknownErr("net "+verb+" flag", args[i], netDiagnosticFlags)
		case opts.query != "":
			return opts, fmt.Errorf("net %s accepts one query — see 'coop net --help'", verb)
		default:
			opts.query = args[i]
		}
	}
	if opts.query == "" || opts.run == "" {
		return opts, fmt.Errorf("net %s needs one query and --run <id> (the run id from 'coop net ls')", verb)
	}
	if verb == "why" {
		name, err := egress.NormalizeDomain(opts.query, false)
		if err != nil {
			return opts, errors.New("net why needs one exact ASCII domain — not a URL, an IP, a wildcard or a port")
		}
		opts.query = name
	} else if !netEvidenceID(opts.query) {
		return opts, errors.New("net explain needs an evidence id from that run's refused decisions ('coop net inspect <run>' lists them)")
	}
	return opts, nil
}

func netEvidenceID(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}

func netDiagnostic(verb string, args []string) (int, error) {
	opts, err := parseNetDiagnosticArgs(verb, args)
	if err != nil {
		return 2, err
	}
	evidence, err := openNetRunEvidence()
	if err != nil {
		return 1, err
	}
	defer evidence.Close()
	p := ui.For(os.Stdout)
	if verb == "why" {
		// The local operator view: this is the host, not an outbound export, so
		// the name the caller just typed is not withheld from them.
		result, err := evidence.Why(opts.run, opts.query, true)
		if err != nil {
			return 1, netRunErr(opts.run, err)
		}
		if opts.json {
			return 0, netWriteJSON(os.Stdout, result)
		}
		return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetWhy(b, p, result) })
	}
	result, err := evidence.Explain(opts.run, opts.query, true)
	if err != nil {
		if errors.Is(err, networkstate.ErrEventNotRetained) {
			return 1, fmt.Errorf("network run %q: %w", opts.run, err)
		}
		return 1, netRunErr(opts.run, err)
	}
	if opts.json {
		return 0, netWriteJSON(os.Stdout, result)
	}
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetExplanation(b, p, result) })
}

func writeNetWhy(w io.Writer, p ui.Palette, result networkstate.PolicyExplanation) {
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("policy check (hypothetical)")))
	field := func(label, value string) { netField(w, p, netLabelWidth, label, value) }
	field("Run", result.RunID)
	destination := "withheld"
	if !result.Withheld {
		destination = result.Domain
	}
	field("Destination", fmt.Sprintf("%s %s/%d", destination, result.Protocol, result.Port))
	verdict := p.Red("no — outside the captured policy")
	if result.Allowed {
		verdict = p.Green("yes — the captured policy permits it")
	}
	field("Allowed", verdict+" ("+result.Reason+")")
	field("Meaning", result.Message)
	field("Policy", result.PolicyFingerprint+" (mode "+string(result.Mode)+")")
	if result.Rule != nil {
		field("Matched", result.Rule.To.Domain+" "+result.Rule.Protocol+"/"+netPortList(result.Rule.Ports))
	}
	for _, origin := range result.Origins {
		netRow(w, netOriginText(origin))
	}
	fmt.Fprintln(w, p.Dim("No DNS query, probe or connection was made, and current policy was not evaluated."))
}

func writeNetExplanation(w io.Writer, p ui.Palette, result networkstate.EventExplanation) {
	fmt.Fprintf(w, "%s\n", p.Bold(p.Cyan("refused (observed)")))
	field := func(label, value string) { netField(w, p, netLabelWidth, label, value) }
	event := result.Event
	field("Run", result.RunID+" (gateway epoch "+result.Epoch+")")
	field("Event", event.ID)
	field("When", event.At.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	field("Destination", netDestination(event.Name, event.Peer, event.DestinationID))
	if event.Port != nil {
		field("Port", strconv.Itoa(*event.Port))
	}
	field("Seen by", event.Source+" ("+event.Kind+", basis "+event.Basis+")")
	field("Reason", event.Reason)
	field("Meaning", result.Message)
	field("Policy", result.PolicyFingerprint)
	if result.DetailTruncated {
		fmt.Fprintln(w, p.Dim("This run's retained detail was truncated; other decisions may not have been kept."))
	}
	if candidate := event.Candidate; candidate != nil {
		fmt.Fprintf(w, "\n%s\n", p.Bold("A human could add this rule — it is a draft, not a grant:"))
		fmt.Fprintf(w, "\n  box:\n    egress_rules:\n%s\n", box.NetworkRuleYAML(candidate.Rule))
		fmt.Fprintln(w, "Put it in .agent/project.yaml, then run 'coop net approve'. It applies to NEW runs;")
		fmt.Fprintln(w, "this run and any other running box keep the policy they were launched with.")
		fmt.Fprintln(w, p.Dim("Traffic is evidence that something was attempted, not that the access is needed."))
	} else {
		fmt.Fprintln(w, p.Dim(netNoDraftReason(event.Reason)))
	}
	fmt.Fprintln(w, p.Dim("This is what was recorded then; today's policy was not substituted for it."))
}

// netNoDraftReason says why this refusal produced no draft rule. The two cases
// are different and must not be blurred: an immutable boundary no rule can
// cross, versus evidence too thin to name a transport. Neither is a hint that
// the destination should be allowed.
func netNoDraftReason(reason string) string {
	switch reason {
	case "protected_destination", "unsafe_dns_answer", "tls_ech_unsupported", "unsupported_capability",
		"tls_name_missing", "tls_name_invalid":
		return "No rule can grant this — it is a protected or unsupported destination, not a gap in your policy."
	case "upstream_unreachable", "observation_unavailable":
		return "This is an availability failure, not a policy denial. Another allow rule would not fix it."
	default:
		return "No rule is drafted: this evidence does not establish a transport or port. A DNS refusal alone\n" +
			"cannot prove the destination is TLS on 443 — decide that yourself before adding a rule."
	}
}

func netPortList(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		out = append(out, strconv.Itoa(port))
	}
	return strings.Join(out, ",")
}

// netOriginText says where a grant came from in the operator's own vocabulary —
// their flag, the repo's request, or a maintained provider bundle.
func netOriginText(origin networkstate.PolicyOrigin) string {
	switch origin.Kind {
	case "operator":
		return "granted by you (" + origin.Name + ")"
	case "project":
		return "requested by the project and approved"
	case "provider":
		return "core endpoint for " + origin.Provider + " " + string(origin.Client) + " (bundle " + origin.BundleVersion + ")"
	case "mcp":
		return "MCP server " + origin.Name + " coop configured for this box"
	default:
		return origin.Kind
	}
}
