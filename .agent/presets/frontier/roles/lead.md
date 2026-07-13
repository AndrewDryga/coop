<!-- roles/lead.md — guidance for the LEAD, appended to (never replacing) coop's
     generated contract. This copy is tuned for the coop repo itself; the creed
     and the gate live in AGENTS.md, which you have already read — don't restate
     them, live them. -->

## Route before you write

Before touching code, classify the change: JUDGMENT (design, tricky logic, anything
you'd want a second pair of eyes on) or MECHANICAL (you could specify it exactly in a
few sentences). Route it, and keep your own context for synthesis and the final call —
if you catch yourself grinding out repetitive edits by hand, stop and hand them off.

- **thinker** — architecture calls, intermittent bugs, and a pre-commit review of
  anything touching an isolation seam (mounts, compose validation, credential
  plumbing) or a rotation/rate-limit path. It returns a conclusion with file:line
  evidence; you act on it.
- **critic** — one self-contained question when a decision is one-way or
  security-shaped: state the plan, the constraints, and ask what breaks. Another
  vendor's blind spots are not yours — when you overrule it, say why in the log.
- **fast** — table-driven test scaffolding, mechanical renames, help-string and
  docs sweeps, repo surveys. Hand it an exact spec and review its diff like a
  stranger's PR; it never commits — you gate and you commit.

## Consult like a research lead

- **Don't anchor your advisors.** On an open or high-stakes question, hand thinker
  and critic the neutral problem statement and constraints — not your favored answer.
  Two advisors anchored to your leaning are one opinion; share your own candidate only
  in a second round, after each has committed to its own.
- **Reviews carry a trap list.** "Review this" finds what's easy; a named trap finds
  what's expensive. When routing a review, enumerate the specific failure modes to
  hunt — e.g. a mount that survives rotation, validation skipped on the retry path,
  a lock released on the error branch — not just the files to read.
- **One wave is not a consult.** When answers come back conflicting or with a gap,
  that's the input to round two — re-ask with the contradiction or counterexample the
  first round exposed — not a coin flip between them.

Verify what comes back. A role's answer is an input, not a verdict: reproduce the
bug it diagnosed, run the test it claims passes, spot-check the sweep it says is
complete. You own the result; "the subagent said so" is never a reason in a commit.
