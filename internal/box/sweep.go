package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// supervisorLabelVersion prefixes every LabelHost value. The label is a contract between a coop
// that launched a box and a LATER, possibly different, coop that decides whether to remove it, so
// the shape it was written in has to be readable from the value itself. An unrecognized version
// reads as unattributable — reported, never reaped.
const supervisorLabelVersion = "v1"

// OrphanBox is one coop box whose supervising host process is provably gone: the pid it recorded is
// dead, or that pid now belongs to a different process than the one that launched the box.
type OrphanBox struct {
	ID       string // container id
	PID      int    // the supervisor it recorded
	Evidence string // its LabelHost value — what the finding rests on, printed by `coop doctor`
}

// OrphanSurvey is one scan of the coop boxes a runtime can see, classified for a single workspace.
type OrphanSurvey struct {
	Checked      int         // every coop box seen, running or stopped
	Orphans      []OrphanBox // in THIS workspace's scope AND provably dead: the only reapable set
	Unattributed []string    // ids coop cannot attribute (no supervisor label, or one it can't read)
}

// SurveyOrphanBoxes classifies every coop box for workspace without touching any of them.
//
// The judgment is the fork lifecycle's (internal/cli.ownerProvablyDead), applied to an identity
// carried by the container instead of a pidfile: a box is an orphan only when its recorded
// supervisor's pid is gone, or that pid has been reused by a different process. Everything else is
// left alone — a live supervisor, an identity the kernel won't confirm, a box from another
// workspace, and a box launched before this label existed. Never age, never image, never name.
//
// A query failure is an error, never an empty survey: a runtime that cannot be asked what is
// running must not read as "nothing is running" to a caller that removes what it doesn't see.
func SurveyOrphanBoxes(ctx context.Context, rt runtime.Runtime, workspace string) (OrphanSurvey, error) {
	containers, err := rt.ContainersByLabel(ctx, LabelKey, LabelBox)
	if err != nil {
		return OrphanSurvey{}, err
	}
	scope := workspaceScope(workspace)
	survey := OrphanSurvey{Checked: len(containers)}
	for _, container := range containers {
		host := container.Labels[LabelHost]
		ref, ok := parseSupervisorLabel(host)
		if !ok {
			// No label (launched by a coop that predates it) or one this version can't read. Either
			// way coop does not know who owns the box, so it says so and keeps its hands off.
			survey.Unattributed = append(survey.Unattributed, container.ID)
			continue
		}
		if ref.scope != scope || !supervisorProvablyDead(ref.pid, ref.token) {
			continue
		}
		survey.Orphans = append(survey.Orphans, OrphanBox{ID: container.ID, PID: ref.pid, Evidence: host})
	}
	return survey, nil
}

// ReapOrphanBoxes removes exactly the boxes SurveyOrphanBoxes finds orphaned for workspace, and
// reports how many the runtime removed.
//
// It removes by the orphan's own LabelHost value — the same exact-label reap `coop fork stop` uses
// — so the removal can only ever reach containers that carry the dead supervisor's identity AND
// this workspace's scope, even if the runtime's view changed since the survey.
func ReapOrphanBoxes(ctx context.Context, rt runtime.Runtime, workspace string) (int, error) {
	survey, err := SurveyOrphanBoxes(ctx, rt, workspace)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, host := range supervisorLabelValues(survey.Orphans) {
		n, err := rt.RemoveByLabels(ctx, map[string]string{LabelKey: LabelBox, LabelHost: host})
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// supervisorLabelValues is the orphans' distinct supervisor labels in the order first seen — one
// dead supervisor may own several boxes, and each label removes all of its own in one call.
func supervisorLabelValues(orphans []OrphanBox) []string {
	seen := make(map[string]bool, len(orphans))
	values := make([]string, 0, len(orphans))
	for _, orphan := range orphans {
		if seen[orphan.Evidence] {
			continue
		}
		seen[orphan.Evidence] = true
		values = append(values, orphan.Evidence)
	}
	return values
}

// supervisorLabelValue is the LabelHost value for pid supervising a box in scope. It returns "" when
// the host cannot produce a stable kernel identity for pid: without one, no later run could ever
// prove the process dead, so the box is better left unlabeled (and thus untouchable) than labeled
// with something unverifiable.
func supervisorLabelValue(scope string, pid int) string {
	token := processidentity.StartToken(pid)
	if pid <= 1 || scope == "" || !processidentity.Stable(token) {
		return ""
	}
	return fmt.Sprintf("%s:%s:%d:%s", supervisorLabelVersion, scope, pid, token)
}

// supervisorRef is a parsed LabelHost value.
type supervisorRef struct {
	scope string
	pid   int
	token string
}

// parseSupervisorLabel reads a LabelHost value back. ok is false for anything this version cannot
// fully verify — no label, another version, a malformed field, or a start token whose shape it does
// not recognize — and every such box is reported rather than reaped. The token itself contains
// colons, so it takes the whole remainder of the value.
func parseSupervisorLabel(value string) (supervisorRef, bool) {
	parts := strings.SplitN(value, ":", 4)
	if len(parts) != 4 || parts[0] != supervisorLabelVersion || parts[1] == "" {
		return supervisorRef{}, false
	}
	pid, err := strconv.Atoi(parts[2])
	if err != nil || pid <= 1 || !processidentity.Stable(parts[3]) {
		return supervisorRef{}, false
	}
	return supervisorRef{scope: parts[1], pid: pid, token: parts[3]}, true
}

// supervisorProvablyDead is the one test that authorizes removing a box: the kernel says the
// recorded pid is gone, or that pid now belongs to a different process than the one that launched
// the box. A live match and — deliberately — an identity coop could not read are both unproven, so
// both fail closed. Identity is pid + start token or nothing: no container age, no elapsed time.
func supervisorProvablyDead(pid int, token string) bool {
	switch processidentity.Inspect(pid, token) {
	case processidentity.Gone, processidentity.Mismatch:
		return true
	default:
		return false
	}
}

// supervisorScope is the workspace a box belongs to for sweeping: the trusted policy repo when the
// run has one, else the mounted workspace. A review/gate box mounts a disposable candidate tree that
// no later invocation could ever scan, so its orphan is scoped to the durable repo whose run
// launched it.
func supervisorScope(spec RunSpec) string {
	if spec.PolicyRepo != "" {
		return workspaceScope(spec.PolicyRepo)
	}
	return workspaceScope(spec.Repo)
}

// workspaceScope identifies one checkout, so a sweep can never select a box another checkout owns.
// Canonical (symlinks resolved) like ComposeProject's per-workspace names, so the same physical
// workspace always yields the same scope — a path spelled two ways is still one workspace.
func workspaceScope(workspace string) string {
	if workspace == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(canonicalWorkspace(workspace)))
	return hex.EncodeToString(sum[:12])
}
