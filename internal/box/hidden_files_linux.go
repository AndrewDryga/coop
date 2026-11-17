package box

import (
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fsIocGetVersion is FS_IOC_GETVERSION, _IOR('v', 1, long), which x/sys does not name.
const fsIocGetVersion = 2<<30 | uint(unsafe.Sizeof(uintptr(0)))<<16 | 'v'<<8 | 1

// inodeGeneration is the generation number ext4, XFS and btrfs give an inode when it is created:
// a deleted file's inode number is reused at once, its generation is not, and a rename or an edit
// in place keeps it. A filesystem without one, or an object Coop does not open (only files and
// folders are opened, never a device or a pipe), reports false.
func inodeGeneration(path string, info fs.FileInfo) (uint32, bool) {
	if !info.Mode().IsRegular() && !info.IsDir() {
		return 0, false
	}
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, info) {
		return 0, false // replaced since it was looked at: its generation proves nothing about info
	}
	gen, err := unix.IoctlGetUint32(int(file.Fd()), fsIocGetVersion)
	return gen, err == nil
}
