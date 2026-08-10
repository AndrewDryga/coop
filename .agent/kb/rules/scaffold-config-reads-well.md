---
name: scaffold-config-reads-well
description: "scaffolded config leads every field with its comment and works as-is"
scope: scaffold
sources: [internal/preset/template.go, .agent/presets/frontier/preset.yaml]
check: "none"
updated: 2026-08-09
---

# Scaffolded, editable config reads top-down and works as-is

Files that `coop <x> init` writes for a human to open and edit (preset.yaml,
`.agent/fleet.yaml`, the prompt Markdown) follow two rules:

**1. Comments LEAD their field — never trail it.** Put the note on its own line(s)
*above* the field, not after it:

    # REQUIRED — one of: claude, codex, gemini.
    agent: claude

not `agent: claude   # REQUIRED — ...`. Trailing comments are harder to scan and
force width games. Separate items with newlines, not `·`/`|` inline separators — a
comment block has vertical room, so one item per line (e.g. `Run:` / `Inspect:` /
`Format:` labels, each on its own line) beats cramming several onto one with dots.

**1a. Document EVERY field inline — don't bury field docs in a section header.**
Each field gets its own leading comment, on the lead AND on every role. A section
header covers section-level facts only (what the map is, the name rule); it is not
where per-field docs go — a reader shouldn't have to scroll up to learn what a field
does. Repeating a short common-field note across sibling roles (`# agent — one of:
claude, codex, gemini.` on each) is fine and expected; a bare field is not. Keep
each comment to one concise line to hold down the cost, and fold a field's mode/kind
note into that field's comment (e.g. the "native subagent" explanation sits on
`mode:`) rather than a separate block, so nothing is said twice.

**2. Scaffolded content is a usable default, not a fill-in-the-blank.** What you
generate must stand on its own for a generic project — real how-to-work guidance,
a runnable recipe — not `<placeholder>` / "(edit me)" stubs the user MUST replace
before it works. Tuning is *recommended*, not required; a leading HTML/`#` note
says "sensible defaults; tune for yours, or delete this" and how to drop it.

**Why:** reviewing the preset scaffold, the user said the leading form is "easier
to read," and "the roles you generate should be good enough for generic projects,
not something you MUST fill in (even though it's recommended)."

**How to apply:**
- New `init` scaffold (or edit to `preset.Template` / `fleetTemplate`): leading
  comments, usable defaults, every line ≤ 80 display columns (count runes — the
  comments use `—·→…`, which are multi-byte).
- Terse REFERENCE illustrations are exempt — a `coop help` example is a compact
  cheat-sheet where trailing annotations and tight space are the point, not a file
  anyone edits.
- A committed dogfood copy (e.g. `.agent/presets/frontier/`) is HAND-CURATED from the
  template's starting shape, not regenerated: every role's actual agent/model and its
  rationale comments are real config a human chose, and they diverge from the
  template's generic defaults on purpose — no tooling should bind the two, since
  regenerating would silently overwrite that choice (evidence in the card's
  changelog). A drift test could still safely pin the shared STYLE the two must keep
  (leading comments, every field documented, the ≤80-rune cap) — never the values.

See [[scaffold-fits-the-repo]] (a scaffold suits the target repo) and
[[help-output-style]].

## Changelog
- 2026-07-03 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read internal/preset/template.go's `Template` const
  and .agent/presets/frontier/preset.yaml in full, plus a rune-length script over both. Confirmed
  clean: comments always LEAD their field in both files (0 trailing-comment hits), and every
  field in both is individually documented (no bare fields). 2 violations found: (1) line length
  — 22 lines in template.go's `Template` (up to 107 runes, e.g. line 13) and 13 lines in
  preset.yaml (up to 90 runes, e.g. line 17) exceed the stated 80-rune cap, in both of the rule's
  own cited sources. (2) the dogfood-regeneration claim doesn't match reality: no Makefile target
  or test regenerates .agent/presets/frontier/preset.yaml from the template (confirmed — no
  "regenerate"/frontier-scaffold hits anywhere in tools/ or a Makefile target), and the two have
  structurally diverged — every role's `agent:` differs (thinker: template's generic
  `claude:claude-opus-4-8/xhigh` @ `mode: native` vs frontier's actual
  `codex:gpt-5.6-terra/xhigh` @ `mode: consult`; critic: `codex:gpt-5.6-sol/xhigh` vs
  `grok:grok-4.5/high`; fast: `gemini:gemini-3.5-flash` vs `codex:gpt-5.6-luna/xhigh`), with
  frontier-specific rationale prose in its comments that no template regeneration could produce.
  frontier/preset.yaml reads as a deliberately hand-curated production config, not a
  regenerate-never-hand-edit copy — flagged possibly-wrong for the lead to reconcile (either
  build the missing regeneration tooling, or correct the card to describe frontier as
  hand-curated-but-format-matching instead). Neither violation fixed here.
- 2026-08-09 — fixed both violations above. (1) Rewrapped comments only (never a key,
  value, or wording) in both files: template.go's `Template` const, 22 lines → 0 over
  80 runes (was up to 107); frontier/preset.yaml, 13 lines → 0 over 80 runes (was up
  to 90) — verified with a rune-length script over both, plus a diff confirming every
  changed line starts with `#` (or is blank) so no key/value moved. The one table-shaped
  comment (lead's `agent:` example list) couldn't fit an aligned single-line-per-example
  layout within 80 runes at all (even zero padding overflows on the longer entries), so
  it's now one line per example, its description indented on the line below — same
  words, no aligned columns. (2) Corrected the claim per the lead's call: the
  divergence is deliberate, so the fix is the card, not new tooling. Confirmed from this
  repo's own history (`git log --follow -- .agent/presets/frontier/preset.yaml`): early
  commits (aee25b3, 3f4a149, 41165a1) touched template.go and frontier/preset.yaml
  together and called it "regenerated," but later ones (cdb11b0 "tune coop's own loop +
  frontier preset to the July 2026 frontier", 4dd055f, "two cross-vendor tiers at xhigh;
  grok as the outside critic") touch only frontier/preset.yaml, hand-tuning its actual
  models/vendors independently — the "regenerated" era ended once frontier's config
  started actively diverging. "How to apply" now says hand-curated-from-template's-
  starting-shape (via `preset.Scaffold`), never bound back to it, and names what a
  drift test could safely pin — the shared STYLE, not the values. Comment/prose drift
  spotted while rewrapping, not fixed (small, on-topic, flagged for the lead): (a)
  template.go has a blank line after `roles:` before the first role and between every
  role; frontier's `roles:` → `thinker:` is missing that one blank line (present
  between its other roles) — reads like a slip, not a choice. (b) frontier's `critic`
  role has no `prompt —` comment at all (thinker and fast both document that field, and
  template's critic documents it as an unset option) — its own section header two lines
  up promises "every field is documented below." (c) the `mode: consult` comment for
  critic is worded differently in each file (template: "a READ-ONLY peer for a second
  opinion (often another vendor), asked as coop-consult critic"; frontier: "a READ-ONLY
  peer via coop-consult for a second opinion") without an apparent frontier-specific
  reason the way the `agent:` rationale has one — possibly harmless copyediting, possibly
  drift.
