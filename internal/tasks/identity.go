package tasks

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const (
	queueIdentityVersion = 1
	taskIdentityVersion  = 1
	queueIdentityLockID  = "\x00queue-identity"

	// QueueIdentityFile and TaskIdentityFile travel with the Markdown authority. A projected task
	// carries the same identities, so the host can reject a folder that was replaced or redirected
	// without treating its human-readable slug as authority.
	QueueIdentityFile = ".coop-queue.json"
	TaskIdentityFile  = ".coop-task.json"
)

var durableIdentityRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

type queueIdentityRecord struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

type taskIdentityRecord struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskRef is the stable identity of one canonical Markdown task. ID remains the readable folder
// slug; QueueID and TaskID are random durable identities and are the authority carried by fork
// assignments, projections, candidates, and land intents.
type TaskRef struct {
	QueueID string `json:"queue_id"`
	TaskID  string `json:"task_id"`
	ID      string `json:"id"`
}

// TaskGeneration fences delete-and-recreate ABA at the filesystem boundary. The durable TaskID is
// the logical identity; device+inode prove that the folder currently answering its path is still
// the exact instance an assignment captured.
type TaskGeneration struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type TaskInstance struct {
	Ref        TaskRef        `json:"task_ref"`
	Generation TaskGeneration `json:"task_generation"`
}

func newDurableIdentity() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func decodeIdentityRecord(data []byte, out any) error {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), taskMetadataFileLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("identity record contains multiple JSON values")
		}
		return err
	}
	return nil
}

func validateIdentity(version int, id string, createdAt time.Time) error {
	if version != 1 || !durableIdentityRE.MatchString(id) || createdAt.IsZero() {
		return errors.New("invalid durable task identity")
	}
	return nil
}

func canonicalTaskRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("task queue %q is not a real directory", root)
	}
	return filepath.Clean(abs), nil
}

func readQueueIdentity(root string) (queueIdentityRecord, error) {
	canonical, err := canonicalTaskRoot(root)
	if err != nil {
		return queueIdentityRecord{}, err
	}
	opened, err := OpenTaskMetadataRoot(canonical)
	if err != nil {
		return queueIdentityRecord{}, err
	}
	defer opened.Close()
	data, err := ReadTaskMetadataFile(opened, QueueIdentityFile)
	if err != nil {
		return queueIdentityRecord{}, err
	}
	var record queueIdentityRecord
	if err := decodeIdentityRecord(data, &record); err != nil {
		return queueIdentityRecord{}, err
	}
	if err := validateIdentity(record.Version, record.ID, record.CreatedAt); err != nil {
		return queueIdentityRecord{}, err
	}
	return record, nil
}

// EnsureQueueIdentity returns the queue's durable identity, creating it atomically when this is a
// legacy queue first entering an ownership transition. Read-only views never call this function.
func EnsureQueueIdentity(root string) (string, error) {
	canonical, err := canonicalTaskRoot(root)
	if err != nil {
		return "", err
	}
	// Atomic rename makes the file durable, but it does not make two different temporary
	// identities agree: concurrent first claims could each return the value they wrote while only
	// the last rename remains authoritative. Serialize the legacy-to-identified transition on the
	// same host registry used for task authority, then re-read under that lock.
	authority, err := lockLeaseAuthority(canonical, queueIdentityLockID, true, syscall.LOCK_EX)
	if err != nil {
		return "", err
	}
	defer unlockLeaseFile(authority)
	if record, err := readQueueIdentity(canonical); err == nil {
		return record.ID, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := newDurableIdentity()
	if err != nil {
		return "", err
	}
	record := queueIdentityRecord{Version: queueIdentityVersion, ID: id, CreatedAt: time.Now().UTC()}
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	opened, err := OpenTaskMetadataRoot(canonical)
	if err != nil {
		return "", err
	}
	defer opened.Close()
	if _, err := opened.Lstat(QueueIdentityFile); err == nil {
		record, readErr := readQueueIdentity(canonical)
		return record.ID, readErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := AtomicWriteTaskFile(opened, QueueIdentityFile, append(body, '\n')); err != nil {
		return "", err
	}
	return id, nil
}

func readTaskIdentity(taskDir string) (taskIdentityRecord, error) {
	opened, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return taskIdentityRecord{}, err
	}
	defer opened.Close()
	data, err := ReadTaskMetadataFile(opened, TaskIdentityFile)
	if err != nil {
		return taskIdentityRecord{}, err
	}
	var record taskIdentityRecord
	if err := decodeIdentityRecord(data, &record); err != nil {
		return taskIdentityRecord{}, err
	}
	if err := validateIdentity(record.Version, record.ID, record.CreatedAt); err != nil {
		return taskIdentityRecord{}, err
	}
	return record, nil
}

func ensureTaskIdentity(taskDir string) (string, error) {
	if record, err := readTaskIdentity(taskDir); err == nil {
		return record.ID, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := newDurableIdentity()
	if err != nil {
		return "", err
	}
	record := taskIdentityRecord{Version: taskIdentityVersion, ID: id, CreatedAt: time.Now().UTC()}
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	opened, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return "", err
	}
	defer opened.Close()
	if _, err := opened.Lstat(TaskIdentityFile); err == nil {
		record, readErr := readTaskIdentity(taskDir)
		return record.ID, readErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := AtomicWriteTaskFile(opened, TaskIdentityFile, append(body, '\n')); err != nil {
		return "", err
	}
	return id, nil
}

func taskFolderGeneration(taskDir string) (TaskGeneration, error) {
	info, err := os.Lstat(taskDir)
	if err != nil {
		return TaskGeneration{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return TaskGeneration{}, fmt.Errorf("task folder %q is not a real directory", taskDir)
	}
	return TaskGeneration{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

// EnsureTaskInstance establishes and returns the canonical logical and filesystem identity for
// item. Callers serialize it with the task owner mutation lock before using it as authority.
func EnsureTaskInstance(root string, item Item) (TaskInstance, error) {
	current, ok := CurrentTask(root, item.ID)
	if !ok || current.State != item.State || current.Dir != item.Dir {
		return TaskInstance{}, errors.New("task changed while establishing its identity")
	}
	queueID, err := EnsureQueueIdentity(root)
	if err != nil {
		return TaskInstance{}, fmt.Errorf("ensure queue identity: %w", err)
	}
	taskID, err := ensureTaskIdentity(item.Dir)
	if err != nil {
		return TaskInstance{}, fmt.Errorf("ensure task identity: %w", err)
	}
	generation, err := taskFolderGeneration(item.Dir)
	if err != nil {
		return TaskInstance{}, err
	}
	return TaskInstance{
		Ref:        TaskRef{QueueID: queueID, TaskID: taskID, ID: item.ID},
		Generation: generation,
	}, nil
}

// ReadTaskInstance is the non-mutating counterpart used by snapshot and fencing checks.
func ReadTaskInstance(root string, item Item) (TaskInstance, error) {
	queue, err := readQueueIdentity(root)
	if err != nil {
		return TaskInstance{}, err
	}
	task, err := readTaskIdentity(item.Dir)
	if err != nil {
		return TaskInstance{}, err
	}
	generation, err := taskFolderGeneration(item.Dir)
	if err != nil {
		return TaskInstance{}, err
	}
	return TaskInstance{
		Ref:        TaskRef{QueueID: queue.ID, TaskID: task.ID, ID: item.ID},
		Generation: generation,
	}, nil
}

func sameTaskInstance(a, b TaskInstance) bool {
	return a.Ref == b.Ref && a.Generation == b.Generation
}
