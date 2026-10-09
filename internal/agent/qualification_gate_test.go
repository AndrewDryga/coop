package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic records prove the comparison independently of the paid qualification. The committed
// record gate below stays dormant until an operator runs provider-qualify and commits real evidence.
func TestQualificationMismatchNamesWhatMoved(t *testing.T) {
	lock, clients, err := QualifiedClientSet()
	if err != nil {
		t.Fatal(err)
	}
	if lock == "" || len(clients) == 0 {
		t.Fatal("this binary builds no client set to qualify")
	}

	matching := Qualification{Lock: lock, Clients: clients}
	if got := QualificationMismatch(matching, lock, clients); got != "" {
		t.Errorf("a record of exactly this client set reported a mismatch: %s", got)
	}

	// A dependency bump: the lock digest moves, and nothing about the clients has to change for the
	// qualification to be void — that is the case a version-only check would miss.
	bumped := Qualification{Lock: strings.Repeat("0", 64), Clients: clients}
	if got := QualificationMismatch(bumped, lock, clients); !strings.Contains(got, "locked dependency set changed") {
		t.Errorf("a moved lock digest was accepted: %q", got)
	}

	// A client bump on one platform.
	for platform, lines := range clients {
		altered := map[string][]string{}
		for k, v := range clients {
			altered[k] = append([]string(nil), v...)
		}
		altered[platform] = append(append([]string(nil), lines[:len(lines)-1]...), "claude-code@999.0.0")
		if got := QualificationMismatch(Qualification{Lock: lock, Clients: altered}, lock, clients); !strings.Contains(got, platform) {
			t.Errorf("a client bump on %s was accepted: %q", platform, got)
		}
		break
	}

	// A platform that lost coverage, and one that gained it: both mean the record no longer
	// describes what this binary builds.
	short := map[string][]string{}
	for platform, lines := range clients {
		short[platform] = lines
		break
	}
	if len(clients) > 1 {
		if got := QualificationMismatch(Qualification{Lock: lock, Clients: short}, lock, clients); got == "" {
			t.Error("a record missing a platform was accepted")
		}
	}
	extra := map[string][]string{"linux/s390x": {"claude-code@1.0.0"}}
	for platform, lines := range clients {
		extra[platform] = lines
	}
	if got := QualificationMismatch(Qualification{Lock: lock, Clients: extra}, lock, clients); !strings.Contains(got, "no longer builds") {
		t.Errorf("a record qualifying an unbuilt platform was accepted: %q", got)
	}
}

// A missing record remains dormant. Once recorded, drift fails unless the operator approved the
// exact, explicitly unqualified release below. That exception never changes qualification evidence.
func TestTheLockedClientsMatchTheirQualification(t *testing.T) {
	const record = "locked-clients/qualification.json"
	data, err := os.ReadFile(filepath.Join(".", record))
	if os.IsNotExist(err) {
		t.Skipf("no %s yet: the locked clients are UNQUALIFIED, and no gate stops a pin move until "+
			"an operator runs `make provider-qualify` and commits the record (PAID)", record)
	}
	if err != nil {
		t.Fatal(err)
	}
	var q Qualification
	if err := decodeQualification(data, &q); err != nil {
		t.Fatalf("the committed qualification does not parse: %v", err)
	}
	lock, clients, err := QualifiedClientSet()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateQualification(q, lock, clients); err != nil {
		changelog, readErr := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
		if readErr == nil && permitsV1100QualificationException(data, lock, clients, string(changelog)) {
			t.Logf("UNQUALIFIED: operator approved v11.0.0 on 2026-10-09 without a fresh full upstream qualification; exact client identity and historical evidence remain unchanged: %v", err)
			return
		}
		t.Fatalf("the committed qualification is incomplete or no longer describes this binary's clients: %v", err)
	}
}

// Delete after v11.0.0. Pin both the retained evidence and every platform's native/adapter identity;
// the latest numbered release binds the exception even after Unreleased is reopened on main.
func permitsV1100QualificationException(record []byte, lock string, clients map[string][]string, changelog string) bool {
	if fmt.Sprintf("%x", sha256.Sum256(record)) != "b5afd7bdfef2fa016e04b9062ec7306df354bd9615764f2f84dfd870686d4876" {
		return false
	}
	identity, err := json.Marshal(Qualification{Lock: lock, Clients: clients})
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(identity)) != "7cc82deafd1a85be711553ce4c18d7101275c4d0fedadcba9e3ae2dfeb0d6a70" {
		return false
	}
	for line := range strings.SplitSeq(changelog, "\n") {
		if strings.HasPrefix(line, "## ") && line != "## Unreleased" {
			return line == "## 11.0.0"
		}
	}
	return false
}

