package sessionsvc

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

type FleetArtifact struct {
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}

type FleetArtifacts struct {
	Checkpoint string          `json:"checkpoint"`
	Directory  string          `json:"directory,omitempty"`
	Files      []FleetArtifact `json:"files"`
}

// FleetResultArtifacts verifies the same retained archive as checkout. Only task
// members are spooled locally; build reports need no source checkout or Git objects.
func FleetResultArtifacts(ctx context.Context, offer, parent string, names []string, wire io.Reader) (FleetArtifacts, error) {
	return fleetResultArtifacts(ctx, offer, parent, names, wire, defaultCheckpointReserve)
}

func fleetResultArtifacts(ctx context.Context, offer, parent string, names []string, wire io.Reader, reserve func(int64) int64) (result FleetArtifacts, err error) {
	parent, err = filepath.Abs(parent)
	if err != nil {
		return result, err
	}
	checkpoint, input, err := readFleetCheckpoint(wire)
	if err != nil {
		return result, err
	}
	ctx, stop, err := checkpointDiskContext(ctx, []string{parent}, 0, reserve)
	if err != nil {
		return result, err
	}
	defer stop()
	stage, err := os.MkdirTemp(parent, ".coop-artifacts-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	entries, prefix, err := spoolFleetArtifacts(ctx, stage, offer, checkpoint, input)
	if err != nil {
		return result, err
	}
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		if !fs.ValidPath(name) || name == "." || strings.ContainsRune(name, '\\') || selected[name] {
			return result, errors.New("invalid or repeated artifact path")
		}
		selected[name] = true
	}
	result = FleetArtifacts{Checkpoint: checkpoint.CheckpointRef, Files: []FleetArtifact{}}
	if len(names) > 0 {
		result.Directory, err = os.MkdirTemp(parent, "coop-artifacts-")
		if err != nil {
			return result, err
		}
		defer func() {
			if err != nil {
				_ = os.RemoveAll(result.Directory)
			}
		}()
	}
	for _, entry := range entries {
		name, ok := strings.CutPrefix(string(entry.PathBytes), prefix)
		if !ok || (len(names) > 0 && !selected[name]) {
			continue
		}
		if !fs.ValidPath(name) || name == "." || strings.ContainsRune(name, '\\') {
			return result, errors.New("invalid saved artifact path")
		}
		src := filepath.Join(stage, tasks.TasksRoot, string(entry.PathBytes))
		mediaType, err := artifactMediaType(src)
		if err != nil {
			return result, err
		}
		result.Files = append(result.Files, FleetArtifact{Name: name, Bytes: entry.ByteSize, SHA256: entry.SHA256, MediaType: mediaType})
		if result.Directory != "" {
			to := filepath.Join(result.Directory, name)
			if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
				return result, err
			}
			if err := os.Link(src, to); err != nil {
				return result, err
			}
			delete(selected, name)
		}
	}
	if len(selected) > 0 {
		return result, errors.New("requested artifact is absent from this checkpoint")
	}
	return result, nil
}

func spoolFleetArtifacts(ctx context.Context, stage, offer string, checkpoint workerproto.WorkspaceCheckpoint, input io.Reader) ([]workerproto.WorkspaceCheckpointFileEntry, string, error) {
	manifest, err := workerproto.ReadWorkspaceCheckpointBundle(checkpoint, reviewScanReader{ctx, input}, func(h *tar.Header, body io.Reader) error {
		if !strings.HasPrefix(h.Name, "task/") {
			return nil
		}
		file, err := os.OpenFile(filepath.Join(stage, filepath.Base(h.Name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		return errors.Join(copyCheckpointContent(ctx, file, body, h.Size, ""), file.Close())
	})
	if err != nil {
		return nil, "", err
	}
	queue := filepath.Join(stage, tasks.TasksRoot)
	for _, entry := range manifest.TaskProjection.Files {
		to := filepath.Join(queue, string(entry.PathBytes))
		if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
			return nil, "", err
		}
		ordinal := filepath.Join(stage, filepath.Base(entry.Entry))
		if err := os.Link(ordinal, to); err != nil {
			return nil, "", err
		}
		// A second spelling, including a filesystem case alias, must not
		// overwrite metadata. Release the staging link before authenticating it.
		if err := os.Remove(ordinal); err != nil {
			return nil, "", err
		}
	}
	binding, err := tasks.ReadControllerTaskBinding(stage, checkpoint.Task.ID)
	if err != nil {
		return nil, "", errors.Join(errors.New("artifact task metadata is invalid"), err)
	}
	if binding.OfferRef != offer || binding.QueueID != checkpoint.Task.QueueID || binding.TaskID != checkpoint.Task.TaskID {
		return nil, "", errors.New("artifact checkpoint belongs to a different job")
	}
	item, found, err := tasks.CurrentTask(queue, binding.ID)
	if err != nil || !found {
		return nil, "", errors.New("artifact task is missing")
	}
	prefix, err := filepath.Rel(queue, filepath.Join(item.Dir, "artifacts"))
	if err != nil {
		return nil, "", err
	}
	return manifest.TaskProjection.Files, filepath.ToSlash(prefix) + "/", nil
}

func artifactMediaType(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sample, err := io.ReadAll(io.LimitReader(file, 512))
	if err != nil {
		return "", err
	}
	return http.DetectContentType(sample), nil
}
