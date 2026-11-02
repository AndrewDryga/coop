package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The wire includes labels and driver status that exposure does not consume.
// The selected definition retains the CLI projection's smaller limit.
const maxDockerVolumeObservation = 1 << 20

// The caller fences the complete inventory with Verify before and after it.
// Only a completed 404 response establishes absence; errors expose no body.
func (d *Docker) inspectVolumeDefinition(ctx context.Context, name string) (volumeDefinition, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := d.daemonGet(ctx, "/volumes/"+url.PathEscape(name))
	if err != nil {
		return volumeDefinition{}, false, errors.Join(errors.New("Docker volume observation unavailable"), ctx.Err())
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDockerVolumeObservation+1))
	if err != nil {
		return volumeDefinition{}, false, errors.Join(errors.New("Docker volume observation incomplete"), ctx.Err())
	}
	if len(data) > maxDockerVolumeObservation {
		return volumeDefinition{}, false, errors.New("Docker observation exceeded its bound; outcome unknown")
	}
	if response.StatusCode == http.StatusNotFound {
		return volumeDefinition{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return volumeDefinition{}, false, errors.New("Docker volume observation unavailable")
	}
	var value volumeDefinition
	if json.Unmarshal(data, &value) != nil || value.Name != name || len(value.Options) > 32 {
		return volumeDefinition{}, false, errors.New("selected runtime volume definition is invalid or unavailable")
	}
	projection, err := json.Marshal(value)
	if err != nil || len(projection) > 64<<10 {
		return volumeDefinition{}, false, errors.New("Docker observation exceeded its bound; outcome unknown")
	}
	return value, true, nil
}