func TestV1100QualificationExceptionIsBoundToReleaseClientsAndEvidence(t *testing.T) {
	record, err := os.ReadFile("locked-clients/qualification.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, changelog string
		changeLock      bool
		changeEvidence  bool
		changePlatforms bool
		want            bool
	}{
		{name: "approved release", changelog: "## 11.0.0\n", want: true},
		{name: "reopened main", changelog: "## Unreleased\n\n## 11.0.0\n", want: true},
		{name: "previous release", changelog: "## 10.1.2\n"},
		{name: "next release", changelog: "## 11.0.1\n\n## 11.0.0\n"},
		{name: "no release", changelog: "## Unreleased\n"},
		{name: "changed dependency", changelog: "## 11.0.0\n", changeLock: true},
		{name: "changed evidence", changelog: "## 11.0.0\n", changeEvidence: true},
		{name: "missing platform", changelog: "## 11.0.0\n", changePlatforms: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lock, clients, err := QualifiedClientSet()
			if err != nil {
				t.Fatal(err)
			}
			if tc.changeLock {
				lock = strings.Repeat("0", 64)
			}
			if tc.changePlatforms {
				delete(clients, "linux/arm64")
			}
			evidence := append([]byte(nil), record...)
			if tc.changeEvidence {
				evidence = append(evidence, '\n')
			}
			if got := permitsV1100QualificationException(evidence, lock, clients, tc.changelog); got != tc.want {
				t.Fatalf("exception allowed=%t, want %t", got, tc.want)
			}
		})
	}
	lock, clients, err := QualifiedClientSet()
	if err != nil {
		t.Fatal(err)
	}
	for platform, lines := range clients {
		for i, line := range lines {
			t.Run(platform+"/"+line, func(t *testing.T) {
				lines[i] = line + " changed"
				defer func() { lines[i] = line }()
				if permitsV1100QualificationException(record, lock, clients, "## 11.0.0\n") {
					t.Fatal("changed native or adapter identity accepted")
				}
			})
		}
	}
	clients["linux/s390x"] = []string{"unexpected client"}
	if permitsV1100QualificationException(record, lock, clients, "## 11.0.0\n") {
		t.Fatal("additional platform accepted")
	}
}

func decodeQualification(data []byte, q *Qualification) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(q); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("qualification has trailing data")
	}
	return nil
}

func TestQualificationRequiresCompleteEvidence(t *testing.T) {
	lock, clients, err := QualifiedClientSet()
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := PinnedCLIVersions()
	if err != nil {
		t.Fatal(err)
	}
	valid := Qualification{Schema: QualificationSchema, QualifiedOn: "2026-09-22", Platform: "linux/arm64", Lock: lock, Clients: clients, Suites: map[string]map[string]string{}}
	for _, suite := range QualificationRequirements() {
		row := map[string]string{}
		for _, provider := range suite.Providers {
			row[provider] = pinned[provider]
			if suite.Evidence == QualificationPassedTest {
				row[provider] = "passed"
			}
		}
		valid.Suites[suite.Name] = row
	}
	valid.Suites["provider-accounts-live-e2e-all"]["grok"] = QualificationNotConfigured
	if err := ValidateQualification(valid, lock, clients); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Qualification){
		"old schema":       func(q *Qualification) { q.Schema = 1 },
		"empty date":       func(q *Qualification) { q.QualifiedOn = "" },
		"invalid date":     func(q *Qualification) { q.QualifiedOn = "2026-02-30" },
		"unknown platform": func(q *Qualification) { q.Platform = "linux/s390x" },
		"missing suite":    func(q *Qualification) { delete(q.Suites, "provider-delegate-live-e2e-all") },
		"extra suite":      func(q *Qualification) { q.Suites["invented"] = map[string]string{} },
		"missing provider": func(q *Qualification) { delete(q.Suites["provider-network-live-e2e-all"], "claude") },
		"extra provider":   func(q *Qualification) { q.Suites["mcp-e2e"]["claude"] = "passed" },
		"wrong CLI":        func(q *Qualification) { q.Suites["provider-live-e2e-all"]["codex"] = "codex-cli 0.0.0" },
		"unproven CLI":     func(q *Qualification) { q.Suites["provider-live-e2e-all"]["codex"] = "passed" },
		"unconfigured strict row": func(q *Qualification) {
			q.Suites["provider-network-live-e2e-all"]["claude"] = QualificationNotConfigured
		},
		"skipped account":       func(q *Qualification) { q.Suites["provider-accounts-live-e2e-all"]["grok"] = "skipped" },
		"stale client identity": func(q *Qualification) { q.Lock = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			var q Qualification
			if err := decodeQualification(data, &q); err != nil {
				t.Fatal(err)
			}
			mutate(&q)
			if err := ValidateQualification(q, lock, clients); err == nil {
				t.Fatal("incomplete or invalid qualification accepted")
			}
		})
	}
	for _, malformed := range []string{string(data) + "{}", strings.Replace(string(data), `"schema":2`, `"schema":2,"account":"private"`, 1)} {
		var q Qualification
		if err := decodeQualification([]byte(malformed), &q); err == nil {
			t.Fatal("unknown or trailing qualification data accepted")
		}
	}
}
