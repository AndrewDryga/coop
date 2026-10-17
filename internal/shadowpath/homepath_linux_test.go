package shadowpath

import (
	"path/filepath"
	"slices"
	"testing"
)

// Fields are split on single spaces only: the kernel escapes a space, tab, newline and backslash in a
// path, and any other character, a no-break space say, belongs to it. A line whose root is not a
// path, as an nsfs mount's is, is left out.
func TestParseMountinfoReadsDevicesRootsAndEscapedPoints(t *testing.T) {
	data := "22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw\n" +
		"40 22 252:1 /srv/data\\040disk /mnt/my\\040disk\\134x rw shared:2 - ext4 /dev/vda1 rw\n" +
		"41 22 252:1 /srv/a\u00a0b /mnt/a\u00a0b rw shared:3 - ext4 /dev/vda1 rw\n" +
		"42 22 0:4 net:[4026531840] /run/netns/x rw shared:4 - nsfs nsfs rw\n" +
		"\n" +
		"short line\n"
	want := []mount{{"252:1", "/", "/"}, {"252:1", "/srv/data disk", `/mnt/my disk\x`}, {"252:1", "/srv/a\u00a0b", "/mnt/a\u00a0b"}}
	if got := parseMountinfo([]byte(data)); !slices.Equal(got, want) {
		t.Errorf("parseMountinfo = %q, want %q", got, want)
	}
}

func TestMountedPathsFollowsHomesFilesystemToItsOtherMounts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		real   string
		mounts []mount
		want   []string
	}{{
		// An ostree system on ext4 or xfs: the whole root filesystem at /sysroot, one folder of it at /var.
		name: "a folder of the filesystem mounted over another",
		real: "/var/home/me",
		mounts: []mount{
			{"0:30", "/ostree/deploy/fedora/deploy/abc.0", "/"},
			{"0:30", "/", "/sysroot"},
			{"0:30", "/ostree/deploy/fedora/var", "/var"},
			{"0:31", "/", "/tmp"},
		},
		want: []string{"/sysroot/ostree/deploy/fedora/var/home/me"},
	}, {
		name:   "a bind mount under the same name",
		real:   "/home/me",
		mounts: []mount{{"8:1", "/", "/"}, {"8:2", "/", "/mnt/data"}, {"8:2", "/home", "/home"}},
		want:   []string{"/mnt/data/home/me"},
	}, {
		name:   "a bind mount under another name",
		real:   "/home/me",
		mounts: []mount{{"8:1", "/", "/"}, {"8:2", "/", "/mnt/data"}, {"8:2", "/homes", "/home"}},
		want:   []string{"/mnt/data/homes/me"},
	}, {
		name:   "a mount of a folder inside home holds no path to home",
		real:   "/home/me",
		mounts: []mount{{"8:1", "/", "/"}, {"8:1", "/home/me/work", "/srv/work"}},
		want:   nil,
	}, {
		name:   "the last of stacked mounts holds home",
		real:   "/home/me",
		mounts: []mount{{"8:1", "/", "/"}, {"8:2", "/", "/home"}, {"8:3", "/", "/home"}, {"8:3", "/", "/mnt/three"}, {"8:2", "/", "/mnt/two"}},
		want:   []string{"/mnt/three/me"},
	}, {
		name:   "no other mount of home's filesystem",
		real:   "/home/me",
		mounts: []mount{{"8:1", "/", "/"}, {"8:2", "/", "/mnt/data"}},
		want:   nil,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, m := range mountedPaths(tc.real, tc.mounts) {
				got = append(got, filepath.Join(m.point, m.root))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("mountedPaths(%s) = %q, want %q", tc.real, got, tc.want)
			}
		})
	}
}
