# Small discovered work goes to the queue, not the backlog

When you spot a SEPARATE task while working — not part of the one you're on — where it lands depends
on its size, not its topicality. A simple, ready fix goes to the QUEUE (`00_todo/`, via
`coop tasks add`) so the loop works it soon. Only work that's genuinely big or not-yet-ready — needs a
spec, a decision, or real scoping — goes to the BACKLOG (`coop backlog add`). The backlog is NOT a
dumping ground for small stuff.

**Why:** the backlog bloated to 21 items because agents parked every discovered task there, including
trivial ones ("delete this dead function", "this help line has a stray `·`"). Nothing in the loop ever
touches the backlog, so those quick fixes languished for weeks while the drawer grew too long to read —
and the human had to hand-triage it back into the queue. A task you can state an acceptance for in one
line IS ready; the queue is where ready work belongs, and the loop drains it. Reserving the backlog for
the big/unready keeps it a short, meaningful list of things that actually need a human's planning.

**How to apply:**
- Trivial + safe + on-topic → just fix it inline (boy-scout); don't create a task at all.
- A separate fix you can write a one-line acceptance for → the QUEUE. On the host: `coop tasks add`.
  In a box (no `coop`): create the folder in `00_todo/<id>/` with a `task.md` stating that acceptance.
- Only if it needs a spec, a decision, or real scoping (you CAN'T state its acceptance in a line yet)
  → the BACKLOG (`coop backlog add`). It waits there until someone fleshes it into a spec + promotes it.
- Never fold the discovered fix into the current task's commit — one task = one commit.

Related: [[fix-the-bug-not-the-feature]].
