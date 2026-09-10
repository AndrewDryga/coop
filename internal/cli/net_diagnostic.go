package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
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
					return opts, errors.New("--run needs the run id 'coop net ls' shows")
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
		case name == "--protocol", name == "--port":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return opts, fmt.Errorf("%s needs a value", name)
				}
				i++
				value = args[i]
			}
			if name == "--protocol" {
				if opts.policy.Protocol != "" {
					return opts, errors.New("coop net why takes --protocol once")
				}
				opts.policy.Protocol = value
				continue
			}
			port, err := strconv.Atoi(value)
			if err != nil || opts.policy.Port != 0 {
				return opts, errors.New("--port needs one port number, once")
			}
			opts.policy.Port = port
		case name == "--icmp":
			if inline {
				return opts, errors.New("--icmp takes no value")
			}
			if opts.policy.Protocol != "" {
				return opts, errors.New("--icmp and --protocol are the same choice; pass one")
			}
			opts.policy.Protocol = "icmp"
		case strings.HasPrefix(args[i], "-"):
			return opts, unknownErr("net "+verb+" flag", args[i], netDiagnosticFlags)
		case opts.query != "":
			return opts, fmt.Errorf("coop net %s takes one destination at a time", verb)
		default:
			opts.query = args[i]
		}
	}
	if opts.query == "" || opts.run == "" {
		return opts, fmt.Errorf("coop net %s needs a destination and --run <id> — list the runs with 'coop net ls'", verb)
	}
	if verb == "why" {
		return netPolicyQuery(opts)
	} else if !netEvidenceID(opts.query) {
		return opts, errors.New("coop net explain needs an event id from that run's refusals — 'coop net inspect <run>' lists them")
	}
	return opts, nil
}

// netPolicyQuery decides which hypothetical the user asked for. A domain is
// TLS — on 443 unless --port names another granted port; an address carries no
// implied transport, so it requires the operator to say which one they mean.
func netPolicyQuery(opts netDiagnosticOptions) (netDiagnosticOptions, error) {
	if address, err := netip.ParseAddr(opts.query); err == nil {
		if opts.policy.Protocol == "" {
			return opts, errors.New("an address needs the transport too: --protocol tcp|udp --port <n>, or --icmp")
		}
		opts.policy.Address = address
		return opts, opts.policy.Validate()
	}
	if opts.policy.Protocol != "" && opts.policy.Protocol != "tls" {
		return opts, errors.New("a domain is checked as TLS; --protocol tcp|udp and --icmp apply to an IP address")
	}
	name, err := egress.NormalizeDomain(opts.query, false)
	if err != nil {
		return opts, errors.New("coop net why needs one exact domain or IP address — not a URL, a wildcard or a port")
	}
	port := opts.policy.Port
	if port == 0 {
		port = 443
	}
	opts.query, opts.policy = name, networkstate.PolicyQuery{Domain: name, Protocol: "tls", Port: port}
	return opts, opts.policy.Validate()
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
		result, err := evidence.Why(opts.run, opts.policy, true)
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
			return 1, err // the message already names the run and the way to list what is kept
		}
		return 1, netRunErr(opts.run, err)
	}
	if opts.json {
		return 0, netWriteJSON(os.Stdout, result)
	}
	return 0, netRender(os.Stdout, func(b *bytes.Buffer) { writeNetExplanation(b, p, result) })
}

