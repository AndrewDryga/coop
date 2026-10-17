package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const waypointCard = ".agent/kb/rules/one-upgrade-for-old-state.md"

// waypointConstants names, per row of the card's waypoint table, the constants holding that state's
// current and oldest-read versions, as "<file under internal/>:<name>". An empty oldest means no
// constant names it yet; the task that retires that reader adds one (it needs it for the refusal).
// An empty current means the versions are not integer constants: the box supervisor label's are the
// string prefixes "v2" and "v1" (box/sweep.go), so its row is listed but pins nothing.
var waypointConstants = map[string]struct{ current, oldest string }{
	"Session database schema":     {"session/records.go:SchemaVersion", "session/records.go:OldestSchemaVersion"},
	"Turn retry receipts":         {"session/operation_result.go:turnOperationResultVersion", ""},
	"Worker command receipts":     {"workerconnector/journal.go:journalVersion", "workerconnector/journal.go:journalVersion"},
	"Worker event streams":        {"workerconnector/event_streams.go:eventStreamVersion", "workerconnector/event_streams.go:eventStreamVersion"},
	"Worker create origins":       {"workerconnector/create_origins.go:createOriginVersion", "workerconnector/create_origins.go:createOriginVersion"},
	"Fork generation":             {"forkspace/generation.go:forkGenerationVersion", "forkspace/generation.go:forkGenerationLegacyVersion"},
	"Fork reservation":            {"forkspace/reservation.go:workspaceReservationVersion", ""},
	"Fork review candidate":       {"tasks/candidate_rounds.go:forkCandidateManifestVersion", "tasks/candidate.go:forkCandidateVersion"},
	"Task owner record":           {"tasks/owner.go:taskOwnershipRecordVersion", "tasks/owner.go:taskOwnerRecordVersion"},
	"Network approval":            {"networkstate/authority.go:networkApprovalVersion", "networkstate/authority.go:networkApprovalLegacyVersion"},
	"Isolated fork generation":    {"forkspace/generation.go:forkGenerationIsolatedVersion", ""},
	"Service approval":            {"box/serviceapproval.go:serviceApprovalVersion", "box/serviceapproval.go:serviceApprovalLegacyVersion"},
	"Workspace checkpoint bundle": {"workerproto/checkpoint.go:WorkspaceCheckpointVersion", "workerproto/checkpoint.go:WorkspaceCheckpointLegacyVersion"},
	"Box supervisor label":        {"", ""},
}

// TestOneUpgradeWaypoints is the check for waypointCard: its waypoint table must state the versions
// the code actually reads. A bumped or retired format whose row did not move with it fails here,
// and so does a row the test cannot pin or a pin whose row is gone.
func TestOneUpgradeWaypoints(t *testing.T) {
	rows := waypointRows(t)
	for _, state := range slices.Sorted(maps.Keys(waypointConstants)) {
		if _, ok := rows[state]; !ok {
			t.Errorf("%s has no row in the waypoint table of %s: restore it, or drop its pin here", state, waypointCard)
		}
	}
	for _, state := range slices.Sorted(maps.Keys(rows)) {
		pins, ok := waypointConstants[state]
		if !ok {
			t.Errorf("waypoint row %q in %s names no constants in waypointConstants (internal/oneupgrade_test.go)", state, waypointCard)
			continue
		}
		row := rows[state]
		for _, pin := range []struct{ column, cell, constant string }{
			{"current", row[0], pins.current}, {"oldest read", row[1], pins.oldest},
		} {
			if pin.constant == "" {
				continue
			}
			if want := strconv.Itoa(intConstant(t, pin.constant)); pin.cell != want {
				t.Errorf("%s in %s says %s version %q, but %s is %s: update the table in the same change "+
					"as the format", state, waypointCard, pin.column, pin.cell, pin.constant, want)
			}
		}
	}
}

// waypointRows reads the table under the card's "## Waypoints" heading into state → [current,
// oldest read], with Markdown code spans removed.
func waypointRows(t *testing.T) map[string][2]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", waypointCard))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "\n## Waypoints\n")
	if !found {
		t.Fatalf("%s has no \"## Waypoints\" section", waypointCard)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	rows := map[string][2]string{}
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if !strings.HasPrefix(strings.TrimSpace(line), "|") || len(cells) < 4 {
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(strings.ReplaceAll(cells[i], "`", ""))
		}
		if cells[0] == "State" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		if _, duplicate := rows[cells[0]]; duplicate {
			t.Errorf("%s lists %q twice", waypointCard, cells[0])
		}
		rows[cells[0]] = [2]string{cells[2], cells[3]}
	}
	if len(rows) == 0 {
		t.Fatalf("%s's waypoint table has no rows", waypointCard)
	}
	return rows
}

// intConstant evaluates "<file>:<name>", a constant declared with an integer literal.
func intConstant(t *testing.T, pin string) int {
	t.Helper()
	file, name, _ := strings.Cut(pin, ":")
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", pin, err)
	}
	for _, decl := range parsed.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value := spec.(*ast.ValueSpec)
			for i, ident := range value.Names {
				if ident.Name != name || i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.INT {
					t.Fatalf("%s is not declared with an integer literal", pin)
				}
				number, err := strconv.Atoi(literal.Value)
				if err != nil {
					t.Fatalf("%s: %v", pin, err)
				}
				return number
			}
		}
	}
	t.Fatalf("%s: no such constant; a renamed or removed format must update waypointConstants and %s", pin, waypointCard)
	return 0
}
