package cli

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/session"
)

func sessionACPInputContent(
	turn session.Turn,
	imageCapable, resourceCapable bool,
) ([]map[string]any, error) {
	content := []map[string]any{{"type": "text", "text": turn.Prompt}}
	for _, artifact := range turn.Artifacts {
		switch {
		case strings.HasPrefix(artifact.MediaType, "image/"):
			if !imageCapable {
				return nil, acpFailure(
					sessionACPProtocolError,
					"selected agent does not accept image input",
				)
			}
			content = append(content, map[string]any{
				"type": "image", "data": base64.StdEncoding.EncodeToString(artifact.Data),
				"mimeType": artifact.MediaType,
			})
		case isTextArtifact(artifact.MediaType):
			if !utf8.Valid(artifact.Data) {
				return nil, acpFailure(
					sessionACPProtocolError,
					"text artifact is not valid UTF-8",
				)
			}
			content = append(content, map[string]any{
				"type": "text",
				"text": fmt.Sprintf(
					"\n<attached-file name=%q media-type=%q sha256=%q>\n%s\n</attached-file>",
					artifact.Name, artifact.MediaType, artifact.SHA256, artifact.Data,
				),
			})
		default:
			if !resourceCapable {
				return nil, acpFailure(
					sessionACPProtocolError,
					"selected agent does not accept embedded file input",
				)
			}
			content = append(content, map[string]any{
				"type": "resource",
				"resource": map[string]any{
					"uri":      "attachment:///" + artifact.SHA256 + "/" + artifact.Name,
					"mimeType": artifact.MediaType,
					"blob":     base64.StdEncoding.EncodeToString(artifact.Data),
				},
			})
		}
	}
	return content, nil
}

func isTextArtifact(mediaType string) bool {
	return strings.HasPrefix(mediaType, "text/") ||
		mediaType == "application/json" ||
		mediaType == "application/yaml" ||
		mediaType == "application/x-yaml"
}
