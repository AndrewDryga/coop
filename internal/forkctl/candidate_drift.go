package forkctl

import (
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// candidateDrift is how a fork's branch moved after its final review. A merge lands only the exact
// reviewed commit. A loop reviews new commits on top of it again, but a branch that no longer builds
// on it (a person rebased or amended in the fork) is one neither a merge nor a loop takes.
type candidateDrift int

const (
	driftNone candidateDrift = iota
	driftAhead
	driftDiverged
)

// forkCandidateDrift compares the fork's HEAD with its reviewed candidate. A HEAD Coop cannot read
// is no drift: whatever reads it next says what is wrong.
func forkCandidateDrift(ws string, candidate tasks.ForkCandidate, head string) candidateDrift {
	if head == candidate.Head || !forkspace.ValidObjectID(head) {
		return driftNone
	}
	if _, err := forkspace.GitOutErr(ws, "merge-base", "--is-ancestor", candidate.Head, head); err == nil {
		return driftAhead
	}
	return driftDiverged
}

// candidateDriftError is the refusal for a fork whose branch moved after its final review, with the
// ways on that work today. Going back is a hard reset of the fork's branch, so it names the commit
// the fork's reflog keeps.
func candidateDriftError(ws, name string, candidate tasks.ForkCandidate, head string, drift candidateDrift, merging bool) error {
	back := shellLine([]string{"git", "-C", ws, "reset", "--hard", candidate.Head})
	discard := [2]string{"Or discard its work:", "coop fork rm " + name + " --force"}
	tail := ""
	if merging {
		tail = " Nothing was merged."
	}
	if drift == driftAhead {
		return ui.CommandFailed("Fork "+name+" has new commits since its final review",
			"Coop merges only the reviewed commit, so the new commits need a review first."+tail,
			[2]string{"Review them:", "coop fork " + name + " --loop"},
			[2]string{"Or go back to the reviewed commit:", back}, discard)
	}
	return ui.CommandFailed("Fork "+name+" changed after its final review",
		"Its branch is at "+head[:12]+", which does not build on the reviewed commit "+candidate.Head[:12]+
			", so Coop can neither merge it nor review it again."+tail+
			" Going back keeps "+head[:12]+" in the fork's reflog.",
		[2]string{"Go back to the reviewed commit:", back},
		[2]string{"Then merge it:", "coop fork merge " + name},
		discard)
}

// RefuseDivergedFork refuses a loop on a fork whose branch no longer builds on its reviewed
// candidate. The loop would stop at its first step, after a background start said "Started", so the
// refusal comes in the terminal, with the same ways on as coop fork merge gives.
func RefuseDivergedFork(repo string, identity forkspace.Identity) error {
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		return err
	}
	ws := forkspace.Workspace(repo, identity.Name)
	head := forkspace.GitOut(ws, "rev-parse", "HEAD")
	if forkCandidateDrift(ws, candidate, head) != driftDiverged {
		return nil
	}
	return candidateDriftError(ws, identity.Name, candidate, head, driftDiverged, false)
}
