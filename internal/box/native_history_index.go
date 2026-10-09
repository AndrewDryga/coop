package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

// Native indexes combine multiple retained account homes. Receipts remember rows,
// not whole source files: a previously live session can become eligible later,
// while a native deletion must not be undone by a second source or a retry.
type nativeIndexReceipt struct {
	Version       int
	Path          string
	Keys          []string
	Before, After nativeHistoryReceipt
	Absent, Done  bool
}

func nativeIndexPrefix(path string) string {
	hash := sha256.Sum256([]byte(path))
	return "index-" + hex.EncodeToString(hash[:]) + "-"
}

func nativeIndexReceipts(root *os.File, path string) (map[string]nativeIndexReceipt, error) {
	dir, err := safefile.CloneDir(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	out := map[string]nativeIndexReceipt{}
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), nativeIndexPrefix(path)) || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			file, err := safefile.OpenRegular(root, entry.Name())
			if err != nil {
				return nil, err
			}
			var receipt nativeIndexReceipt
			decoder := json.NewDecoder(file)
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&receipt)
			if err == nil && decoder.Decode(new(any)) != io.EOF {
				err = errors.New("trailing native index receipt data")
			}
			err = errors.Join(err, file.Close())
			if err != nil || receipt.Version != 1 || receipt.Path != path || len(receipt.After.Digest) != 64 || receipt.After.Size < 0 || !receipt.Absent && (len(receipt.Before.Digest) != 64 || receipt.Before.Size < 0) {
				return nil, errors.New("invalid native index import receipt; preserve for recovery")
			}
			for _, key := range receipt.Keys {
				if decoded, err := hex.DecodeString(key); err != nil || len(decoded) != 32 {
					return nil, errors.New("invalid native index row receipt")
				}
			}
			out[entry.Name()] = receipt
		}
		if errors.Is(readErr, io.EOF) {
			return out, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func nativeIndexConsumed(root *os.File, path string) (map[string]bool, bool, error) {
	receipts, err := nativeIndexReceipts(root, path)
	keys := map[string]bool{}
	pending := false
	for _, receipt := range receipts {
		if !receipt.Done {
			pending = true
			continue
		}
		for _, key := range receipt.Keys {
			keys[key] = true
		}
	}
	return keys, pending, err
}

// Callers fence legacy AND destination writers before a pending index merge.
// The private staged image and before/after digests make publication recoverable
// without treating a native edit after a crash as permission to overwrite it.
func importNativeIndex(root *accountAuthorityRoot, lock, origin, destination *os.File, source string, witness agents.NativeHistoryFile, indexKey string, spans []agents.NativeHistorySpan, dependencies []agents.NativeHistoryFile) error {
	if !filepath.IsLocal(witness.Path) || filepath.Clean(witness.Path) != witness.Path || witness.Path == "." {
		return errors.New("invalid native index path")
	}
	receipts, err := nativeIndexReceipts(root.dir(), witness.Path)
	if err != nil {
		return err
	}
	consumed := map[string]bool{}
	for name, receipt := range receipts {
		if !receipt.Done {
			if err := publishNativeIndex(root, lock, destination, name, receipt); err != nil {
				return err
			}
		}
		for _, key := range receipt.Keys {
			consumed[key] = true
		}
	}
	var selected []agents.NativeHistorySpan
	positions := map[string]int{}
	for _, span := range spans {
		if span.Key == "" {
			return errors.New("native index row has no import identity")
		}
		if consumed[span.Key] {
			continue
		}
		if position, duplicate := positions[span.Key]; duplicate {
			if indexKey != "" {
				selected[position] = span
			}
			continue
		}
		positions[span.Key] = len(selected)
		selected = append(selected, span)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Offset < selected[j].Offset })
	if len(selected) == 0 {
		return nil
	}
	// Check every ownership witness before staging any selected source bytes.
	for _, proof := range dependencies {
		if _, err := copyNativeHistory(origin, proof, io.Discard, nil); err != nil {
			return err
		}
	}
	name := nativeIndexPrefix(witness.Path) + randomHex(16) + ".json"
	stageName := strings.TrimSuffix(name, ".json") + ".data"
	fd, err := unix.Openat(int(root.dir().Fd()), stageName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	stage := os.NewFile(uintptr(fd), stageName)
	defer stage.Close()
	journaled := false
	defer func() {
		if !journaled {
			_ = unix.Unlinkat(int(root.dir().Fd()), stageName, 0)
		}
	}()
	receipt := nativeIndexReceipt{Version: 1, Path: witness.Path}
	current, err := safefile.OpenRegular(destination, witness.Path)
	if errors.Is(err, os.ErrNotExist) {
		receipt.Absent = true
	} else if err != nil {
		return err
	} else {
		defer current.Close()
		info, err := current.Stat()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 {
			return errors.New("native index has ambiguous link ownership")
		}
		hash := sha256.New()
		receipt.Before.Size, err = io.Copy(io.MultiWriter(stage, hash), current)
		if err != nil {
			return err
		}
		receipt.Before.Digest = hex.EncodeToString(hash.Sum(nil))
		if _, err := current.Seek(0, io.SeekStart); err != nil {
			return err
		}
		existing := map[string]bool{}
		if err := agents.NativeHistoryIndexRows(current, indexKey, func(key string, _, _ int64) error { existing[key] = true; return nil }); err != nil {
			return fmt.Errorf("native index changed or is invalid: %w", err)
		}
		kept := selected[:0]
		for _, span := range selected {
			receipt.Keys = append(receipt.Keys, span.Key)
			if !existing[span.Key] {
				kept = append(kept, span)
			}
		}
		selected = kept
		if receipt.Before.Size > 0 && len(selected) > 0 {
			var last [1]byte
			if _, err := stage.ReadAt(last[:], receipt.Before.Size-1); err != nil {
				return err
			}
			if last[0] != '\n' {
				if _, err := stage.Write([]byte("\n")); err != nil {
					return err
				}
			}
		}
	}
	if receipt.Absent {
		for _, span := range selected {
			receipt.Keys = append(receipt.Keys, span.Key)
		}
	}
	if _, err := copyNativeHistory(origin, witness, stage, selected); err != nil {
		return err
	}
	for _, proof := range dependencies {
		if _, err := copyNativeHistory(origin, proof, io.Discard, nil); err != nil {
			return err
		}
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	receipt.After.Size, err = io.Copy(hash, stage)
	if err != nil {
		return err
	}
	receipt.After.Digest = hex.EncodeToString(hash.Sum(nil))
	if err := stage.Sync(); err != nil {
		return err
	}
	// Receipt publication can have taken effect even if its final sync fails.
	// Keep the complete stage whenever that uncertainty is possible.
	journaled = true
	if err := writeNativeHistoryReceipt(root, lock, name, receipt, true); err != nil {
		return err
	}
	return publishNativeIndex(root, lock, destination, name, receipt)
}

func publishNativeIndex(root *accountAuthorityRoot, lock, destination *os.File, name string, receipt nativeIndexReceipt) error {
	stageName := strings.TrimSuffix(name, ".json") + ".data"
	if err := root.checkLock(lock); err != nil {
		return err
	}
	parent, err := nativeHistoryParent(destination, filepath.Dir(receipt.Path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := matchNativeHistoryReceipt(destination, receipt.Path, receipt.After); err != nil {
		if receipt.Absent {
			if !errors.Is(err, os.ErrNotExist) {
				return errors.New("native index appeared during import; originals and stage retained")
			}
		} else if err := matchNativeHistoryReceipt(destination, receipt.Path, receipt.Before); err != nil {
			return errors.New("native index changed during import; originals and stage retained")
		}
		if err := matchNativeHistoryReceipt(root.dir(), stageName, receipt.After); err != nil {
			return err
		}
		if receipt.Absent {
			if err := unix.Linkat(int(root.dir().Fd()), stageName, int(parent.Fd()), filepath.Base(receipt.Path), 0); err != nil {
				return err
			}
		} else if err := unix.Renameat(int(root.dir().Fd()), stageName, int(parent.Fd()), filepath.Base(receipt.Path)); err != nil {
			return err
		}
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	receipt.Done = true
	if err := writeNativeHistoryReceipt(root, lock, name, receipt, false); err != nil {
		return err
	}
	return clearNativeHistoryStage(root, lock, stageName)
}
