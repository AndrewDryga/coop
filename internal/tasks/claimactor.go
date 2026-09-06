package tasks

import (
	"strings"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

// ClaimActor is the process a claim is bound to and the label the queue shows for it. A zero PID
// means the claim is bound to nothing — a person's claim, which only that person releases.
type ClaimActor struct {
	Label      string
	PID        int
	StartToken string
}

// claimActorProbe is the process-tree reader captureClaimActor walks; tests inject a fake tree.
type claimActorProbe struct {
	parent  func(int) int
	command func(int) string
	token   func(int) string
}

var realClaimActorProbe = claimActorProbe{
	parent: processidentity.Parent, command: processidentity.Command, token: processidentity.StartToken,
}

// shellNames are the interpreters an agent runtime wraps a tool call in. None of them outlives the
// call, so a claim bound to one would read "gone" the moment the command returned.
var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true, "ash": true, "busybox": true,
}

func isShellCommand(name string) bool {
	return shellNames[strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "-")] // a login shell reports "-zsh"
}

// captureClaimActor binds a claim to the process that made it: coop's parent, or the nearest
// non-shell ancestor when the parent is a shell (an IDE agent runs every tool call in a short-lived
// `sh -c`, so the shell is never the durable owner). An interactive claim — coop's stdin is a
// terminal — is a person at a keyboard and is bound to nothing: their claim must survive the
// terminal, and pre-flight must never release it. An explicit pid always binds. An identity whose
// start token cannot be read is dropped rather than guessed; the claim then reserves the task for
// the label alone, exactly as every claim did before claims carried an identity.
func captureClaimActor(probe claimActorProbe, start int, interactive bool, override ClaimActor) ClaimActor {
	actor := ClaimActor{Label: override.Label}
	pid := override.PID
	if pid == 0 && !interactive {
		pid = start
		for depth := 0; depth < 4 && pid > 1 && isShellCommand(probe.command(pid)); depth++ {
			parent := probe.parent(pid)
			if parent <= 1 {
				break
			}
			pid = parent
		}
	}
	if pid > 1 {
		if token := probe.token(pid); processidentity.Stable(token) {
			actor.PID, actor.StartToken = pid, token
			if actor.Label == "" {
				actor.Label = probe.command(pid)
			}
		}
	}
	actor.Label = claimActorLabel(actor.Label)
	return actor
}

// claimActorLabel keeps a label to what a queue row can show verbatim — letters, digits, and
// `-_@.:` (so "codex@zed" and "claude:opus" survive), at most 32 runes; anything else is dropped.
func claimActorLabel(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 32 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("-_@.:", r) {
			return ""
		}
	}
	return s
}

// ownerProcessLive reports whether a process-bound claim's owner still exists. Unknown counts as
// live: a claim is only ever released on proof of death, never on a failure to prove life.
func ownerProcessLive(record TaskOwnerRecord) bool {
	if record.ActorPID == 0 {
		return true
	}
	switch processidentity.Inspect(record.ActorPID, record.ActorStart) {
	case processidentity.Gone, processidentity.Mismatch:
		return false
	}
	return true
}
