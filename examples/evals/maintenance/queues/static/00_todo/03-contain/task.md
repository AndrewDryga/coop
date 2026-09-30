# Keep every request inside the document root

Deny traversal, including encoded `..` segments, and symlinks whose real target is outside the document root. Never return outside-root bytes. Keep legitimate nested files and earlier HTTP behavior. Add visible denial regressions.

Not done: a lexical prefix check that lets a symlink escape, or hiding the problem by deleting ordinary file serving.
