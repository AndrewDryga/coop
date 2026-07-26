# Agent-supervised long commands use static, bounded output

When an agent supervises a long loop, gate, watch, build, or test command, keep the command's
terminal output static and keep only a bounded excerpt in model context. This is an agent
execution rule, not a product change: human-run interactive commands keep their live UI.

**Why:** streaming a repainting progress view into an agent transcript repeats whole terminal
frames, consumes context without adding evidence, and can obscure the final exit status.

**How to apply:**
- Set `TERM=dumb`, `NO_COLOR=1`, and `COOP_SPINNER=0` where the command supports them.
- Redirect complete stdout and stderr to the task's `tmp/` directory or `/tmp`; preserve and
  report the command's real exit status.
- While it runs, poll for completion without streaming the log. Inspect only a bounded `tail`
  or a targeted `rg` filter, expanding narrowly when a failure needs more evidence.
- Do not dump the complete log into model context, pipe a live command through `tail`, or disable
  the interactive UI in product code to satisfy this supervision rule.

See also [[command-output-tiers]] and [[fix-the-bug-not-the-feature]].
