package box

import (
	"fmt"
	"os"
	"runtime"
)

// agentIdentity is selected by the host, never by a repository or a box. Docker
// Desktop does not share the macOS host's UID space with Linux containers.
type agentIdentity struct{ uid, gid int }

func boxAgentIdentity() agentIdentity {
	if runtime.GOOS == "linux" && os.Getuid() != 0 {
		return agentIdentity{os.Getuid(), os.Getgid()}
	}
	return agentIdentity{1000, 1000}
}

func (id agentIdentity) user() string { return fmt.Sprintf("%d:%d", id.uid, id.gid) }

func (id agentIdentity) valid() bool {
	// 65532 is the gateway's separate socket/egress identity. Its overlap would
	// let an agent match the gateway's outbound firewall rules.
	return id.uid > 0 && id.gid > 0 && id.uid != 65532 && id.uid <= 1<<31-1 && id.gid <= 1<<31-1
}

func (id agentIdentity) volume(name string) string {
	if id.uid == 1000 && id.gid == 1000 {
		return name
	}
	return name + "-" + fmt.Sprint(id.uid) + "-" + fmt.Sprint(id.gid)
}

func (id agentIdentity) projectImage(name string) string {
	if id.uid == 1000 && id.gid == 1000 {
		return name
	}
	return fmt.Sprintf("%s-u%dg%d", name, id.uid, id.gid)
}
