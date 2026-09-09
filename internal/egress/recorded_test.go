package egress

import (
	"slices"
	"strings"
	"testing"
)

func TestRecordedPolicyValidationDoesNotAuthenticateAuthority(t *testing.T) {
	snapshot, err := Compile("recorded", Filtered, []Input{{Rules: []Rule{tlsRule("a.example.com"), tlsRule("*.example.net")}, Origin: Origin{Kind: "operator"}}}, nil, false, ownerKey())
	if err != nil || snapshot.ValidateRecorded() != nil {
		t.Fatal("valid captured policy rejected", err)
	}
	changed := snapshot.Clone()
	changed.Fingerprint = strings.Repeat("a", 64)
	if changed.ValidateRecorded() != nil || changed.Verify(ownerKey()) == nil {
		t.Fatal("shape validation became authentication")
	}
	if snapshot.Verify(nil) == nil {
		t.Fatal("keyless launch authentication succeeded")
	}
	for name, mutate := range map[string]func(*Snapshot){
		"fingerprint":       func(s *Snapshot) { s.Fingerprint = "../elsewhere" },
		"key-id":            func(s *Snapshot) { s.KeyID = "not-a-key" },
		"mode":              func(s *Snapshot) { s.Mode = Open },
		"rule-order":        func(s *Snapshot) { slices.Reverse(s.Grants) },
		"duplicate-id":      func(s *Snapshot) { s.Grants[1].ID = s.Grants[0].ID },
		"noncanonical":      func(s *Snapshot) { s.Grants[0].Rule.Ports = []int{443, 443} },
		"provenance":        func(s *Snapshot) { s.Grants[0].Origins[0].Name = "unsafe\x1b[31m" },
		"origin-duplicate":  func(s *Snapshot) { s.Grants[0].Origins = append(s.Grants[0].Origins, s.Grants[0].Origins[0]) },
		"provider-selector": func(s *Snapshot) { s.Grants[0].Rule = Rule{To: Destination{Provider: "anthropic"}} },
	} {
		t.Run(name, func(t *testing.T) {
			copy := snapshot.Clone()
			mutate(&copy)
			if copy.ValidateRecorded() == nil {
				t.Fatal("malformed retained policy accepted")
			}
		})
	}
}
