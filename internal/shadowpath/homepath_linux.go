package shadowpath

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// otherHomePaths gives the other paths to the home folder at real, its real path, that Linux's mount
// table offers and that are home. A filesystem can be mounted at several places, whole or one folder
// of it (a bind mount): with /data/home bound at /home, /home/me is also /data/home/me. Each path
// counts only through real folders, as on macOS. NFS reports / as the root of every mount, so a
// second mount of the export that holds home is not seen.
func otherHomePaths(real string, home os.FileInfo) []string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	var paths []string
	for _, m := range mountedPaths(real, parseMountinfo(data)) {
		if folderThrough(m.point, strings.Split(m.root, "/"), home) {
			paths = append(paths, filepath.Join(m.point, m.root))
		}
	}
	return paths
}

// A mount is one line of the mount table: the folder root of the filesystem on device, mounted at
// point.
type mount struct{ device, root, point string }

// parseMountinfo reads /proc/self/mountinfo, whose lines read
// "36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw": the third field is the device,
// the fourth the folder of the filesystem that is mounted, the fifth where. Fields are separated by
// single spaces, and only a space, tab, newline or backslash in a path is written as an octal escape,
// so any other character, a no-break space say, belongs to the path. A line whose root or mount point
// is not an absolute path (an nsfs mount's "net:[4026531840]") is left out.
func parseMountinfo(data []byte) []mount {
	var mounts []mount
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.Split(line, " ")
		if len(f) < 5 {
			continue
		}
		m := mount{device: f[2], root: unescapeMountPath(f[3]), point: unescapeMountPath(f[4])}
		if filepath.IsAbs(m.root) && filepath.IsAbs(m.point) {
			mounts = append(mounts, m)
		}
	}
	return mounts
}

func unescapeMountPath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountedPaths gives the other mounts that reach real, each with the rest of the path below its mount
// point in root. real lies under the mount whose point is the longest that holds it, the last such
// when mounts are stacked, at a path inside that filesystem; every other mount of the same
// filesystem whose root holds that path reaches it too.
func mountedPaths(real string, mounts []mount) []mount {
	holder := -1
	for i, m := range mounts {
		if holds(m.point, real) && (holder < 0 || len(m.point) >= len(mounts[holder].point)) {
			holder = i
		}
	}
	if holder < 0 {
		return nil
	}
	h := mounts[holder]
	inside := filepath.Join(h.root, relative(h.point, real))
	var paths []mount
	for i, m := range mounts {
		if i != holder && m.device == h.device && holds(m.root, inside) {
			paths = append(paths, mount{device: m.device, root: relative(m.root, inside), point: m.point})
		}
	}
	return paths
}

// holds reports whether path is the folder dir or below it.
func holds(dir, path string) bool {
	return dir == "/" || path == dir || strings.HasPrefix(path, dir+"/")
}

// relative is path below dir, which holds it.
func relative(dir, path string) string {
	rel, _ := filepath.Rel(dir, path)
	return rel
}
