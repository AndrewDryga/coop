package secretscan

import (
	"strings"
	"testing"
)

// A finding's identity is what a .coopsecretsignore entry names, so these are the properties a
// person is relying on when they write one down: it survives edits AROUND the finding, it dies
// when the finding itself changes, and two different secrets are never the same entry.

const openAIKey = "sk-proj-N7qFvZm2Ld8RwXcTb3JhKp6Ys9Ug4Aa1"
const otherKey = "sk-proj-Qb4TzH8nWk2Fd7Pv3Lm9Xc6Ry1Ea5Uu0"

func only(t *testing.T, findings []SecretFinding) SecretFinding {
	t.Helper()
	if len(findings) != 1 {
		t.Fatalf("want exactly one finding, got %d: %+v", len(findings), findings)
	}
	return findings[0]
}

// Inserting a line above a finding moves it; it does not make it a different finding. This is the
// whole reason the line number is outside the identity.
func TestFingerprintSurvivesLineInsertion(t *testing.T) {
	before := only(t, ScanFile("config/client.go", "key = \""+openAIKey+"\"\n"))
	after := only(t, ScanFile("config/client.go", "// a new comment\n\nkey = \""+openAIKey+"\"\n"))
	if before.Fingerprint != after.Fingerprint {
		t.Errorf("inserting a line changed the finding's id: %s → %s", before.Fingerprint, after.Fingerprint)
	}
	if before.Line == after.Line {
		t.Errorf("the fixture did not actually move the finding (both on line %d)", before.Line)
	}
}

// A changed value or a moved file is a finding nobody has reviewed, so its old exception must
// stop applying.
func TestFingerprintChangesWithValueOrPath(t *testing.T) {
	base := only(t, ScanFile("config/client.go", "key = \""+openAIKey+"\"\n"))
	value := only(t, ScanFile("config/client.go", "key = \""+otherKey+"\"\n"))
	path := only(t, ScanFile("config/other.go", "key = \""+openAIKey+"\"\n"))
	if base.Fingerprint == value.Fingerprint {
		t.Error("a changed credential kept its old id — an old exception would still hide it")
	}
	if base.Fingerprint == path.Fingerprint {
		t.Error("the same value in a different file kept its id — an exception would travel with it")
	}
}

// Nothing machine-specific goes into an id, so the same repository checked out twice — at a
// different absolute path, by a different person — produces the same entries. The exception file
// can therefore be committed.
func TestFingerprintIsStableAcrossCheckouts(t *testing.T) {
	content := "key = \"" + openAIKey + "\"\n"
	first := only(t, ScanFile("config/client.go", content))
	second := only(t, ScanFile("config/client.go", content))
	if first.Fingerprint != second.Fingerprint {
		t.Errorf("the same content produced two ids: %s and %s", first.Fingerprint, second.Fingerprint)
	}
	if !strings.HasPrefix(first.Fingerprint, FingerprintVersion+":") {
		t.Errorf("id %q is not versioned", first.Fingerprint)
	}
	// The id is a checksum of the material, not the material: it must not contain it.
	if strings.Contains(first.Fingerprint, openAIKey) {
		t.Error("the id carries the credential it identifies")
	}
}

// Two independent tokens on one line are two findings. Reviewing one must not silence the other.
func TestSameLineTokensAreIndependentFindings(t *testing.T) {
	findings := ScanFile("config/client.go", "keys = [\""+openAIKey+"\", \""+otherKey+"\"]\n")
	if len(findings) != 2 {
		t.Fatalf("want both tokens on the line, got %d: %+v", len(findings), findings)
	}
	if findings[0].Fingerprint == findings[1].Fingerprint {
		t.Error("two different tokens on one line share an id — ignoring one would hide the other")
	}
	for _, f := range findings {
		if f.Line != 1 {
			t.Errorf("finding reported on line %d, want 1", f.Line)
		}
	}
}

