package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// OrphanBox is one coop box, or a box's broker helper, whose supervising host process is provably
// gone: the pid it recorded is dead, or that pid now belongs to a different process than the one
// that launched it.
type OrphanBox struct {
	ID       string // container id
	PID      int    // the supervisor it recorded
	Evidence string // its LabelHost value — what the finding rests on
}

// OrphanSurvey is one scan of the coop boxes a runtime can see, classified for a single workspace.
type OrphanSurvey struct {
	Checked      int         // every coop box seen, running or stopped
	Orphans      []OrphanBox // in THIS workspace's scope AND provably dead: the only reapable boxes
	Brokers      []OrphanBox // credential broker helpers orphaned the same way, with or without their box
	Unattributed []string    // box ids coop cannot attribute (no supervisor label, or one it can't read)
}

// SurveyOrphanBoxes classifies every coop box, and every broker helper, for workspace without
// touching any of them.
//
// The judgment is the fork lifecycle's (internal/forkspace.OwnerProvablyDead), applied to an identity
// carried by the container instead of a pidfile: a box is an orphan only when its recorded
// supervisor's pid is gone, or that pid has been reused by a different process. Everything else is
// left alone — a live supervisor, an identity the kernel won't confirm, a box from another
// workspace, and a box launched before this label existed. Never age, never image, never name.
//
// A broker is judged the same way, on its own label, because it can outlive its box: when its coop
// is killed the box goes with --rm, while the native broker has no --rm and stays, stopped and
// holding its image. One listing of every container carrying LabelKey answers for both, so a
// survey that finds nothing still costs exactly one runtime call.
//
// A query failure is an error, never an empty survey: a runtime that cannot be asked what is
// running must not read as "nothing is running" to a caller that removes what it doesn't see.
func SurveyOrphanBoxes(ctx context.Context, rt runtime.Runtime, workspace string) (OrphanSurvey, error) {
	containers, err := rt.ContainerLabels(ctx, LabelKey, "", LabelKey, LabelHost, composeProjectLabel)
	if err != nil {
		return OrphanSurvey{}, err
	}
	scope := workspaceScope(workspace)
	var survey OrphanSurvey
	for _, container := range containers {
		kind := container.Labels[LabelKey]
		if kind != LabelBox && kind != LabelBroker {
			continue // a project service or a task channel: other sweeps judge those
		}
		host := container.Labels[LabelHost]
		ref, ok := parseSupervisorLabel(host)
		// Coop never puts a Compose label on a box or a broker. One that carries it is a development
		// service whose file planted coop's labels with a dead supervisor of this workspace, or a box
		// whose image carries the label; either way it is never removed.
		ok = ok && container.Labels[composeProjectLabel] == ""
		if kind == LabelBox {
			survey.Checked++
			if !ok {
				// No label (launched by a coop that predates it), one this version can't read, or a
				// Compose container. Coop does not know who owns the box, so it says so and keeps its
				// hands off.
				survey.Unattributed = append(survey.Unattributed, container.ID)
				continue
			}
		}
		if !ok || ref.scope != scope || !supervisorProvablyDead(ref.pid, ref.token) {
			continue
		}
		orphan := OrphanBox{ID: container.ID, PID: ref.pid, Evidence: host}
		if kind == LabelBox {
			survey.Orphans = append(survey.Orphans, orphan)
		} else {
			survey.Brokers = append(survey.Brokers, orphan)
		}
	}
	return survey, nil
}

// ReapOrphanBoxes removes exactly the boxes and broker helpers SurveyOrphanBoxes finds orphaned for
// workspace, and reports how many boxes the runtime removed: a broker is a helper of a box, not one
// the user started, so the line a launch prints counts boxes only.
//
// It removes by the orphan's own LabelHost value — the same exact-label reap `coop fork stop` uses
// — so the removal can only ever reach containers that carry the dead supervisor's identity AND
// this workspace's scope, even if the runtime's view changed since the survey.
func ReapOrphanBoxes(ctx context.Context, rt runtime.Runtime, workspace string) (int, error) {
	survey, err := SurveyOrphanBoxes(ctx, rt, workspace)
	if err != nil {
		return 0, err
	}
	removed, err := removeBySupervisor(ctx, rt, LabelBox, survey.Orphans)
	if err != nil {
		return removed, err
	}
	_, err = removeBySupervisor(ctx, rt, LabelBroker, survey.Brokers)
	return removed, err
}

