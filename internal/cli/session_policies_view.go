package cli

// The human view of `coop sessions policies`: what a remote application can ask THIS machine to
// do. It answers the operator's four questions — which project, which agents and accounts, whether
// they can change files, and what they may reach on the network — from the loaded Policy and the
// network this host actually resolves for it. Identifiers (the digests a worker configuration
// advertises) stay in --json, where a machine reads them.

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkreport"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
)

// sessionConfigurationView is one configuration as a person reads it. Every field is resolved
// evidence: an access summary derived from the policy's mode, the targets pinned at load, and the
// network this host compiled. issue is set INSTEAD of the network rows when the host could not
// resolve them — an unresolved policy gets the question it raises, never a made-up permission.
type sessionConfigurationView struct {
	Name             string
	Repository       string
	Branch           string
	Companions       []string
	Access           string
	Targets          []string
	Network          string
	Rules            []string
	Export           bool
	Issue            string
	ApprovalRequired bool
}

func sessionConfigurationViewOf(name string, policy sessionsvc.Policy, network sessionPolicyNetwork, snapshot egress.Snapshot) sessionConfigurationView {
	view := sessionConfigurationView{Name: name, Access: sessionAccessSummary(policy)}
	if policy.Mode != agents.ModeBare {
		view.Repository = policy.Repository
		view.Branch = policy.Branch
		for _, companion := range policy.Companions {
			row := companion.Repository
			if companion.Branch != "" {
				row += " (" + companion.Branch + ")"
			}
			view.Companions = append(view.Companions, row)
		}
	}
	for _, target := range policy.Targets {
		view.Targets = append(view.Targets, target.String())
	}
	if network.Unresolved != "" {
		view.Issue = network.Unresolved
		view.ApprovalRequired = network.ApprovalRequired
		return view
	}
	view.Network, view.Rules = sessionNetworkSummary(egress.Mode(network.Mode), snapshot, policy)
	view.Export = policy.Egress.ExportDestinations
	return view
}

// sessionAccessSummary answers what a session may do to the files, from the only two fields that
// decide it. There is no separate commit permission: a writable session works in its OWN copy of
// the project, so committing there changes nothing the owner did not already hand over.
func sessionAccessSummary(policy sessionsvc.Policy) string {
	switch {
	case policy.Mode == agents.ModeBare:
		return "Questions and answers only; no project files or tools"
	case policy.Mode == agents.ModeReadOnly || policy.RepositoryReadOnly:
		return "Read-only"
	}
	return "Read, edit, and commit in a separate project copy"
}

// sessionNetworkSummary describes the posture and, for a filtered one, the RESOLVED grants — the
// provider bundles a selected agent brings with it, then the approved rules. The bundles are
// collapsed to their vendors because they are the provider's own connection requirements, not
// something anyone requested; every other grant is named by its destination.
func sessionNetworkSummary(mode egress.Mode, snapshot egress.Snapshot, policy sessionsvc.Policy) (string, []string) {
	if mode != egress.Filtered {
		if mode == egress.Open || mode == "" {
			return "Unrestricted", nil
		}
		return "Offline — internet access is blocked", nil
	}
	var vendors []string
	var rules []string
	seen := map[string]bool{}
	for _, grant := range snapshot.Grants {
		if vendor := sessionGrantVendor(grant); vendor != "" {
			if !seen["vendor:"+vendor] {
				seen["vendor:"+vendor] = true
				vendors = append(vendors, vendor)
			}
			continue
		}
		row := sessionRuleLabel(grant.Rule)
		if !seen["rule:"+row] {
			seen["rule:"+row] = true
			rules = append(rules, row)
		}
	}
	// A policy whose rules this host could not compile still has its own YAML to show; showing
	// them is better than an empty list, and the mode above is still the resolved one.
	if len(snapshot.Grants) == 0 {
		for _, rule := range policy.Egress.Rules {
			rules = append(rules, sessionRuleLabel(rule))
		}
	}
	sort.Strings(rules)
	var out []string
	if len(vendors) > 0 {
		sort.Strings(vendors)
		out = append(out, ui.List(vendors, "and")+" provider endpoints")
	}
	return "Filtered — only approved network traffic is allowed", append(out, rules...)
}

