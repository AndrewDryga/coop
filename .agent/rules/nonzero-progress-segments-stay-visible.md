# Nonzero live progress segments must stay visible

When a fixed-width state bar represents a positive live or attention-required count, reserve at
least one cell for that segment whenever the bar can fit the protected states. Exact proportional
rounding is secondary to showing that work is active or blocked.

**Why:** a queue with one in-progress task showed a yellow counter and marker but no yellow bar
segment because its proportional share rounded to zero. The bar visually denied live work that the
same row reported in text.

**How to apply:**
- Floor positive in-progress and blocked segments to one cell before allocating completed work.
- Define the impossible-width priority explicitly; blocked keeps priority over active because a
  hidden human decision can make an unfinished queue look complete.
- Keep the total display width fixed by taking rounding overflow from completed work first.
- Sweep every caller of a shared bar helper; a visibility guarantee is only real when each surface
  supplies the state count instead of a placeholder zero.
- Pin proportional, rounded-to-zero, combined protected-state, and tiny-width cases with distinct
  color sentinels, then verify at least one real colored terminal rendering.
