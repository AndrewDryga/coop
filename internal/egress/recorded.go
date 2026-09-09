package egress

import (
	"errors"
	"strings"
)

// ValidateRecorded checks only the bounded canonical shape of retained policy
// evidence. It does not authenticate a fingerprint or grant launch authority.
// Launchers must keep using Verify with the original owner key.
func (s Snapshot) ValidateRecorded() error {
	bad := errors.New("invalid retained network policy")
	hex := func(value string, size int) bool {
		return len(value) == size && strings.Trim(value, "0123456789abcdef") == ""
	}
	if s.Version != Version || !validScope(s.Scope) || !hex(s.KeyID, 16) || !hex(s.Fingerprint, 64) ||
		len(s.Grants) > MaxGrants || len(s.Dependencies) > MaxRules {
		return bad
	}
	if _, err := ParseMode(string(s.Mode)); err != nil || s.Mode != Filtered && len(s.Grants) != 0 {
		return bad
	}
	seen, previous, origins := map[string]bool{}, "", 0
	for _, grant := range s.Grants {
		rule, err := canonicalRule(grant.Rule)
		key := ruleKey(rule)
		if err != nil || rule.To.Provider != "" || key != ruleKey(grant.Rule) || key <= previous ||
			!hex(grant.ID, 32) || seen[grant.ID] || len(grant.Origins) == 0 {
			return bad
		}
		seen[grant.ID], previous = true, key
		previousOrigin := ""
		for _, origin := range grant.Origins {
			origins++
			key := originKey(origin)
			if origins > MaxGrantOrigins || !validOrigin(origin) || key <= previousOrigin {
				return bad
			}
			previousOrigin = key
		}
	}
	for i, dependency := range s.Dependencies {
		if !validLabel(dependency.Provider) || !validLabel(dependency.Backend) || !validLabel(dependency.AuthMode) ||
			(dependency.Client != ClientCLI && dependency.Client != ClientACP) ||
			len(dependency.Version) == 0 || len(dependency.Version) > 128 ||
			strings.IndexFunc(dependency.Version, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 ||
			i > 0 && compareDependencies(s.Dependencies[i-1], dependency) >= 0 {
			return bad
		}
	}
	return nil
}
