# Docs: bold is for structure, not emphasis

In the README and prose docs, bold (`**…**`) marks STRUCTURE — table-group headers, the
label column of a table (e.g. the troubleshooting left column), numbered-step labels, the
one hero tagline, and a short paragraph lead-in label (`**Term.**` opening a
definition-style paragraph). It is NOT for mid-sentence emphasis: don't bold a word or
phrase to stress it, and don't bold `inline code` (the monospace already sets it apart).

**Why:** scattered bold reads as shouting and stops being a signal — when a third of the
prose is bold, none of it stands out. Reserving bold for structure keeps it a scanning
aid. (The README had grown to ~100 bold spans, most of them inline emphasis.)

**How to apply:**
- Want to stress a point? Rephrase so the sentence carries it, or leave it plain; at most
  an occasional *italic*.
- A bold lead-in label that opens a paragraph as a sub-heading is fine; a bold phrase
  buried mid-sentence is not.
- Never bold inline code spans.
- Smell check: `grep -oE '\*\*[^*]+\*\*' README.md | wc -l` should be in the dozens, and
  nearly every hit should be a header, a table cell, a step, or a lead-in label — not
  prose emphasis.
