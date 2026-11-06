package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The full response includes unused image metadata; selected fields retain the
// former CLI projection's smaller bound. Environment values are never decoded.
const maxDockerImageObservation = 4 << 20

type dockerImageJSON struct {
	ID     string
	Config *struct {
		Labels                 map[string]string
		Entrypoint, Cmd, Shell []string
		Healthcheck            *DockerHealthcheck
		Volumes                map[string]struct{}
	}
	RootFS struct{ Layers []string }
}

func (d *Docker) inspectImageJSON(ctx context.Context, name string) (dockerImageJSON, error) {
	var value dockerImageJSON
	if !dockerToken(name, 512) || strings.HasPrefix(name, "-") {
		return value, errors.New("invalid Docker image reference")
	}
	if ctx == nil {
		return value, errors.Join(errDockerCommandNotStarted, errors.New("invalid bounded Docker operation"))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := d.Verify(ctx); err != nil {
		return value, err
	}
	response, err := d.daemonGet(ctx, "/images/"+url.PathEscape(name)+"/json")
	if err != nil {
		return value, errors.Join(errors.New("Docker image observation unavailable"), ctx.Err())
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDockerImageObservation+1))
	if err != nil {
		return value, errors.Join(errors.New("Docker image observation incomplete"), ctx.Err())
	}
	if len(data) > maxDockerImageObservation {
		return value, errors.New("Docker observation exceeded its bound; outcome unknown")
	}
	if err := d.Verify(ctx); err != nil {
		return value, err
	}
	if response.StatusCode == http.StatusNotFound {
		return value, ErrDockerImageMissing
	}
	if response.StatusCode != http.StatusOK {
		return value, errors.New("Docker image observation unavailable")
	}
	if json.Unmarshal(data, &value) != nil || value.Config == nil ||
		!strings.HasPrefix(value.ID, "sha256:") || !dockerHexID(strings.TrimPrefix(value.ID, "sha256:")) {
		return dockerImageJSON{}, errors.New("invalid Docker image observation")
	}
	return value, nil
}

func dockerImageProjectionFits(value any) bool {
	data, err := json.Marshal(value)
	return err == nil && len(data) <= 64<<10
}

// TagImage gives the image with this ID the name tag on this daemon, and reads the tag back. A
// project build names its image only once the image passed its proofs, so an image Coop refused never
// takes the project's tag.
func (d *Docker) TagImage(ctx context.Context, id, tag string) error {
	if !strings.HasPrefix(id, "sha256:") || !dockerHexID(strings.TrimPrefix(id, "sha256:")) ||
		!dockerToken(tag, 512) || strings.HasPrefix(tag, "-") {
		return errors.New("invalid Docker image tag")
	}
	_, tagErr := d.output(ctx, 1024, "image", "tag", id, tag)
	named, _, err := d.Image(ctx, tag)
	if err == nil && named == id {
		return nil
	}
	return errors.Join(errors.New("Docker did not tag the image"), tagErr, err)
}
