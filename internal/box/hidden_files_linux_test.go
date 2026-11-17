package box

import "testing"

// FS_IOC_GETVERSION is _IOR('v', 1, long); Coop ships only 64-bit Linux, where it is 0x80087601.
func TestTheInodeGenerationRequestIsTheKernels(t *testing.T) {
	if fsIocGetVersion != 0x80087601 {
		t.Fatalf("FS_IOC_GETVERSION = %#x, want 0x80087601", fsIocGetVersion)
	}
}
