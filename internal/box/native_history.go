package box

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

// Import receipts live beside owner.json, outside the writable native home. A
// completed receipt prevents old history from resurrecting native deletions.
type nativeHistoryReceipt struct {
	Version            int    `json:"version"`
	Source             string `json:"source"`
	Path               string `json:"path"`
	SourceDigest       string `json:"source_digest"`
	SourceSize         int64  `json:"source_size"`
	Digest             string `json:"digest"`
	Size               int64  `json:"size"`
	Done               bool   `json:"done"`
	DependenciesDigest string `json:"dependencies_digest"`
}

// Check under the run's shared home lease. A competing importer may have
// crashed after preparation but before the launch acquired that lease.
func checkNativeHistorySettled(home string) error {
	root, err := safefile.OpenRoot(filepath.Join(filepath.Dir(home), "imports"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	for {
		entries, readErr := root.ReadDir(128)
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			file, err := safefile.OpenRegular(root, entry.Name())
			if err != nil {
				return err
			}
			var receipt struct {
				Version int
				Done    bool
			}
			decodeErr := json.NewDecoder(file).Decode(&receipt)
			closeErr := file.Close()
			if decodeErr != nil || closeErr != nil || receipt.Version != 1 || !receipt.Done {
				return errors.New("native history import needs recovery; retry preparation before launching this home")
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func nativeHistoryImported(home, source string, plan agents.NativeHistoryPlan) (bool, error) {
	if len(plan.Files) == 0 {
		return true, nil
	}
	root, err := safefile.OpenRoot(filepath.Join(filepath.Dir(home), "imports"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	for _, file := range plan.Files {
		if _, index := plan.Indexes[file.Path]; index {
			keys, pending, err := nativeIndexConsumed(root, file.Path)
			if err != nil {
				return false, err
			}
			if pending {
				return false, nil
			}
			for _, row := range plan.Filtered[file.Path] {
				if !keys[row.Key] {
					return false, nil
				}
			}
			continue
		}
		key := sha256.Sum256([]byte(source + "\x00" + file.Path))
		data, err := safefile.ReadRegular(root, hex.EncodeToString(key[:])+".json", 16<<10)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var receipt nativeHistoryReceipt
		if json.Unmarshal(data, &receipt) != nil || receipt.Version != 1 || receipt.Source != source || receipt.Path != file.Path || len(receipt.Digest) != 64 || len(receipt.SourceDigest) != 64 || receipt.Size < 0 || receipt.SourceSize < 0 {
			return false, errors.New("invalid native import receipt; preserve for recovery")
		}
		if !receipt.Done {
			return false, nil
		}
	}
	return true, nil
}

// ImportACPThreadBindings keeps both published records and their import receipts
// repository-local. A forgotten thread cannot reappear from the retained global store.
func ImportACPThreadBindings(ctx context.Context, cfg *config.Config, repo string, plan agents.NativeHistoryPlan) (string, error) {
	key, _, err := HistoryKey(repo)
	if err != nil {
		return "", err
	}
	root, err := openPrivateAccountTree(cfg.ConfigDir, "acp-threads", key, "bindings")
	if err != nil {
		return "", err
	}
	if err := root.checkNamed(); err != nil {
		root.close()
		return "", err
	}
	root.close()
	source := filepath.Join(cfg.ConfigDir, "acp-threads")
	destination := filepath.Join(source, key, "bindings")
	return destination, importNativeHistory(ctx, destination, source, plan)
}

func importNativeHistory(ctx context.Context, home, source string, plan agents.NativeHistoryPlan) error {
	root, err := openPrivateAccountTree(filepath.Dir(home), "imports")
	if err != nil {
		return err
	}
	defer root.close()
	lock, err := root.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	destination, err := safefile.OpenRoot(home)
	if err != nil {
		return err
	}
	defer destination.Close()
	if len(plan.Files) == 0 {
		return nil
	}
	origin, err := safefile.OpenRoot(source)
	if err != nil {
		return err
	}
	defer origin.Close()
	for _, file := range plan.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkNativeHistoryHome(home, destination); err != nil {
			return err
		}
		if err := checkNativeHistoryHome(source, origin); err != nil {
			return err
		}
		var importErr error
		if indexKey, index := plan.Indexes[file.Path]; index {
			importErr = importNativeIndex(root, lock, origin, destination, source, file, indexKey, plan.Filtered[file.Path], plan.Dependencies[file.Path])
		} else {
			importErr = importNativeHistoryFile(root, lock, origin, destination, source, file, plan.Filtered[file.Path], plan.Dependencies[file.Path]...)
		}
		if importErr != nil {
			return fmt.Errorf("import native history %s (original retained): %w", file.Path, importErr)
		}
		if err := checkNativeHistoryHome(home, destination); err != nil {
			return err
		}
		if err := checkNativeHistoryHome(source, origin); err != nil {
			return err
		}
	}
	return nil
}

func checkNativeHistoryHome(home string, destination *os.File) error {
	current, err := safefile.OpenRoot(home)
	if err != nil {
		return err
	}
	defer current.Close()
	if !sameAccountInode(current, destination) {
		return errors.New("native history home changed during import")
	}
	return nil
}

func importNativeHistoryFile(root *accountAuthorityRoot, lock, origin, destination *os.File,
	source string, witness agents.NativeHistoryFile, spans []agents.NativeHistorySpan, dependencies ...agents.NativeHistoryFile) error {
	if !filepath.IsLocal(witness.Path) || filepath.Clean(witness.Path) != witness.Path || witness.Path == "." {
		return errors.New("invalid native history path")
	}
	key := sha256.Sum256([]byte(source + "\x00" + witness.Path))
	name := hex.EncodeToString(key[:])
	receiptName, stageName := name+".json", name+".data"
	proofHash := sha256.New()
	encoder := json.NewEncoder(proofHash)
	for _, proof := range dependencies {
		if err := encoder.Encode(proof); err != nil {
			return err
		}
	}
	proofDigest := hex.EncodeToString(proofHash.Sum(nil))
	var receipt nativeHistoryReceipt
	data, err := safefile.ReadRegular(root.dir(), receiptName, 16<<10)
	if err == nil {
		if json.Unmarshal(data, &receipt) != nil || receipt.Version != 1 || receipt.Source != source ||
			receipt.Path != witness.Path || len(receipt.Digest) != 64 || receipt.Size < 0 ||
			len(receipt.SourceDigest) != 64 || receipt.SourceSize < 0 {
			return errors.New("invalid native import receipt; preserve for recovery")
		}
		if receipt.Done {
			return clearNativeHistoryStage(root, lock, stageName)
		}
		if receipt.DependenciesDigest != proofDigest {
			return errors.New("native ownership proofs changed during interrupted import; originals retained")
		}
		if receipt.SourceDigest != witness.SHA256 || receipt.SourceSize != witness.Size {
			return errors.New("source changed during interrupted migration; preserve both copies for recovery")
		}
		if _, err := copyNativeHistory(origin, witness, io.Discard, nil); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		// Without a receipt, no link into the native home could have been
		// published. An interrupted private scratch copy is safe to retry.
		if prior, openErr := safefile.OpenRegular(root.dir(), stageName); openErr == nil {
			privateErr := privateAccountFile(prior)
			_ = prior.Close()
			if privateErr != nil {
				return privateErr
			}
			if err := unix.Unlinkat(int(root.dir().Fd()), stageName, 0); err != nil {
				return err
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			return openErr
		}
		fd, err := unix.Openat(int(root.dir().Fd()), stageName,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return fmt.Errorf("create import stage: %w", err)
		}
		stage := os.NewFile(uintptr(fd), stageName)
		digest := sha256.New()
		count, copyErr := copyNativeHistory(origin, witness, io.MultiWriter(stage, digest), spans)
		if copyErr == nil {
			copyErr = stage.Sync()
		}
		copyErr = errors.Join(copyErr, stage.Close())
		if copyErr != nil {
			// No receipt or native file exists yet; an ordinary retry can safely
			// recreate this owned scratch file after the legacy writer exits.
			return errors.Join(copyErr, unix.Unlinkat(int(root.dir().Fd()), stageName, 0), root.dir().Sync())
		}
		receipt = nativeHistoryReceipt{Version: 1, Source: source, Path: witness.Path,
			SourceDigest: witness.SHA256, SourceSize: witness.Size,
			Digest: hex.EncodeToString(digest.Sum(nil)), Size: count, DependenciesDigest: proofDigest}
		if err := writeNativeHistoryReceipt(root, lock, receiptName, receipt, true); err != nil {
			return err
		}
	}
	parent, err := nativeHistoryParent(destination, filepath.Dir(witness.Path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := root.checkLock(lock); err != nil {
		return err
	}
	if err := matchNativeHistoryReceipt(root.dir(), stageName, receipt); err != nil {
		return err
	}
	for _, proof := range dependencies {
		if _, err := copyNativeHistory(origin, proof, io.Discard, nil); err != nil {
			return fmt.Errorf("native ownership proof changed: %w", err)
		}
	}
	// linkat is portable, atomic and no-clobber. The stage remains private until
	// its completed receipt is durable; recovery accepts only those exact bytes.
	err = unix.Linkat(int(root.dir().Fd()), stageName, int(parent.Fd()), filepath.Base(witness.Path), 0)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	if err := matchNativeHistoryReceipt(destination, witness.Path, receipt); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	receipt.Done = true
	if err := writeNativeHistoryReceipt(root, lock, receiptName, receipt, false); err != nil {
		return err
	}
	return clearNativeHistoryStage(root, lock, stageName)
}

func clearNativeHistoryStage(root *accountAuthorityRoot, lock *os.File, stageName string) error {
	if err := root.checkLock(lock); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(root.dir().Fd()), stageName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return root.dir().Sync()
}

func copyNativeHistory(root *os.File, witness agents.NativeHistoryFile, out io.Writer, spans []agents.NativeHistorySpan) (int64, error) {
	file, err := safefile.OpenRegular(root, witness.Path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return 0, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || before.Size() != witness.Size {
		return 0, errors.New("history ownership or size changed after inspection")
	}
	digest := sha256.New()
	reader := io.TeeReader(file, digest)
	var count int64
	if spans == nil {
		count, err = io.Copy(out, reader)
	} else {
		var at int64
		for _, span := range spans {
			if span.Offset < at || span.Size <= 0 || span.Offset > witness.Size || span.Size > witness.Size-span.Offset {
				return 0, errors.New("invalid native history selection")
			}
			if _, err := io.CopyN(io.Discard, reader, span.Offset-at); err != nil {
				return 0, err
			}
			n, err := io.CopyN(out, reader, span.Size)
			count += n
			if err != nil {
				return 0, err
			}
			at = span.Offset + span.Size
		}
		_, err = io.Copy(io.Discard, reader)
	}
	if err != nil {
		return 0, err
	}
	after, err := file.Stat()
	if err != nil {
		return 0, err
	}
	named, err := safefile.OpenRegular(root, witness.Path)
	if err != nil {
		return 0, err
	}
	defer named.Close()
	current, err := named.Stat()
	if err != nil || !os.SameFile(before, current) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) || before.Size() != current.Size() ||
		!before.ModTime().Equal(current.ModTime()) || hex.EncodeToString(digest.Sum(nil)) != witness.SHA256 {
		return 0, errors.New("history changed after ownership inspection; retry after its writer exits")
	}
	return count, nil
}

func nativeHistoryParent(root *os.File, rel string) (*os.File, error) {
	dir, err := safefile.CloneDir(root)
	if err != nil || rel == "." {
		return dir, err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if err := unix.Mkdirat(int(dir.Fd()), part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			_ = dir.Close()
			return nil, err
		}
		next, err := safefile.OpenDir(dir, part)
		syncErr := dir.Sync()
		_ = dir.Close()
		if err != nil || syncErr != nil {
			if next != nil {
				_ = next.Close()
			}
			return nil, errors.Join(err, syncErr)
		}
		dir = next
	}
	return dir, nil
}

func matchNativeHistoryReceipt(root *os.File, name string, receipt nativeHistoryReceipt) error {
	file, err := safefile.OpenRegular(root, name)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	count, err := io.Copy(digest, file)
	if err != nil {
		return err
	}
	if count != receipt.Size || hex.EncodeToString(digest.Sum(nil)) != receipt.Digest {
		return errors.New("native destination conflicts with imported history; neither copy was overwritten")
	}
	return nil
}

func writeNativeHistoryReceipt(root *accountAuthorityRoot, lock *os.File, name string, receipt any, create bool) error {
	if err := root.checkLock(lock); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	target := ".receipt-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(root.dir().Fd()), target,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(root.dir().Fd()), target, 0) }()
	file := os.NewFile(uintptr(fd), target)
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	if create {
		if err := unix.Linkat(int(root.dir().Fd()), target, int(root.dir().Fd()), name, 0); err != nil {
			return err
		}
	} else {
		if err := unix.Renameat(int(root.dir().Fd()), target, int(root.dir().Fd()), name); err != nil {
			return err
		}
	}
	return errors.Join(root.dir().Sync(), root.checkLock(lock))
}
