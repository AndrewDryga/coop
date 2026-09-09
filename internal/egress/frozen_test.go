package egress

import "testing"

func TestFrozenWildcardSurvivesSuffixPolicyChangesWithoutAdmittingNewWildcard(t *testing.T) {
	// Model a previously accepted suffix which today's catalog prohibits. The
	// fixture signs the historical bytes, not a newly compiled public request.
	rule := tlsRule("*.github.io")
	snapshot, err := Compile("test", Filtered, nil, nil, false, ownerKey())
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Grants = []Grant{{ID: keyed(ownerKey(), "rule", []byte(ruleKey(rule)))[:32], Rule: rule, Origins: []Origin{{Kind: "operator"}}}}
	snapshot.Fingerprint = snapshot.fingerprint(ownerKey())
	if err := snapshot.Verify(ownerKey()); err != nil {
		t.Fatal("current suffix catalog invalidated authenticated historical bytes", err)
	}
	if !snapshot.Domain("project.github.io", 443).Allowed || !Covers(rule, tlsRule("project.github.io")) {
		t.Fatal("historical wildcard lost its exact approved scope")
	}
	if _, err := NormalizeRules([]Rule{rule}); err == nil {
		t.Fatal("historical normalization leaked into current authoring")
	}
	if got, err := Compile("test", Filtered, []Input{{Rules: []Rule{rule}, Origin: Origin{Kind: "operator"}}}, nil, false, ownerKey()); err == nil || got.Fingerprint != "" {
		t.Fatal("new capture admitted a currently prohibited wildcard", err)
	}
	if Covers(rule, rule) || Covers(rule, tlsRule("deep.project.github.io")) {
		t.Fatal("historical envelope widened current authoring or one-label scope")
	}
	snapshot.Grants[0].Rule.To.Domain = "*.other.example.com"
	if err := snapshot.Verify(ownerKey()); err == nil {
		t.Fatal("historical normalization bypassed the fingerprint")
	}
}
