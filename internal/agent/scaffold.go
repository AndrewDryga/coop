package agent

import "io/fs"

// ScaffoldSpec is data only: scaffold owns all writes and no-clobber checks.
// An empty Project.Dir means the provider is not selectable by coop init.
type ScaffoldSpec struct {
	Project, Fallback            ScaffoldLayout
	EstablishedSkills            string
	KnowledgeIgnore              []string // within the shared knowledge stanza, after presets
	AlwaysIgnore, SelectedIgnore []string // complete standalone stanzas
	CommitGate                   *ScaffoldCommitGate
}

type ScaffoldLayout struct {
	Dir            string
	Dirs           []string // additional repo-relative directories
	Files          []ScaffoldFile
	CommitGatePath string
}

type ScaffoldFile struct {
	Path, Template string
	Mode           fs.FileMode
}

// The adapter owns the native tool-call envelope; scaffold inserts the detected
// stack's format checks between Prefix and Suffix with the provider's failure code.
type ScaffoldCommitGate struct {
	Prefix, Suffix, FailureCode string
}

func Scaffoldable() []string {
	var out []string
	for _, name := range Names() {
		ag, _ := Get(name)
		if ag.Scaffold().Project.Dir != "" {
			out = append(out, name)
		}
	}
	return out
}

// EstablishedSkillsSources follows the shared .agent/skills source when adopting
// an older real project skill tree. It is independent of the selected providers.
func EstablishedSkillsSources() []string {
	var out []string
	for _, name := range Names() {
		ag, _ := Get(name)
		if path := ag.Scaffold().EstablishedSkills; path != "" {
			out = append(out, path)
		}
	}
	return out
}
