package fsfilter

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

const ControlPath = "/.coop-fs"

type Admission struct {
	Roots []Policy `json:"roots"`
}

type ExecutionManifest struct {
	BootID      string   `json:"boot_id"`
	Command     []string `json:"command"`
	Healthcheck []string `json:"healthcheck"`
}

type Link struct {
	Name    string `json:"name"`
	ID      uint32 `json:"id"`
	Program uint32 `json:"program"`
}

type Ready struct {
	Controller  string         `json:"controller,omitempty"`
	Token       uint64         `json:"token,omitempty"`
	Maps        []KernelObject `json:"maps,omitempty"`
	Status      string         `json:"status"`
	Parent      string         `json:"parent"`
	BootID      string         `json:"boot_id"`
	CgroupInode uint64         `json:"cgroup_inode"`
	Programs    int            `json:"programs"`
	Pins        int            `json:"pins"`
	Links       []Link         `json:"links"`
}

var parentName = regexp.MustCompile(`^coop-fs-[0-9a-f]{32}$`)
var bootID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidParent(name string) bool { return parentName.MatchString(name) }
func ValidBootID(id string) bool   { return bootID.MatchString(id) }

var hookNames = []string{
	"file_open", "file_permission", "file_truncate", "file_receive", "mmap_file", "file_mprotect",
	"inode_setattr", "inode_unlink", "inode_rmdir", "inode_link", "inode_rename", "inode_create",
	"inode_mkdir", "inode_symlink", "inode_mknod", "inode_follow_link", "inode_readlink", "inode_getattr",
	"inode_getxattr", "inode_listxattr", "inode_setxattr", "inode_removexattr", "inode_get_acl",
	"inode_set_acl", "inode_remove_acl", "inode_file_setattr", "inode_file_getattr", "unix_stream_connect",
	"unix_may_send",
}

// HookNames are the kernel's BPF LSM hooks the filter attaches to, without their bpf_lsm_ prefix.
// A kernel must have every one; the newest, inode_file_setattr and inode_file_getattr, arrived in
// recent kernels, so an older one keeps mount-based protection.
func HookNames() []string { return slices.Clone(hookNames) }

// hookGenerations are the lengths of hookNames that released helpers attached: 13.0.0 attached the
// first 27. New hooks are only appended, so every generation is a prefix of hookNames. A record an
// earlier release wrote must still read, so that its own image can retire it; a launch reuses only a
// controller of its own image.
var hookGenerations = []int{27, len(hookNames)}

// guardGeneration returns the guard program names of the generation that attached n hooks, or nil
// when no release attached n.
func guardGeneration(n int) map[string]bool {
	if !slices.Contains(hookGenerations, n) {
		return nil
	}
	guards := make(map[string]bool, n)
	for _, name := range hookNames[:n] {
		guards["guard_"+name] = true
	}
	return guards
}

func (r Ready) Validate(parent string) error {
	if r.Controller != "" {
		if r.Status != "ready" || r.Parent != "/"+parent || !ValidParent(parent) || !ValidController(r.Controller) || !ValidBootID(r.BootID) || r.CgroupInode == 0 || r.Token == 0 || r.Pins < 1 || r.Pins > MaxPaths || r.Programs != 0 || len(r.Links) != 0 || !exactKernelObjects(r.Maps, []string{"objects", "paths", "devices", "config"}) {
			return errors.New("invalid shared filesystem admission receipt")
		}
		return nil
	}
	if r.Token != 0 || len(r.Maps) != 0 {
		return errors.New("unexpected shared admission objects")
	}
	if r.Status != "ready" || r.Parent != "/"+parent || !ValidParent(parent) || !ValidBootID(r.BootID) || r.CgroupInode == 0 || r.Programs != len(r.Links) || r.Pins < 1 || r.Pins > MaxPaths {
		return errors.New("invalid filesystem admission receipt")
	}
	wanted := guardGeneration(len(r.Links))
	if wanted == nil {
		return errors.New("invalid filesystem admission receipt")
	}
	seenIDs := make(map[uint32]bool, len(r.Links))
	for _, link := range r.Links {
		if !wanted[link.Name] || seenIDs[link.ID] || link.ID == 0 || link.Program == 0 || strings.ContainsAny(link.Name, "/:") {
			return errors.New("invalid filesystem attachment receipt")
		}
		delete(wanted, link.Name)
		seenIDs[link.ID] = true
	}
	return nil
}