func writeNetWhy(w io.Writer, p ui.Palette, result networkstate.PolicyExplanation) {
	destination := "withheld"
	if !result.Withheld {
		destination = result.Domain
	}
	if !result.Withheld && result.Peer != "" {
		destination = result.Peer
	}
	transport := result.Protocol
	if result.Port != 0 {
		transport += "/" + strconv.Itoa(result.Port)
	} else if result.Protocol == "icmp" {
		transport += " echo-request"
	}
	b := newNetBlock(w, p, "Could run "+result.RunID+" reach "+destination+" "+transport+"?")
	verdict := p.Red("no (" + result.Reason + ")")
	if result.Allowed {
		verdict = p.Green("yes (" + result.Reason + ")")
	}
	b.field("Answer", verdict)
	b.field("Because", result.Message)
	b.field("Rules", result.PolicyFingerprint+", egress "+string(result.Mode))
	if rule := result.Rule; rule != nil {
		matched := rule.To.Domain + rule.To.CIDR
		if rule.To.Service != "" {
			matched = "service " + rule.To.Service
		}
		matched += " " + rule.Protocol
		if len(rule.Ports) != 0 {
			matched += "/" + netPortList(rule.Ports)
		}
		if len(rule.Types) != 0 {
			matched += " types " + strings.Join(rule.Types, ",")
		}
		b.field("Matched", matched)
	}
	for _, origin := range result.Origins {
		b.row(netOriginText(origin))
	}
	if result.ProtectedScope == "not-retained" {
		b.field("Careful", "this run kept no list of its host addresses, so only the fixed protected ranges were checked")
	} else if result.ProtectedScope != "" {
		b.field("Also", "host, metadata and runtime addresses are never reachable, whatever the rules say")
	}
	b.flush(w)
	fmt.Fprintln(w, p.Dim("nothing was sent — this is the rule set that run started with, not today's"))
}

func writeNetExplanation(w io.Writer, p ui.Palette, result networkstate.EventExplanation) {
	event := result.Event
	b := newNetBlock(w, p, "Refused in run "+result.RunID)
	destination := netDestination(event.Name, event.Peer, event.DestinationID)
	if event.Port != nil {
		destination += " port " + strconv.Itoa(*event.Port)
	}
	b.field("Destination", destination)
	b.field("Because", result.Message)
	b.field("When", event.At.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	b.field("Reason", event.Reason)
	b.field("Seen by", event.Source+" ("+event.Kind+", basis "+event.Basis+")")
	b.field("Rules", result.PolicyFingerprint+", gateway epoch "+result.Epoch)
	b.field("Event", event.ID)
	if result.DetailTruncated {
		b.field("Careful", "this run kept only part of its detail — other refusals may be missing")
	}
	b.flush(w)
	if candidate := event.Candidate; candidate != nil {
		fmt.Fprintf(w, "\n%s\n", "To allow it, add this under box.egress_rules in .agent/project.yaml, then run 'coop net approve':")
		fmt.Fprintf(w, "\n  box:\n    egress_rules:\n%s\n", box.NetworkRuleYAML(candidate.Rule))
		fmt.Fprintln(w, p.Dim("it applies to new runs; this box keeps the rules it started with — and traffic alone is not proof the access is needed"))
		return
	}
	fmt.Fprintln(w, p.Dim(netNoDraftReason(event.Reason)))
}

// netNoDraftReason says why this refusal comes with no rule to copy. The two
// cases are different and must not be blurred: a boundary no rule can cross,
// versus a record too thin to name a transport. Neither is a hint that the
// destination should be allowed.
func netNoDraftReason(reason string) string {
	switch reason {
	case "protected_destination", "unsafe_dns_answer", "tls_ech_unsupported", "unsupported_capability",
		"tls_name_missing", "tls_name_invalid":
		return "no rule can allow this one — it is a protected or unsupported destination, not a gap in your rules"
	case "upstream_unreachable", "observation_unavailable":
		return "this one failed to work, it was not refused by a rule — another rule would not fix it"
	default:
		return "no rule to copy: a refused DNS lookup does not say whether the destination is TLS, or on which port — decide that yourself"
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
		return "allowed by you (" + origin.Name + ")"
	case "project":
		return "asked for by the project, and approved"
	case "provider":
		return "a core endpoint for " + origin.Provider + " " + string(origin.Client) + " (bundle " + origin.BundleVersion + ")"
	case "mcp":
		return "the MCP server " + origin.Name + " coop set up for this box"
	default:
		return origin.Kind
	}
}
