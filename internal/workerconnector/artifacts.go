package workerconnector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	maxArtifactBytes    = 8 << 20
	maxReviewPatchBytes = 64 << 20
)

type Artifact struct {
	ID        string
	Name      string
	MediaType string
	SHA256    string
	Data      []byte
}

type ArtifactTransport interface {
	FetchInputArtifact(context.Context, string, string) (Artifact, error)
	FetchWorkspaceCheckpoint(context.Context, string, string) (workerproto.WorkspaceCheckpoint, []byte, error)
	UploadOutputArtifact(context.Context, string, Artifact) ([]byte, error)
	UploadReviewPatch(context.Context, string, string, string, []byte) ([]byte, error)
	UploadWorkspaceCheckpoint(context.Context, string, workerproto.WorkspaceCheckpoint, []byte) ([]byte, error)
}

type OutputArtifactAPI interface {
	FetchOutputArtifact(context.Context, string, string, string) (Artifact, error)
}

type ReviewPatchAPI interface {
	FetchReviewPatch(context.Context, string, string, int64) ([]byte, error)
}

type WorkspaceCheckpointAPI interface {
	FetchWorkspaceCheckpointBundle(context.Context, string, workerproto.WorkspaceCheckpoint) ([]byte, error)
}

type WorkspaceRestoreAPI interface {
	RestoreWorkspaceCheckpoint(
		context.Context,
		string,
		string,
		int,
		workerproto.WorkspaceCheckpoint,
		[]byte,
	) (json.RawMessage, error)
}

func validateArtifact(artifact Artifact, requireID bool) error {
	if requireID && !reference(artifact.ID, 256) {
		return errors.New("artifact id is invalid")
	}
	if !safeArtifactName(artifact.Name) || !artifactMediaType(artifact.MediaType) ||
		len(artifact.Data) == 0 || len(artifact.Data) > maxArtifactBytes || !digest(artifact.SHA256) {
		return errors.New("artifact metadata is invalid")
	}
	sum := sha256.Sum256(artifact.Data)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return errors.New("artifact digest does not match")
	}
	return nil
}

func safeArtifactName(value string) bool {
	if value == "" || len(value) > 255 || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if character < 32 || character == 127 || character == '/' || character == '\\' {
			return false
		}
	}
	return true
}

func artifactMediaType(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/webp", "image/gif", "text/plain", "text/markdown", "text/csv", "application/json", "application/yaml", "application/x-yaml", "application/pdf":
		return true
	default:
		return false
	}
}

func outputArtifactMediaType(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}
