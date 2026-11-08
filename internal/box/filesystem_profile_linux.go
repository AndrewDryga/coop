package box

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"golang.org/x/sys/unix"
)

// ext4 and xfs keep an inode's identity across renames and hardlinks, which is what the
// kernel policy keys on, and neither folds or normalizes names unless ext4 casefolding is
// on (filesystemVolumeProfile detects that). Other filesystems keep mount-based protection
// until they are qualified the same way: btrfs subvolumes report a different device than the
// kernel sees, and overlayfs can change an inode on copy-up.
func filesystemProfileCandidate(root string) bool {
	var value unix.Statfs_t
	return unix.Statfs(root, &value) == nil && (value.Type == unix.EXT4_SUPER_MAGIC || value.Type == unix.XFS_SUPER_MAGIC)
}

var (
	//lint:ignore ST1005 it is a sentence for a person
	errFilesystemNoLSMList = errors.New("Kernel filesystem protection could not read the kernel's security modules")
	//lint:ignore ST1005 it is a sentence for a person, and BPF is a name
	errFilesystemNoBPFLSM = errors.New("Kernel filesystem protection needs the kernel's BPF LSM, which is off (add bpf to the lsm= boot parameter)")
	//lint:ignore ST1005 it is a sentence for a person, and BTF is a name
	errFilesystemNoBTF = errors.New("Kernel filesystem protection could not read the kernel's BTF type information")
	//lint:ignore ST1005 it is a sentence for a person
	errFilesystemOldKernel = errors.New("Kernel filesystem protection needs a newer Linux kernel with every file security hook it attaches to")
)

// kernelBTF reads the kernel's type information once per process; it is several megabytes and does
// not change while the kernel runs.
var kernelBTF = sync.OnceValues(func() ([]byte, error) { return os.ReadFile("/sys/kernel/btf/vmlinux") })

// filesystemKernelReady checks that the host kernel runs the BPF LSM the protection
// attaches to. It is on by default on Ubuntu 24.04 and other current distributions; a
// kernel without it keeps mount-based protection.
func filesystemKernelReady() error {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return errFilesystemNoLSMList
	}
	if !slices.Contains(strings.Split(strings.TrimSpace(string(data)), ","), "bpf") {
		return errFilesystemNoBPFLSM
	}
	// The helper refuses a kernel missing a hook, and that would fail the launch, so check first.
	// Each hook is a function in the kernel's type information, which any user may read; its name
	// sits NUL-terminated in the string table.
	btf, err := kernelBTF()
	if err != nil {
		return errFilesystemNoBTF
	}
	for _, hook := range fsfilter.HookNames() {
		if !bytes.Contains(btf, []byte("\x00bpf_lsm_"+hook+"\x00")) {
			return fmt.Errorf("%w (%s)", errFilesystemOldKernel, hook)
		}
	}
	return nil
}

// fsCasefoldFlag is FS_CASEFOLD_FL, which golang.org/x/sys does not name.
const fsCasefoldFlag = 0x40000000

// filesystemMayFold reports whether the volume holding private supports ext4 casefolding.
// Casefolding is set per directory and an agent could set it on a folder it made, so a
// volume that supports it is treated as folding and normalizing everywhere: the kernel
// policy then hides every spelling of a secret's name.
func filesystemMayFold(private string) (bool, error) {
	dir, err := os.MkdirTemp(private, "casefold-")
	if err != nil {
		return false, err
	}
	defer os.Remove(dir)
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		// xfs and others have no such flags; they never fold.
		return false, nil
	}
	return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(flags|fsCasefoldFlag)) == nil, nil
}