func TestSameLineDetectorFamiliesAreIndependent(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		want    []string
	}{
		"provider and URL": {
			`key="` + openAIKey + `" db=postgres://app:Tr0ub4dorAlpha9@db/app`,
			[]string{DetectorOpenAIAPIKey, DetectorURLPassword},
		},
		"two URLs": {
			`a=postgres://app:Tr0ub4dorAlpha9@db/a b=redis://app:C0rrectHorseBeta8@db/b`,
			[]string{DetectorURLPassword, DetectorURLPassword},
		},
		"provider and assigned value": {
			`key="` + openAIKey + `" api_key="aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx"`,
			[]string{DetectorOpenAIAPIKey, DetectorAssignedHighEntro},
		},
		"same literal": {
			`api_key="` + openAIKey + `"`,
			[]string{DetectorOpenAIAPIKey},
		},
	} {
		t.Run(name, func(t *testing.T) {
			findings := ScanSecrets(tc.content)
			if len(findings) != len(tc.want) {
				t.Fatalf("findings = %+v, want detectors %v", findings, tc.want)
			}
			for i, want := range tc.want {
				if findings[i].Detector != want {
					t.Errorf("finding %d detector = %q, want %q", i, findings[i].Detector, want)
				}
			}
		})
	}
}

// Every private key in a file starts with the same BEGIN marker. Identity is the BLOCK, so two
// keys in one file are two findings with two ids.
func TestPrivateKeysWithIdenticalHeadersDiffer(t *testing.T) {
	content := "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n" +
		"-----BEGIN OPENSSH PRIVATE KEY-----\nBBBB\n-----END OPENSSH PRIVATE KEY-----\n"
	findings := ScanFile("deploy/keys.pem", content)
	if len(findings) != 2 {
		t.Fatalf("want one finding per key, got %d: %+v", len(findings), findings)
	}
	if findings[0].Fingerprint == findings[1].Fingerprint {
		t.Error("two keys sharing a header share an id — one exception would hide both")
	}
}

// An unterminated block cannot be bounded, so identity falls back to the whole file: the id then
// changes whenever anything else changes, which re-reports rather than hides.
func TestUnterminatedPrivateKeyFallsBackToTheWholeFile(t *testing.T) {
	truncated := "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n"
	first := only(t, ScanFile("deploy/key.pem", truncated))
	second := only(t, ScanFile("deploy/key.pem", truncated+"# an unrelated trailing line\n"))
	if first.Fingerprint == second.Fingerprint {
		t.Error("an unterminated key kept its id after the file changed; the fallback must be conservative")
	}
}

// The label a person reads is presentation and may be reworded; the detector id underneath it is
// the compatibility surface every saved exception depends on.
func TestLabelsAndDetectorIDs(t *testing.T) {
	key := only(t, ScanFile("config/client.go", "key = \""+openAIKey+"\"\n"))
	if key.Detector != DetectorOpenAIAPIKey || key.Label() != "OpenAI API key" {
		t.Errorf("openai finding = %+v, want the openai detector and label", key)
	}
	url := only(t, ScanFile("config/db.yml", "url: postgres://app:s3cr3tpassphrase@db:5432/app\n"))
	if url.Detector != DetectorURLPassword || url.Label() != "Password in a connection URL" {
		t.Errorf("url finding = %+v, want the connection-url detector and label", url)
	}
	entropy := only(t, ScanFile("config/app.yml", "api_key: aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx\n"))
	if entropy.Detector != DetectorAssignedHighEntro || entropy.Label() != "Possible secret assigned to api_key" {
		t.Errorf("entropy finding = %+v, want the assignment detector naming its key", entropy)
	}
	// The key's NAME is safe to print; its value never is.
	if strings.Contains(entropy.Label(), "aB3xK9mP") {
		t.Error("the assignment label leaked the value it found")
	}
}

// ScanSecrets is the pure detector every other consumer shares — fork merge, checkpoint upload,
// session redaction. It must keep finding exactly what it found before, and must not hand those
// callers an id, since identity needs a path they do not supply.
func TestScanSecretsKeepsItsContractForSharedCallers(t *testing.T) {
	content := "key = \"" + openAIKey + "\"\n"
	shared := only(t, ScanSecrets(content))
	if shared.Kind != "OpenAI API key" || shared.Line != 1 {
		t.Errorf("shared scan = %+v, want the same kind and line as before", shared)
	}
	if shared.Fingerprint != "" {
		t.Errorf("shared scan produced an id (%q) without a path to bind it to", shared.Fingerprint)
	}
}
