package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// Builds up to v11.3.0 filed a project's approval (versions 1 to 3) and withdrawal (version 1) under
// an id keyed with owner.key, and wrote execution records at version 1. This build reads none of
// them. A withdrawal is a barrier, so a store that still holds one must never let a launch through
// that the withdrawal would have refused: oldRecordsFor finds every such file that could belong to a
// project, and projectIdentity, the start of every network operation on a project, refuses by name.
//
// The refusal is per project, the narrowest that stays closed. An approval or withdrawal is filed
// under a hash of its project's path, so with owner.key a file whose name is neither this project's
// plain id nor its keyed one provably belongs to some other project and is left alone. Without the
// key nothing can be proved, so any old-format approval or withdrawal refuses. An execution names
// its project, so it refuses only that project.

var oldFileRE = regexp.MustCompile(`^(approval|withdrawn)-([0-9a-f]{64})\.json$|^(execution)-[0-9a-f]{32}\.json$`)

// OldRecordsError refuses a project whose network records an older coop wrote.
type OldRecordsError struct {
	Project string
	Dir     string
	// Files are approvals and withdrawals; Runs are execution records.
	Files []string
	Runs  []string
	// anchors are the private links an old approval left; Forget removes them with it.
	anchors []string
}

func (e *OldRecordsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "coop will not start network work for %s: %s holds records an older coop wrote, which this coop no longer reads", e.Project, e.Dir)
	if len(e.Files) > 0 {
		fmt.Fprintf(&b, "\n  approval or withdrawal: %s", strings.Join(e.Files, ", "))
		fmt.Fprintf(&b, "\n  fix: run 'coop net forget --project %s', which removes them and keeps this project's launches refused until you run 'coop approve' there; if the project no longer exists, delete those files from %s instead", e.Project, e.Dir)
	}
	if len(e.Runs) > 0 {
		fmt.Fprintf(&b, "\n  run record: %s", strings.Join(e.Runs, ", "))
		fmt.Fprintf(&b, "\n  fix: open this project once with Coop v12.1.0, which cleans that run up; if the run is long gone, delete the file from %s", e.Dir)
	}
	return b.String()
}

// keyedProjectID is the id builds up to v11.3.0 filed a project's records under. It is only compared
// with a file's name to tell whose the file is, never used to read one; "" without the key.
func (s *Store) keyedProjectID(canonical string) string {
	if len(s.key) != 32 {
		return ""
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("project-v1\x00" + canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// oldRecordsFor lists the old-format records that could belong to the project at canonical, nil when
// there are none. Records it cannot read are not listed: the readers of the current format fail on
// those.
func (s *Store) oldRecordsFor(canonical string) (*OldRecordsError, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	slices.Sort(names)
	id, keyed := s.projectKey(canonical), s.keyedProjectID(canonical)
	found := &OldRecordsError{Project: canonical, Dir: s.path}
	for _, name := range names {
		if keyed != "" && strings.HasPrefix(name, "project-"+keyed+"-") {
			found.anchors = append(found.anchors, name)
		}
		match := oldFileRE.FindStringSubmatch(name)
		switch {
		case match == nil:
		case match[3] == "execution":
			if s.runIsOld(name, canonical) {
				found.Runs = append(found.Runs, name)
			}
		case keyed != "" && match[2] == keyed:
			found.Files = append(found.Files, name)
		case match[2] == id || keyed == "":
			// Its own name, or no key to prove whose it is: old when its version says so.
			current := withdrawalVersion
			if match[1] == "approval" {
				current = networkApprovalVersion
			}
			if head, ok := s.fileHead(name); ok && head.Version < current {
				found.Files = append(found.Files, name)
			}
		}
	}
	if len(found.Files) == 0 && len(found.Runs) == 0 {
		return nil, nil
	}
	return found, nil
}

type recordHead struct {
	Version int    `json:"version"`
	Project string `json:"project"`
}

func (s *Store) fileHead(name string) (recordHead, bool) {
	data, err := s.read(name)
	var head recordHead
	if err != nil || json.Unmarshal(data, &head) != nil {
		return recordHead{}, false
	}
	return head, true
}

// runIsOld reports whether an execution record is below the current version and could be this
// project's: it names its project, so one that provably names another is left alone.
func (s *Store) runIsOld(name, canonical string) bool {
	head, ok := s.fileHead(name)
	if !ok || head.Version >= ExecutionVersion {
		return false
	}
	if head.Project == canonical {
		return true
	}
	other, err := canonicalPath(head.Project)
	return err != nil || other == canonical
}

// refuseOldRecords is the refusal projectIdentity makes before any network operation on a project.
func (s *Store) refuseOldRecords(canonical string) error {
	found, err := s.oldRecordsFor(canonical)
	if err != nil {
		return err
	}
	if found != nil {
		return found
	}
	return nil
}
