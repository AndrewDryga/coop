package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const QualificationSchema = 2
const QualificationNotConfigured = "not_configured"

// Qualification is the operator-run evidence beside locked-clients/package-lock.json. A matching
// client digest alone is insufficient: required suite scopes and evidence must also be complete.
type Qualification struct {
	Schema      int                          `json:"schema"`
	QualifiedOn string                       `json:"qualified_on"`
	Platform    string                       `json:"platform"` // the one the suites ran on
	Lock        string                       `json:"lock_sha256"`
	Clients     map[string][]string          `json:"clients"`
	Suites      map[string]map[string]string `json:"suites"`
}

type QualificationEvidence int

const (
	QualificationPinnedCLI QualificationEvidence = iota
	QualificationPassedTest
	QualificationAccounts
)

type QualificationSuite struct {
	Name      string
	Providers []string
	Evidence  QualificationEvidence
}

// QualificationRequirements is shared by the recorder and committed-record gate. Returned data
// is fresh; only the accounts row can honestly lack a configured second account.
func QualificationRequirements() []QualificationSuite {
	return []QualificationSuite{
		{"provider-live-e2e-all", Names(), QualificationPinnedCLI},
		{"provider-live-e2e-effort", Names(), QualificationPinnedCLI},
		{"provider-resume-live-e2e-all", Names(), QualificationPinnedCLI},
		{"provider-loop-live-e2e-all", Names(), QualificationPinnedCLI},
		{"provider-consult-live-e2e-all", Names(), QualificationPinnedCLI},
		{"provider-delegate-live-e2e-all", Names(), QualificationPinnedCLI},
		{"provider-network-live-e2e-all", Names(), QualificationPinnedCLI},
		{"provider-accounts-live-e2e-all", Names(), QualificationAccounts},
		{"acp-e2e", Names(), QualificationPassedTest},
		{"native-roles-e2e", Names(), QualificationPassedTest},
		{"skills-e2e", Names(), QualificationPassedTest},
		{"mcp-e2e", []string{"codex", "gemini", "grok"}, QualificationPassedTest},
	}
}

// PinnedCLIVersions is the CLI identity emitted by the live summaries. ACP adapters have their own
// distinct pins in QualifiedClientSet; a test-pass ACP row does not claim to report a CLI version.
func PinnedCLIVersions() (map[string]string, error) {
	closure, err := LockedClientClosure(ClientPlatform{OS: "linux", Architecture: "amd64", Libc: "glibc"})
	if err != nil {
		return nil, err
	}
	pinned := make(map[string]string)
	for _, client := range closure.Clients {
		if client.Client == egress.ClientCLI {
			pinned[client.Provider] = client.Provider + "-cli " + client.Version
		}
	}
	return pinned, nil
}

func ValidateQualification(q Qualification, lock string, clients map[string][]string) error {
	if q.Schema != QualificationSchema {
		return fmt.Errorf("qualification schema is %d, want %d", q.Schema, QualificationSchema)
	}
	date, err := time.Parse(time.DateOnly, q.QualifiedOn)
	if err != nil || date.Format(time.DateOnly) != q.QualifiedOn {
		return errors.New("qualification needs a valid YYYY-MM-DD date")
	}
	if q.Platform != "linux/amd64" && q.Platform != "linux/arm64" {
		return errors.New("qualification platform must be linux/amd64 or linux/arm64")
	}
	if mismatch := QualificationMismatch(q, lock, clients); mismatch != "" {
		return errors.New(mismatch)
	}
	required := QualificationRequirements()
	if len(q.Suites) != len(required) {
		return fmt.Errorf("qualification has %d suites, want %d", len(q.Suites), len(required))
	}
	pinned, err := PinnedCLIVersions()
	if err != nil {
		return err
	}
	for _, suite := range required {
		results, ok := q.Suites[suite.Name]
		if !ok || len(results) != len(suite.Providers) {
			return fmt.Errorf("qualification suite %s is missing or has incomplete provider scope", suite.Name)
		}
		for _, provider := range suite.Providers {
			got := results[provider]
			want := pinned[provider]
			if suite.Evidence == QualificationPassedTest {
				want = "passed"
			}
			if suite.Evidence == QualificationAccounts && got == QualificationNotConfigured {
				continue
			}
			if want == "" || got != want {
				return fmt.Errorf("qualification suite %s: %s has %q, want %q", suite.Name, provider, got, want)
			}
		}
	}
	return nil
}
