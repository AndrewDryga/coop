// Package taskstate holds the canonical names of the task-queue state directories — the one place
// the cli (which reads and moves tasks) and the scaffold (which creates the dirs on `coop init`)
// both import, so the two can never disagree. It depends on nothing, so either package can import
// it without a cycle (cli imports scaffold, so scaffold cannot import cli's constants directly —
// which is what let scaffold.go drift its own hardcoded copies before).
//
// Each name carries a two-digit sort-key prefix so a plain `ls .agent/tasks` lists the states in
// lifecycle order (todo → in_progress → blocked → done) instead of alphabetically; done uses "99_"
// so it always sorts last, and Backlog uses "xx_" so it sorts after everything (it's off to the
// side, not part of the flow). A shell hook and the doc templates name these as literal strings
// (they can't import Go), so scaffold's drift-guard test pins those back to these values.
package taskstate

const (
	Todo       = "00_todo"
	InProgress = "10_in_progress"
	Blocked    = "50_blocked"
	Done       = "99_done"

	// Backlog holds unscheduled ideas as task folders (`coop backlog`). It is DELIBERATELY absent
	// from All: every counter and lister walks All, while loops and the sweep Stop hook key off the
	// actionable Todo plus InProgress states,
	// so keeping Backlog out of it means all of them ignore xx_backlog for free — it's a staging
	// drawer, not a fifth lifecycle state. Only the `coop backlog` commands read it directly.
	Backlog = "xx_backlog"
)

// All is the LIFECYCLE state directories in order — the list the loop and `coop tasks ls` follow,
// and the exact set `coop init` creates. Backlog is intentionally NOT here (see its doc); a test
// pins that so no one "helpfully" adds it and makes the loop start working un-promoted ideas.
var All = []string{Todo, InProgress, Blocked, Done}
