package networkstate

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func featureBundle(client egress.Client, auth string) egress.Bundle {
	return egress.Bundle{Provider: "model", Client: client, Backend: "direct", AuthMode: auth, Version: "1",
		Core: []egress.Rule{rule("api.example.com")}, Features: map[string][]egress.Rule{"cloud-mcp": {rule("mcp.example.com")}}}
}

func TestOversizedBundleSelectionCannotPartiallyPublish(t *testing.T) {
	s := openStore(t)
	first, second := featureBundle(egress.ClientACP, "key"), featureBundle(egress.ClientCLI, "key")
	for i := range egress.MaxConstraints {
		second.Sources = append(second.Sources, fmt.Sprintf("https://example.com/%d/", i)+strings.Repeat("a", 1500))
	}
	if err := s.CheckBundles([]egress.Bundle{first, second}); err == nil {
		t.Fatal("oversized bundle accepted")
	}
	files, err := os.ReadDir(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "bundle-") {
			t.Fatal("byte validation occurred after a partial publication")
		}
	}
}

func TestBundleSourceOrderAndEmptyShapeDoNotCauseIntegrityDrift(t *testing.T) {
	s := openStore(t)
	bundle := featureBundle(egress.ClientCLI, "key")
	for _, sources := range [][]string{nil, {}} {
		bundle.Sources = sources
		if err := s.CheckBundles([]egress.Bundle{bundle}); err != nil {
			t.Fatal("empty shape caused false drift", err)
		}
	}
	bundle.Version = "2"
	for _, sources := range [][]string{{"https://b.example.com", "https://a.example.com"}, {"https://a.example.com", "https://b.example.com", "https://a.example.com"}} {
		bundle.Sources = sources
		if err := s.CheckBundles([]egress.Bundle{bundle}); err != nil {
			t.Fatal("source order caused false drift", err)
		}
	}
}

func TestHistoricalApprovalSurvivesSuffixChangesButNewAuthoringDoesNot(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	id, err := s.projectID(project)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a private historical record, as if the suffix catalog changed after
	// approval. New approval below must still apply today's stricter catalog.
	data, err := json.Marshal(Approval{Version: 1, ProjectID: id, Posture: egress.Filtered, Envelope: []egress.Rule{rule("*.github.io")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publish("approval-"+id+".json", data, true); err != nil {
		t.Fatal(err)
	}
	if approval, err := s.Approval(project); err != nil || approval == nil {
		t.Fatal("historical approval became unreadable", err)
	}
	if got, err := s.Admit(project, Admission{Requests: []egress.Rule{rule("project.github.io")}}); err != nil || !got.Domain("project.github.io", 443).Allowed {
		t.Fatal("exact child lost its existing approved envelope", err)
	}
	if got, err := s.Admit(project, Admission{Requests: []egress.Rule{rule("*.github.io")}}); err == nil || got.Fingerprint != "" {
		t.Fatal("new wildcard request bypassed today's catalog", err)
	}
	if err := s.Approve(project, egress.Filtered, []egress.Rule{rule("*.github.io")}, nil); err == nil {
		t.Fatal("new approval bypassed today's catalog")
	}
}

func TestFeatureApprovalBindsVariantButAllowsUnchangedReleaseExpansion(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	bundles := []egress.Bundle{featureBundle(egress.ClientCLI, "key"), featureBundle(egress.ClientACP, "oauth")}
	requests := []egress.Rule{{To: egress.Destination{Provider: "model", Features: []string{"cloud-mcp"}}}}
	if err := s.Approve(project, egress.Filtered, requests, bundles); err != nil {
		t.Fatal(err)
	}
	first, err := s.Approval(project)
	if err != nil || len(first.Features) != 2 {
		t.Fatal("approval lost selected variants", first, err)
	}
	slices.Reverse(bundles)
	if err := s.Approve(project, egress.Filtered, requests, bundles); err != nil {
		t.Fatal(err)
	}
	second, err := s.Approval(project)
	if err != nil || !equalJSON(first, second) {
		t.Fatal("approval changed with selection order", err)
	}
	for i := range bundles {
		bundles[i].Version = "2"
		bundles[i].Core = append(bundles[i].Core, rule("new-core.example.com"))
	}
	if _, err := s.Admit(project, Admission{Requests: requests, Bundles: bundles}); err != nil {
		t.Fatal("core maintenance unnecessarily invalidated unchanged optional expansion", err)
	}
	for _, change := range []string{"client", "backend", "auth", "expansion"} {
		changed := slices.Clone(bundles)
		switch change {
		case "client":
			changed[0].Client = egress.ClientCLI
		case "backend":
			changed[0].Backend = "other"
		case "auth":
			changed[0].AuthMode = "other"
		case "expansion":
			changed[0].Version = "3"
			changed[0].Features = map[string][]egress.Rule{"cloud-mcp": {rule("different.example.com")}}
		}
		if got, err := s.Admit(project, Admission{Requests: requests, Bundles: changed}); err == nil || got.Fingerprint != "" || !strings.Contains(err.Error(), "network_approval_required") {
			t.Fatal("variant or optional expansion reused unrelated approval", change, got, err)
		}
	}
}

func TestBundleIntegrityIncludesClientAndValidationPrecedesPersistence(t *testing.T) {
	s := openStore(t)
	cli, acp := featureBundle(egress.ClientCLI, "oauth"), featureBundle(egress.ClientACP, "oauth")
	acp.Core = []egress.Rule{rule("acp.example.com")}
	if err := s.CheckBundles([]egress.Bundle{cli, acp}); err != nil {
		t.Fatal("different clients collided", err)
	}
	cli.Core = []egress.Rule{rule("changed.example.com")}
	if err := s.CheckBundles([]egress.Bundle{cli}); err == nil {
		t.Fatal("same client/version drift accepted")
	}
	clean := openStore(t)
	acp.Client = ""
	if err := clean.CheckBundles([]egress.Bundle{cli, acp}); err == nil {
		t.Fatal("missing client accepted")
	}
	files, err := os.ReadDir(clean.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "bundle-") {
			t.Fatal("invalid selection partially persisted bundles")
		}
	}
}
