package box

import "io/fs"

// inodeGeneration has no counterpart a user process can read on macOS (st_gen reads as 0 unless
// root), so a remembered file there proves itself with its size and modification time.
func inodeGeneration(string, fs.FileInfo) (uint32, bool) { return 0, false }
