# Spinner frames must animate one recognizable object

A compact spinner's frames must read as successive states of the same object. Keep the object's
silhouette and visual vocabulary stable, and change one obvious property such as position,
orientation, or fill. Do not assemble a spinner from unrelated punctuation or symbols merely
because every frame has the same width.

**Why:** Pocket Run (`. > [ * ] >`) fit in one column, but looked like random characters rather
than motion. Corner Run (`◰ ◳ ◲ ◱`) stays one column while a filled corner travels clockwise
around one square, so the animation remains recognizable at a glance in a dense task list.

**How to apply:**
- Name the object and the motion in one sentence before accepting a frame sequence.
- Preview the cycle in a real terminal; a static list of code points is insufficient evidence.
- Pin the exact frame order and width in tests, including animated and frozen behavior.
- Keep surface-specific width contracts: compact rows may use a one-column companion while wider
  progress surfaces retain the full signature spinner.
