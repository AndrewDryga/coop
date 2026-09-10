package networkstate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func admissionMode(mode egress.Mode) *egress.Mode { return &mode }

func TestAdmissionPreviewChecksEnvelopeWithoutReadingFeatureBundles(t *testing.T) {
	root, project := filepath.Join(t.TempDir(), "network"), t.TempDir()
	request := egress.Rule{To: egress.Destination{Provider: "model", Features: []string{"cloud-mcp"}}}
	input := Admission{Requests: []egress.Rule{request}}
	if _, err := PreviewAdmissionMode(root, project, nil, input); err == nil || !strings.Contains(err.Error(), "network_approval_required") {
		t.Fatal("unapproved preview proceeded", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denial created authority", err)
	}
	s, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bundle := egress.Bundle{Provider: "model", Client: egress.ClientCLI, Backend: "direct", AuthMode: "key", Version: "1", Features: map[string][]egress.Rule{"cloud-mcp": {rule("mcp.example.com")}}}
	if err := approve(s, project, egress.Filtered, input.Requests, []egress.Bundle{bundle}); err != nil {
		t.Fatal(err)
	}
	if mode, err := PreviewAdmissionMode(root, project, nil, input); err != nil || mode != egress.Filtered {
		t.Fatal("approved feature needs no bundle for preliminary envelope check", mode, err)
	}
	input.Requests = []egress.Rule{rule("outside.example.com")}
	if _, err := PreviewAdmissionMode(root, project, nil, input); err == nil || !strings.Contains(err.Error(), "network_approval_required") {
		t.Fatal("changed envelope proceeded", err)
	}
}

func TestAdmissionPreviewDoesNotCreateOrRepairAuthority(t *testing.T) {
	root, project := filepath.Join(t.TempDir(), "network"), t.TempDir()
	input := Admission{ProjectMode: admissionMode(egress.Filtered)}
	mode, err := PreviewAdmissionMode(root, project, nil, input)
	if err != nil || mode != egress.Filtered {
		t.Fatal(mode, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created authority", err)
	}
	store, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := approve(store, project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mode, err = PreviewAdmissionMode(root, project, nil, input)
	if err != nil || mode != egress.None {
		t.Fatal("preview ignored remembered posture", mode, err)
	}
	if err := os.Remove(filepath.Join(root, "owner.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewAdmissionMode(root, project, nil, input); err == nil {
		t.Fatal("preview reset lost key over existing approval")
	}
	if _, err := os.Stat(filepath.Join(root, "owner.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview repaired owner key", err)
	}
}

func TestAdmissionDirectPresenceMatrix(t *testing.T) {
	choices := []*egress.Mode{nil, admissionMode(egress.Open), admissionMode(egress.Filtered), admissionMode(egress.None)}
	for _, invocation := range choices {
		for _, remembered := range choices {
			for _, preference := range choices {
				for _, project := range choices {
					var approval *Approval
					if remembered != nil {
						approval = &Approval{Posture: *remembered}
					}
					want := egress.Open
					for _, value := range []*egress.Mode{project, preference, remembered, invocation} {
						if value != nil {
							want = *value
						}
					}
					input := Admission{InvocationMode: invocation, HostPreference: preference, ProjectMode: project}
					got, err := input.resolveMode(approval)
					if err != nil || got != want {
						t.Fatalf("presence: %#v approval=%#v: got %q %v, want %q", input, approval, got, err, want)
					}
				}
			}
		}
	}
}

func TestAdmissionNamedPolicyMatrix(t *testing.T) {
	choices := []*egress.Mode{nil, admissionMode(egress.Open), admissionMode(egress.Filtered), admissionMode(egress.None)}
	for _, policy := range choices[1:] {
		for _, remembered := range choices {
			var approval *Approval
			if remembered != nil {
				approval = &Approval{Posture: *remembered}
			}
			conflict := remembered != nil && *remembered != egress.Open && *remembered != *policy
			input := Admission{PolicyMode: policy, HostPreference: admissionMode(egress.None), ProjectMode: admissionMode(egress.Open)}
			got, err := input.resolveMode(approval)
			if (err != nil) != conflict || (!conflict && got != *policy) || (conflict && got != "") {
				t.Fatal("named policy ignored a remembered restriction", input, approval, got, err)
			}
		}
	}
	if _, err := (Admission{PolicyMode: admissionMode(egress.Open), InvocationMode: admissionMode(egress.None)}).resolveMode(nil); err == nil {
		t.Fatal("API invocation override accepted")
	}
}

func TestAdmissionRejectsInvalidShadowedModesAndRuleConflicts(t *testing.T) {
	for _, value := range []egress.Mode{"", "OPEN", "Filtered", "unknown"} {
		for field := 0; field < 4; field++ {
			input := Admission{InvocationMode: admissionMode(egress.None)}
			switch field {
			case 0:
				input.InvocationMode = &value
			case 1:
				input.HostPreference = &value
			case 2:
				input.ProjectMode = &value
			case 3:
				input.InvocationMode, input.PolicyMode = nil, &value
			}
			if got, err := input.resolveMode(nil); err == nil || got != "" {
				t.Fatal("invalid present mode became absence", input, got, err)
			}
		}
	}
	for _, operator := range []bool{false, true} {
		input := Admission{Requests: []egress.Rule{rule("example.com")}}
		if operator {
			input.Operator, input.Requests = []egress.Input{{Rules: input.Requests, Origin: egress.Origin{Kind: "operator"}}}, nil
		}
		if mode, err := input.resolveMode(nil); err != nil || mode != egress.Filtered {
			t.Fatal("rules did not imply filtered", mode, err)
		}
		for _, mode := range []egress.Mode{egress.Open, egress.None} {
			for _, source := range []string{"invocation", "posture", "preference", "project", "policy"} {
				value := input
				var approval *Approval
				switch source {
				case "invocation":
					value.InvocationMode = &mode
				case "posture":
					approval = &Approval{Posture: mode}
				case "preference":
					value.HostPreference = &mode
				case "project":
					value.ProjectMode = &mode
				case "policy":
					value.PolicyMode = &mode
				}
				if got, err := value.resolveMode(approval); err == nil || got != "" {
					t.Fatal("rules silently changed an explicit/stored mode", source, mode, got, err)
				}
			}
		}
	}
}

func TestAdmitKeepsPostureWithoutRewritingOrRecapturing(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	for _, input := range []Admission{
		{}, // deleting every repo request does not reset posture
		{HostPreference: admissionMode(egress.Open), ProjectMode: admissionMode(egress.Open)},
		{HostPreference: admissionMode(egress.None)},
	} {
		got, err := s.Admit(project, input)
		if err != nil || got.Mode != egress.Filtered || len(got.Grants) != 0 {
			t.Fatal("removed requests changed posture or retained deleted grants", got, err)
		}
	}
	for _, input := range []Admission{
		{InvocationMode: admissionMode(egress.Open)},
		{InvocationMode: admissionMode(egress.None)},
	} {
		if _, err := s.Admit(project, input); err != nil {
			t.Fatal(err)
		}
		approval, err := s.Approval(project)
		if err != nil || approval.Posture != egress.Filtered || len(approval.Envelope) != 1 {
			t.Fatal("invocation override rewrote approval", approval, err)
		}
	}
	snapshot, err := s.Admit(project, Admission{Requests: []egress.Rule{rule("example.com")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	old, err := s.LoadSnapshot(project, snapshot.Fingerprint)
	if err != nil || old.Mode != egress.Filtered || !old.Domain("example.com", 443).Allowed {
		t.Fatal("new posture retroactively changed frozen capture", old, err)
	}
}

func TestAdmitUsesOneApprovalForModeAndEnvelope(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("example.com")}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	changed := false
	s.syncDir = func(dir *os.File) error {
		if !changed {
			changed = true
			if err := approve(s, project, egress.None, nil, nil); err != nil {
				return err
			}
		}
		return dir.Sync()
	}
	bundle := egress.Bundle{Provider: "model", Client: egress.ClientCLI, Version: "1", Backend: "direct", AuthMode: "key"}
	got, err := s.Admit(project, Admission{Requests: requests, Bundles: []egress.Bundle{bundle}})
	if err != nil || !changed || got.Mode != egress.Filtered || !got.Domain("example.com", 443).Allowed {
		t.Fatal("capture combined different approval generations", got, changed, err)
	}
	current, err := s.Admit(project, Admission{})
	if err != nil || current.Mode != egress.None {
		t.Fatal("subsequent capture ignored the new posture", current, err)
	}
}

func TestAdmitFailsClosedWithoutUsableAuthority(t *testing.T) {
	for _, failure := range []string{"corrupt-approval", "missing-key", "unapproved", "policy"} {
		t.Run(failure, func(t *testing.T) {
			s, project := openStore(t), t.TempDir()
			if err := approve(s, project, egress.Filtered, nil, nil); err != nil {
				t.Fatal(err)
			}
			input := Admission{}
			switch failure {
			case "corrupt-approval":
				id, err := s.projectID(project)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.Path(), "approval-"+id+".json"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-key":
				if err := os.Remove(filepath.Join(s.Path(), "owner.key")); err != nil {
					t.Fatal(err)
				}
			case "unapproved":
				input.Requests = []egress.Rule{rule("example.com")}
			case "policy":
				input.PolicyMode = admissionMode(egress.None)
			}
			got, err := s.Admit(project, input)
			if err == nil || got.Fingerprint != "" || got.Mode != "" || len(got.Grants) != 0 {
				t.Fatal("failure returned usable authority", got, err)
			}
		})
	}
}

func TestAdmitNoneHasNoProviderExceptionAndPersistenceFailureReturnsNoCapture(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	bundle := egress.Bundle{Provider: "model", Client: egress.ClientCLI, Version: "1", Backend: "direct", AuthMode: "key", Core: []egress.Rule{rule("example.com")}}
	got, err := s.Admit(project, Admission{InvocationMode: admissionMode(egress.None), Bundles: []egress.Bundle{bundle}})
	if err != nil || len(got.Grants) != 0 || len(got.Dependencies) != 0 {
		t.Fatal("none admitted a provider exception", got, err)
	}
	s.syncDir = func(*os.File) error { return errors.New("fixture storage failure") }
	for attempt := 0; attempt < 3; attempt++ {
		got, err = s.Admit(project, Admission{})
		if err == nil || got.Fingerprint != "" {
			t.Fatal("failed durable capture returned authority", attempt, got, err)
		}
	}
	s.syncDir = nil
	if got, err := s.Admit(project, Admission{}); err != nil || got.Fingerprint == "" {
		t.Fatal("capture did not recover after storage sync recovered", got, err)
	}
}

func TestBundlePublicationRetryConfirmsDurability(t *testing.T) {
	s := openStore(t)
	bundles := []egress.Bundle{{Provider: "model", Client: egress.ClientCLI, Version: "1", Backend: "direct", AuthMode: "key"}}
	s.syncDir = func(*os.File) error { return errors.New("fixture storage failure") }
	for attempt := 0; attempt < 3; attempt++ {
		if err := s.checkBundles(bundles); err == nil {
			t.Fatal("bundle retry ignored persistent directory sync failure", attempt)
		}
	}
	s.syncDir = nil
	if err := s.checkBundles(bundles); err != nil {
		t.Fatal("bundle publication failed after storage recovered", err)
	}
}

// storeDigest is every byte of the owner-private tree, plus each entry's name and mode: the
// evidence a resolve wrote nothing at all — not an approval, not a snapshot, not a key.
func storeDigest(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.New()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(sum, "%s\x00%o\x00%d\x00", entry.Name(), info.Mode().Perm(), len(data))
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Resolve answers what Admit would authorize and writes nothing doing it. The two share one input
// assembly, so the fingerprint a host publishes before a launch is the fingerprint that launch
// freezes — and the published one costs no approval, no snapshot and no owner key.
func TestResolveCompilesTheAdmittedAuthorityWithoutPublishingIt(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("example.com")}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	bundle := egress.Bundle{Provider: "model", Client: egress.ClientCLI, Version: "1", Backend: "direct", AuthMode: "key", Core: []egress.Rule{rule("api.example.net")}}
	input := Admission{Requests: requests, Bundles: []egress.Bundle{bundle}}

	before := storeDigest(t, s.Path())
	resolved, err := s.Resolve(project, input)
	if err != nil || resolved.Mode != egress.Filtered || !resolved.Domain("example.com", 443).Allowed {
		t.Fatalf("resolve = %+v, %v; want the filtered authority Admit would compile", resolved, err)
	}
	if after := storeDigest(t, s.Path()); after != before {
		t.Fatal("resolving the fingerprint wrote host state")
	}
	if _, err := s.LoadSnapshot(project, resolved.Fingerprint); err == nil {
		t.Fatal("resolve published its snapshot; only a capture may")
	}

	admitted, err := s.Admit(project, input)
	if err != nil || admitted.Fingerprint != resolved.Fingerprint {
		t.Fatalf("admit = %q, %v; want the resolved fingerprint %q", admitted.Fingerprint, err, resolved.Fingerprint)
	}
	if _, err := s.LoadSnapshot(project, admitted.Fingerprint); err != nil {
		t.Fatalf("admitted snapshot is not retrievable: %v", err)
	}
	if storeDigest(t, s.Path()) == before {
		t.Fatal("admit published nothing; the two paths are indistinguishable")
	}

	// A request outside the approved envelope resolves to nothing, and refusing costs nothing:
	// resolving must never be the act that widens or remembers anything.
	refused := storeDigest(t, s.Path())
	widened := Admission{Requests: append(slices.Clone(requests), rule("elsewhere.example")), Bundles: input.Bundles}
	if got, err := s.Resolve(project, widened); err == nil || got.Fingerprint != "" {
		t.Fatalf("unapproved resolve = %+v, %v; want a refusal with no authority", got, err)
	}
	if storeDigest(t, s.Path()) != refused {
		t.Fatal("a refused resolve wrote host state")
	}
}
