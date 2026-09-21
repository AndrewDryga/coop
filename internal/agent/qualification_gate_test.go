package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gate this repo is missing: a pin cannot move without re-running the paid conformance suites.
//
// The COMPARISON is tested here and now, against synthetic records, so the logic a future record
// will be judged by is itself proven rather than landing untested alongside it. Only pointing it at
// the real file waits for an operator to run `make provider-qualify`.
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

// The gate itself. It is DORMANT until an operator runs `make provider-qualify` and commits the
// record — deliberately: forcing the record's existence would block every ordinary `make check` on a
// paid step. The moment the file lands, this begins failing whenever the client set drifts from it,
// with no further wiring.
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
	if err := json.Unmarshal(data, &q); err != nil {
		t.Fatalf("the committed qualification does not parse: %v", err)
	}
	lock, clients, err := QualifiedClientSet()
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := QualificationMismatch(q, lock, clients); mismatch != "" {
		t.Fatalf("the committed qualification no longer describes this binary's clients: %s", mismatch)
	}
}
