package shadowpath

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// otherHomePaths gives the other paths to the home folder at real, its real path, that are home.
// macOS keeps the folders people use on a data volume mounted at /System/Volumes/Data and joins them
// into / with firmlinks, which no path shows as links: /Users/me is also
// /System/Volumes/Data/Users/me, and a volume mounted under /Volumes or inside home is reached
// through the data volume too. So the mount point of each volume that holds home's real path or a
// folder above it, followed by real, through real folders, is such a path when it is home itself.
// Only those volumes are asked: one elsewhere, a stalled disk image say, is never touched.
func otherHomePaths(real string, home os.FileInfo) []string {
	parts := strings.Split(strings.TrimPrefix(real, "/"), "/")
	seen := map[string]bool{"/": true}
	var paths []string
	for dir := real; ; dir = filepath.Dir(dir) {
		var fs unix.Statfs_t
		if unix.Statfs(dir, &fs) == nil {
			point := unix.ByteSliceToString(fs.Mntonname[:])
			if !seen[point] && filepath.IsAbs(point) {
				seen[point] = true
				if folderThrough(point, parts, home) {
					paths = append(paths, filepath.Join(point, real))
				}
			}
		}
		if filepath.Dir(dir) == dir {
			return paths
		}
	}
}
