package workerconnector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

var ErrCommandConflict = errors.New("worker command identity conflicts with its durable receipt")

const journalVersion = 1

type journal struct {
	dir     string
	streams string
	origins string
	// Fault injection for the rename-visible / directory-not-yet-durable crash window.
	testSyncActivityDir func(string) error
}

type journalEntry struct {
	Version       int                        `json:"version"`
	CommandID     string                     `json:"command_id"`
	CommandDigest string                     `json:"command_digest"`
	Command       *workerproto.Command       `json:"command"`
	State         string                     `json:"state"`
	Result        *workerproto.CommandResult `json:"result"`
}

type commandIdentity struct {
	CommandID           string          `json:"command_id"`
	WorkerID            string          `json:"worker_id"`
	SessionRef          string          `json:"session_ref"`
	PlacementGeneration int             `json:"placement_generation"`
	LeaseRef            string          `json:"lease_ref"`
	Kind                string          `json:"kind"`
	CommandVersion      int             `json:"command_version"`
	Payload             json.RawMessage `json:"payload"`
	IdempotencyKey      string          `json:"idempotency_key"`
}

func openJournal(dir string) (*journal, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, errors.New("worker command journal needs an absolute directory")
	}
	commands := filepath.Join(dir, "commands")
	streams := filepath.Join(dir, "event-streams")
	origins := filepath.Join(dir, "create-origins")
	if err := os.MkdirAll(origins, 0o700); err != nil {
		return nil, fmt.Errorf("create worker origin journal: %w", err)
	}
	if err := os.Chmod(origins, 0o700); err != nil {
		return nil, fmt.Errorf("protect worker origin journal: %w", err)
	}
	if err := os.MkdirAll(commands, 0o700); err != nil {
		return nil, fmt.Errorf("create worker command journal: %w", err)
	}
	if err := os.MkdirAll(streams, 0o700); err != nil {
		return nil, fmt.Errorf("create worker event stream journal: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect worker command journal: %w", err)
	}
	if err := os.Chmod(commands, 0o700); err != nil {
		return nil, fmt.Errorf("protect worker command receipt directory: %w", err)
	}
	if err := os.Chmod(streams, 0o700); err != nil {
		return nil, fmt.Errorf("protect worker event stream directory: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, err // persist new child journal directories before any receipt uses them
	}
	return &journal{dir: commands, streams: streams, origins: origins}, nil
}

