package box

import "golang.org/x/sys/unix"

// Initial automatic qualification is deliberately limited to APFS. Other
// filesystems keep the existing protection backend until their lookup semantics
// and object identities have been qualified with the same live checks.
func filesystemProfileCandidate(root string) bool {
	var value unix.Statfs_t
	return unix.Statfs(root, &value) == nil && unix.ByteSliceToString(value.Fstypename[:]) == "apfs"
}

// Docker Desktop's VM kernel is qualified by the helper itself; the Mac has nothing to read.
func filesystemKernelReady() error { return nil }

// APFS case behaviour is a volume property, which filesystemVolumeProfile measures directly.
func filesystemMayFold(string) (bool, error) { return false, nil }