// sessionGrantVendor names the provider whose built-in endpoints a grant came from, or "" when
// the grant is an explicitly approved rule.
func sessionGrantVendor(grant egress.Grant) string {
	for _, origin := range grant.Origins {
		if origin.Kind != "provider" {
			continue
		}
		if ag, ok := agents.Get(origin.Provider); ok {
			return ag.Vendor()
		}
		return origin.Provider
	}
	return ""
}

// sessionRuleLabel is one approved rule the way the network views say it — `github.com:443 · TLS`,
// `10.0.0.0/8 · TCP`, `service registry · UDP` — so a destination reads the same everywhere. A
// rule with no protocol or no ports keeps what it has rather than being labeled TLS by default.
func sessionRuleLabel(rule egress.Rule) string {
	destination := box.NetworkRuleText(rule)
	if rule.To.Provider != "" || rule.To.Domain == "" && rule.To.IP == "" && rule.To.CIDR == "" && rule.To.Service == "" {
		return destination
	}
	label := rule.To.Domain
	switch {
	case label != "":
	case rule.To.IP != "":
		label = rule.To.IP
	case rule.To.CIDR != "":
		label = rule.To.CIDR
	default:
		label = "service " + rule.To.Service
	}
	if len(rule.Ports) > 0 {
		ports := make([]string, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
		label += ":" + strings.Join(ports, ",")
	}
	if rule.Protocol == "" {
		return label
	}
	return label + " · " + networkreport.TransportLabel(rule.Protocol)
}

// renderSessionConfigurations prints the human view: one block per configuration, its fields in a
// single label column, and the file to edit at the end. The rotation sentence appears only when
// some configuration actually lists more than one agent.
func renderSessionConfigurations(w io.Writer, p ui.Palette, file string, views []sessionConfigurationView) {
	fmt.Fprintln(w, p.Bold("Remote session configurations"))
	fmt.Fprintln(w, "Each name defines which project and agents a remote application can use.")
	rotates := false
	for _, view := range views {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.Bold(p.Cyan(view.Name)))
		field := func(label, value string) {
			fmt.Fprintf(w, "  %s  %s\n", padRight(label, 7), value)
		}
		if view.Repository != "" {
			field("Project", view.Repository)
		}
		if view.Branch != "" {
			field("Branch", view.Branch)
		}
		for i, companion := range view.Companions {
			if i == 0 {
				field("Also", companion)
				continue
			}
			fmt.Fprintf(w, "  %s  %s\n", strings.Repeat(" ", 7), companion)
		}
		field("Access", view.Access)
		switch len(view.Targets) {
		case 0:
		case 1:
			field("Agent", view.Targets[0])
		default:
			rotates = true
			field("Agents", view.Targets[0])
			for _, target := range view.Targets[1:] {
				fmt.Fprintf(w, "  %s  %s\n", strings.Repeat(" ", 7), target)
			}
		}
		if view.Issue != "" {
			// An unresolved network is a question to answer, not a permission summary to invent.
			// The remedy names the policy's OWN project: the reader's shell may be elsewhere.
			fmt.Fprintln(w)
			headline := "Network access is not ready"
			if view.ApprovalRequired {
				headline = "Network access needs approval"
			}
			fmt.Fprintf(w, "  %s %s\n", p.Yellow("⚠"), headline)
			fmt.Fprintln(w)
			for _, line := range strings.Split(sessionsvc.BoundedDetail(view.Issue), "\n") {
				fmt.Fprintln(w, "      "+line)
			}
			if view.ApprovalRequired {
				fmt.Fprintln(w)
				fmt.Fprintln(w, "  Run coop net approve in "+view.Repository+".")
			}
			continue
		}
		field("Network", view.Network)
		for _, rule := range view.Rules {
			fmt.Fprintln(w, "    "+rule)
		}
		if view.Export {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  Remote hostnames and IP addresses are shared with the remote controller.")
		}
	}
	if rotates {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Agents are tried in order when one reaches its usage limit.")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Edit configurations: "+file)
}