func (j *journal) begin(command workerproto.Command) (journalEntry, error) {
	digest, err := commandDigest(command)
	if err != nil {
		return journalEntry{}, err
	}
	path := j.path(command.CommandID)
	entry, err := j.read(path)
	if err == nil {
		if entry.CommandDigest != digest || entry.CommandID != command.CommandID {
			return journalEntry{}, ErrCommandConflict
		}
		return entry, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return journalEntry{}, err
	}

	entry = journalEntry{
		Version: journalVersion, CommandID: command.CommandID, CommandDigest: digest, Command: &command, State: "received",
	}
	encoded, err := encodeWireJSON(entry)
	if err != nil {
		return journalEntry{}, fmt.Errorf("encode worker command receipt: %w", err)
	}
	// Write the receipt beside its final name and publish it with an exclusive hard link: a crash
	// mid-write then leaves an unnamed temp file, never a truncated receipt that every later poll
	// would fail to decode and so never poll again. The link keeps the create-exclusive semantics —
	// a sibling that published first wins and is re-read above.
	temporary, err := os.CreateTemp(j.dir, ".command-receipt-*")
	if err != nil {
		return journalEntry{}, fmt.Errorf("create worker command receipt: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return journalEntry{}, fmt.Errorf("protect worker command receipt: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return journalEntry{}, fmt.Errorf("write worker command receipt: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return journalEntry{}, fmt.Errorf("sync worker command receipt: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return journalEntry{}, fmt.Errorf("close worker command receipt: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return j.begin(command)
		}
		return journalEntry{}, fmt.Errorf("publish worker command receipt: %w", err)
	}
	if err := syncDir(j.dir); err != nil {
		return journalEntry{}, err
	}
	return entry, nil
}

func (j *journal) complete(entry journalEntry, result workerproto.CommandResult) (journalEntry, error) {
	if entry.State == "completed" {
		if entry.Result != nil && sameResult(*entry.Result, result) {
			return entry, nil
		}
		return journalEntry{}, ErrCommandConflict
	}
	entry.State = "completed"
	entry.Result = &result
	encoded, err := encodeWireJSON(entry)
	if err != nil {
		return journalEntry{}, fmt.Errorf("encode completed worker command receipt: %w", err)
	}
	path := j.path(entry.CommandID)
	temporary, err := os.CreateTemp(j.dir, ".command-result-*")
	if err != nil {
		return journalEntry{}, fmt.Errorf("create worker command result: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return journalEntry{}, fmt.Errorf("protect worker command result: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return journalEntry{}, fmt.Errorf("write worker command result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return journalEntry{}, fmt.Errorf("sync worker command result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return journalEntry{}, fmt.Errorf("close worker command result: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return journalEntry{}, fmt.Errorf("publish worker command result: %w", err)
	}
	if err := syncDir(j.dir); err != nil {
		return journalEntry{}, err
	}
	return entry, nil
}

func (j *journal) read(path string) (journalEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return journalEntry{}, err
	}
	var entry journalEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return journalEntry{}, fmt.Errorf("decode worker command receipt: %w", err)
	}
	if entry.Version != journalVersion || entry.CommandID == "" || entry.CommandDigest == "" || entry.Command == nil ||
		(entry.State != "received" && entry.State != "completed") ||
		(entry.State == "received" && entry.Result != nil) ||
		(entry.State == "completed" && entry.Result == nil) {
		return journalEntry{}, errors.New("worker command receipt is malformed")
	}
	digest, err := commandDigest(*entry.Command)
	if err != nil || entry.Command.CommandID != entry.CommandID || digest != entry.CommandDigest {
		return journalEntry{}, errors.New("worker command receipt identity is malformed")
	}
	return entry, nil
}

func (j *journal) pending() ([]journalEntry, error) {
	files, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, fmt.Errorf("list worker command receipts: %w", err)
	}
	entries := make([]journalEntry, 0, len(files))
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		entry, err := j.read(filepath.Join(j.dir, file.Name()))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].CommandID < entries[right].CommandID })
	return entries, nil
}

func (j *journal) acknowledgeResults(commandIDs []string) error {
	var failures error
	for _, commandID := range commandIDs {
		path := j.path(commandID)
		entry, err := j.read(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if entry.State != "completed" || entry.Result == nil {
			return fmt.Errorf("acknowledge incomplete worker command %s", commandID)
		}
		if err := j.preserveCreateOrigin(entry); err != nil {
			failures = errors.Join(failures, fmt.Errorf("preserve worker create %s before acknowledgement: %w", commandID, err))
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove acknowledged worker command result: %w", err)
		}
	}
	if len(commandIDs) > 0 {
		return errors.Join(failures, syncDir(j.dir))
	}
	return failures
}

func (j *journal) path(commandID string) string {
	digest := sha256.Sum256([]byte(commandID))
	return filepath.Join(j.dir, hex.EncodeToString(digest[:])+".json")
}

func commandDigest(command workerproto.Command) (string, error) {
	identity := commandIdentity{
		CommandID: command.CommandID, WorkerID: command.WorkerID, SessionRef: command.SessionRef,
		PlacementGeneration: command.PlacementGeneration, LeaseRef: command.LeaseRef, Kind: command.Kind,
		CommandVersion: command.CommandVersion, Payload: command.Payload, IdempotencyKey: command.IdempotencyKey,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode worker command identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func sameResult(left, right workerproto.CommandResult) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open worker command journal directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync worker command journal directory: %w", err)
	}
	return nil
}