// removeBySupervisor removes the containers of kind that each orphan's dead supervisor labeled, one
// exact-label call per supervisor, and reports how many went.
func removeBySupervisor(ctx context.Context, rt runtime.Runtime, kind string, orphans []OrphanBox) (int, error) {
	removed := 0
	for _, host := range supervisorLabelValues(orphans) {
		n, err := rt.RemoveByLabels(ctx, map[string]string{LabelKey: kind, LabelHost: host})
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

// runProject reports whether project is one of workspace's per-run projects (ComposeProjectFor with
// an owner). Coop's own override labels only those, so this is never the development project `coop
// up` runs, whose labels the project's file may set, however its checkout happens to be named.
func runProject(workspace, project string) bool {
	suffix, ok := strings.CutPrefix(project, ComposeProject(workspace)+"-run-")
	_, err := hex.DecodeString(suffix)
	return ok && len(suffix) == 8 && err == nil && suffix == strings.ToLower(suffix)
}

// ReapOrphanServices removes the per-run Compose services, and then their volumes, of every loop or
// session in workspace whose supervisor is provably dead, and reports how many containers it removed:
// a killed run leaves them behind, since only a clean exit tears them down. The proof is
// SurveyOrphanBoxes': the supervisor label coop's own override wrote (runServiceLabels), a scope of
// this workspace and the kernel's word that the pid is gone or reused. The project must also be one
// of this workspace's per-run ones, whose labels coop's override always replaces. A service with no
// provable label is left alone, and the network goes with ReapOrphanNetworks once nothing is
// attached.
func ReapOrphanServices(ctx context.Context, rt runtime.Runtime, workspace string) (int, error) {
	containers, err := rt.ContainerLabels(ctx, LabelKey, LabelService, LabelHost, composeProjectLabel)
	if err != nil {
		return 0, err
	}
	scope := workspaceScope(workspace)
	type run struct{ project, host string }
	var dead []run
	for _, container := range containers {
		project, host := container.Labels[composeProjectLabel], container.Labels[LabelHost]
		ref, ok := parseSupervisorLabel(host)
		if !ok || ref.scope != scope || !runProject(workspace, project) ||
			slices.Contains(dead, run{project, host}) || !supervisorProvablyDead(ref.pid, ref.token) {
			continue
		}
		dead = append(dead, run{project, host})
	}
	removed := 0
	var errs []error
	for _, r := range dead {
		n, err := rt.RemoveByLabels(ctx, map[string]string{LabelKey: LabelService, LabelHost: r.host, composeProjectLabel: r.project})
		removed += n
		if err == nil {
			_, err = rt.RemoveVolumesByLabel(ctx, composeProjectLabel, r.project)
		}
		errs = append(errs, err)
	}
	return removed, errors.Join(errs...)
}

// coopComposeProjectName matches the compose project names coop itself creates (ComposeProject:
// "coop-<basename>-<8 hex>", optionally with ComposeProjectFor's run suffix), so the network
// sweep can never reach a project a human composed.
var coopComposeProjectName = regexp.MustCompile(`^coop-[a-z0-9_-]*-[0-9a-f]{8}(-run-[0-9a-f]{8})?$`)

// ReapOrphanNetworks removes every network of a coop compose project that no container — running
// or stopped — is attached to, across all workspaces, and reports how many. These are what a
// session leaves behind when its containers were removed without a `compose down` (a crash, or a
// coop older than StopSessionServices' network cleanup), and Docker's default address pool holds
// only about thirty of them. Anyone else's compose networks are never inspected for removal.
func ReapOrphanNetworks(ctx context.Context, rt runtime.Runtime) (int, error) {
	ids, err := rt.NetworkIDs(ctx, "label="+composeProjectLabel)
	if err != nil {
		return 0, err
	}
	var ours []string
	for _, id := range ids {
		project, err := rt.NetworkLabel(ctx, id, composeProjectLabel)
		if err != nil {
			return 0, err
		}
		if coopComposeProjectName.MatchString(project) {
			ours = append(ours, id)
		}
	}
	return removeUnusedNetworks(ctx, rt, ours)
}
