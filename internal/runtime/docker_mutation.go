package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Only fixed, empty-body container operations use this path. Custody/start
// checks stay in the caller, and a response never substitutes for its postcondition.
func (d *Docker) containerMutation(ctx context.Context, method, id, suffix string) error {
	stop := false
	if grace, err := strconv.Atoi(strings.TrimPrefix(suffix, "/stop?t=")); err == nil {
		stop = grace >= 0 && grace <= 10 && suffix == "/stop?t="+strconv.Itoa(grace)
	}
	if !dockerHexID(id) || !(method == http.MethodPost && (suffix == "/start" || stop) || method == http.MethodDelete && suffix == "?force=true") {
		return errors.New("invalid Docker container mutation")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := d.Verify(ctx); err != nil {
		return err
	}
	response, err := d.daemonRequest(ctx, method, "/containers/"+id+suffix)
	if err != nil {
		return errors.Join(errors.New("Docker container mutation outcome unknown"), ctx.Err())
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1025))
	if err != nil || len(data) > 1024 {
		return errors.Join(errors.New("Docker container mutation response incomplete; outcome unknown"), ctx.Err())
	}
	if err := d.Verify(ctx); err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent {
		return errors.New("Docker container mutation failed; outcome unknown")
	}
	return nil
}
