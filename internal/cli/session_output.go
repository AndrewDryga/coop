package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/session"
)

const sessionOutputRoot = ".coop-output"

func prepareSessionOutputDir(workspace, turnID string) (string, string, error) {
	if !filepath.IsAbs(workspace) || !validSessionHTTPPathID(turnID) {
		return "", "", errors.New("invalid turn output identity")
	}
	root := filepath.Join(workspace, sessionOutputRoot)
	dir := filepath.Join(root, turnID)
	if err := os.Mkdir(root, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", fmt.Errorf("create turn output root: %w", err)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("turn output root is unsafe")
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		return "", "", fmt.Errorf("create turn output directory: %w", err)
	}
	return dir, filepath.ToSlash(filepath.Join(sessionOutputRoot, turnID)), nil
}

func removeSessionOutputDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	root := filepath.Dir(dir)
	if entries, err := os.ReadDir(root); err == nil && len(entries) == 0 {
		_ = os.Remove(root)
	}
	return nil
}

func collectSessionOutputDir(dir string) ([]session.OutputArtifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read turn output directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if len(entries) > session.MaxTurnArtifacts {
		return nil, errors.New("turn produced too many output artifacts")
	}
	artifacts := make([]session.OutputArtifact, 0, len(entries))
	total := 0
	for _, entry := range entries {
		name := entry.Name()
		if !validOutputName(name) || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil, errors.New("turn produced an unsafe output artifact")
		}
		path := filepath.Join(dir, name)
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, fmt.Errorf("open turn output artifact: %w", err)
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			file.Close()
			return nil, errors.New("turn output artifact is unsafe")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, session.MaxArtifactBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > session.MaxArtifactBytes {
			return nil, errors.New("turn output artifact exceeds its bound")
		}
		if total > session.MaxTurnArtifactBytes-len(data) {
			return nil, errors.New("turn output artifacts exceed their total bound")
		}
		total += len(data)
		mediaType, ok := outputMediaType(name, data)
		if !ok {
			return nil, errors.New("turn output artifact is not a supported image")
		}
		artifacts = appendOutputArtifact(artifacts, name, mediaType, data)
	}
	return artifacts, nil
}

func appendOutputArtifact(artifacts []session.OutputArtifact, name, mediaType string, data []byte) []session.OutputArtifact {
	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	for _, artifact := range artifacts {
		if artifact.SHA256 == hexDigest {
			return artifacts
		}
	}
	return append(artifacts, session.OutputArtifact{
		ID: "artifact_" + hexDigest[:24], Name: name, MediaType: mediaType,
		SHA256: hexDigest, Bytes: int64(len(data)), Data: append([]byte(nil), data...),
	})
}

func decodeACPOutputImage(data, mediaType string, ordinal int) (session.OutputArtifact, error) {
	if ordinal >= session.MaxTurnArtifacts {
		return session.OutputArtifact{}, errors.New("turn produced too many output artifacts")
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) == 0 || len(decoded) > session.MaxArtifactBytes {
		return session.OutputArtifact{}, errors.New("ACP image content is invalid or outside bounds")
	}
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[mediaType]
	name := fmt.Sprintf("generated-%d%s", ordinal+1, extension)
	detected, ok := outputMediaType(name, decoded)
	if !ok || detected != mediaType {
		return session.OutputArtifact{}, errors.New("ACP image content does not match its media type")
	}
	return appendOutputArtifact(nil, name, mediaType, decoded)[0], nil
}

func validOutputName(name string) bool {
	if name == "" || len(name) > session.MaxArtifactNameBytes || filepath.Base(name) != name || name == "." || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func outputMediaType(name string, data []byte) (string, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif":
		detected := http.DetectContentType(data)
		want := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif"}[ext]
		return want, detected == want
	case ".webp":
		return "image/webp", len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	default:
		return "", false
	}
}
