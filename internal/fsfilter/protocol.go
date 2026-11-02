package fsfilter

import (
	"errors"
	"regexp"
	"strings"
)

const ControlPath = "/.coop-fs"

const ProbeContent = "coop filesystem admission fixture\n"

type ShareProbe struct {
	Denied   []string `json:"denied"`
	Readable []string `json:"readable"`
}

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

// Birth is durable before the first LSM attachment. Program identities are known
// after load, so one immutable receipt also reconciles a partially attached run.
type Birth struct {
	Parent             string `json:"parent"`
	BootID             string `json:"boot_id"`
	CgroupInode        uint64 `json:"cgroup_inode"`
	CgroupDevice       uint64 `json:"cgroup_device"`
	PinDirectoryInode  uint64 `json:"pin_directory_inode"`
	PinDirectoryDevice uint64 `json:"pin_directory_device"`
	Programs           []Link `json:"programs"`
}

func (b Birth) Validate(parent string) error {
	if b.Parent != "/"+parent || !ValidParent(parent) || !ValidBootID(b.BootID) || b.CgroupInode == 0 || b.CgroupDevice == 0 || b.PinDirectoryInode == 0 || b.PinDirectoryDevice == 0 || len(b.Programs) != len(hookNames) {
		return errors.New("invalid filesystem birth receipt")
	}
	wanted := map[string]bool{}
	for _, name := range hookNames {
		wanted["guard_"+name] = true
	}
	for _, program := range b.Programs {
		if !wanted[program.Name] || program.ID != 0 || program.Program == 0 {
			return errors.New("invalid filesystem program receipt")
		}
		delete(wanted, program.Name)
	}
	return nil
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
	"inode_set_acl", "inode_remove_acl", "inode_file_setattr", "inode_file_getattr",
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
	if r.Status != "ready" || r.Parent != "/"+parent || !ValidParent(parent) || !ValidBootID(r.BootID) || r.CgroupInode == 0 || r.Programs != len(hookNames) || r.Pins < 1 || r.Pins > MaxPaths || len(r.Links) != len(hookNames) {
		return errors.New("invalid filesystem admission receipt")
	}
	wanted := make(map[string]bool, len(hookNames))
	for _, name := range hookNames {
		wanted["guard_"+name] = true
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
