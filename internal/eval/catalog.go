package eval

// Starter is one shipped public suite: a pinned, qualified workload a user can run without authoring
// anything. The catalog is empty in v1 milestone 1 — a public starter only ships once its inputs,
// license, dependency pins and every isolation/retrieval control are qualified (a later milestone),
// and advertising one before then would be a promise the harness cannot yet keep.
type Starter struct {
	ID      string
	Summary string
	// Path is where the shipped manifest lives once starters ship; empty until then.
	Path string
}

// starters is the shipped catalog. It is deliberately empty: see the Starter doc.
var starters []Starter

// Starters returns the qualified public starter suites, in catalog order.
func Starters() []Starter { return append([]Starter(nil), starters...) }

// StarterPath resolves a bare starter id to its shipped manifest path. ok=false for anything not in
// the catalog, so the CLI can tell "unknown starter" from "a path to a custom suite".
func StarterPath(id string) (string, bool) {
	for _, s := range starters {
		if s.ID == id {
			return s.Path, true
		}
	}
	return "", false
}
