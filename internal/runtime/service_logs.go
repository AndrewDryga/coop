package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Each cursor includes the last timestamp's records. Docker's since filter is inclusive.
// Verify those same records before continuing so rotation or a replacement cannot skip data.
type serviceLogCursor struct {
	Container string
	Time      string
	Count     int
	Digest    [32]byte
}

type ServiceLogRecord struct {
	Time   string `json:"time"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type ServiceLogPage struct {
	Records []ServiceLogRecord `json:"records"`
	Cursor  string             `json:"cursor,omitempty"`
	AtEnd   bool               `json:"at_end"`
}

func decodeServiceLogCursor(container, token string) (serviceLogCursor, error) {
	c := serviceLogCursor{Container: container}
	if token == "" {
		return c, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(token) > 512 || json.Unmarshal(raw, &c) != nil || c.Container != container || c.Count < 1 || c.Count > 100_000 {
		return c, errors.New("invalid service log cursor")
	}
	_, err = time.Parse(time.RFC3339Nano, c.Time)
	return c, err
}

// ServiceLogs reads only the retained logs of the immutable, job-labelled container. It
// buffers whole records for redaction and never hands daemon error text to the caller.
func (r Runtime) ServiceLogs(ctx context.Context, labels map[string]string, id, token string) (ServiceLogPage, error) {
	var empty ServiceLogPage
	if r.composeEndpoint == "" || !dockerHexID(id) || len(token) > 512 {
		return empty, errors.New("service log observation needs a frozen engine and container")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cursor, err := decodeServiceLogCursor(id, token)
	if err != nil {
		return empty, err
	}
	container, found, err := r.ObserveServiceContainer(ctx, labels)
	if err != nil || !found || container.ID != id {
		return empty, errors.New("service log container changed")
	}
	d, err := InspectDocker(ctx, r)
	if err != nil {
		return empty, err
	}
	defer d.Close()
	query := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}}
	if cursor.Time != "" {
		at, _ := time.Parse(time.RFC3339Nano, cursor.Time)
		query.Set("since", fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond()))
	}
	response, err := d.daemonGet(ctx, "/containers/"+id+"/logs?"+query.Encode())
	if err != nil {
		return empty, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return empty, errors.New("Docker service logs unavailable")
	}
	page, err := readServiceLogPage(response.Body, cursor)
	if err != nil {
		return empty, err
	}
	return page, d.Verify(ctx)
}

func readServiceLogPage(src io.Reader, previous serviceLogCursor) (ServiceLogPage, error) {
	page := ServiceLogPage{Records: []ServiceLogRecord{}}
	next := serviceLogCursor{Container: previous.Container}
	reader := bufio.NewReader(src)
	verified := previous.Time == ""
	size := 0
	for len(page.Records) < 128 && size < 48<<10 {
		var header [8]byte
		_, err := io.ReadFull(reader, header[:])
		if err == io.EOF && verified {
			page.AtEnd = true
			break
		}
		if err != nil || (header[0] != 1 && header[0] != 2) || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return ServiceLogPage{}, errors.New("service log stream is unavailable, changed or malformed")
		}
		length := binary.BigEndian.Uint32(header[4:])
		if length == 0 || length > 1<<20 {
			return ServiceLogPage{}, errors.New("service log record exceeds observation limits")
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(reader, data); err != nil {
			return ServiceLogPage{}, err
		}
		stamp, message, ok := bytes.Cut(data, []byte(" "))
		if _, err := time.Parse(time.RFC3339Nano, string(stamp)); !ok || err != nil {
			return ServiceLogPage{}, errors.New("service log record has no Docker timestamp")
		}
		if !verified && string(stamp) != previous.Time {
			return ServiceLogPage{}, errors.New("previous service logs are no longer retained")
		}
		if string(stamp) != next.Time {
			next = serviceLogCursor{Container: previous.Container, Time: string(stamp)}
		}
		hash := sha256.New()
		hash.Write(next.Digest[:])
		hash.Write(header[:])
		hash.Write(data)
		copy(next.Digest[:], hash.Sum(nil))
		next.Count++
		if next.Count > 100_000 {
			return ServiceLogPage{}, errors.New("too many log entries share a timestamp")
		}
		if !verified {
			if next.Count == previous.Count {
				if next.Digest != previous.Digest {
					return ServiceLogPage{}, errors.New("previous service logs changed")
				}
				verified = true
			}
			continue
		}
		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}
		page.Records = append(page.Records, ServiceLogRecord{Time: next.Time, Stream: stream, Text: string(message)})
		size += len(message)
	}
	if next.Count > 0 {
		raw, err := json.Marshal(next)
		if err != nil {
			return ServiceLogPage{}, err
		}
		page.Cursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
