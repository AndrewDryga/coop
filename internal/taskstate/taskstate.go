// Package taskstate holds the canonical names of the task-queue state directories — the one place
// the cli (which reads and moves tasks) and the scaffold (which creates the dirs on `coop init`)
// both import, so the two can never disagree. It depends on nothing, so either package can import
// it without a cycle (cli imports scaffold, so scaffold cannot import cli's constants directly —
// which is what let scaffold.go drift its own hardcoded copies before).
//
// Each name carries a two-digit sort-key prefix so a plain `ls .agent/tasks` lists the states in
// lifecycle order (todo → in_progress → blocked → done) instead of alphabetically; done uses "99_"
// so it always sorts last. A shell hook and the doc templates name these as literal strings (they
// can't import Go), so scaffold's drift-guard test pins those back to these values.
package taskstate

const (
	Todo       = "00_todo"
	InProgress = "10_in_progress"
	Blocked    = "50_blocked"
	Done       = "99_done"
)

// All is the state directories in lifecycle order — the list order the loop and `coop tasks list`
// follow, and the exact set `coop init` creates.
var All = []string{Todo, InProgress, Blocked, Done}
