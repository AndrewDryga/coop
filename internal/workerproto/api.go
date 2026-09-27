package workerproto

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// APIRequest addresses only the worker's private Coop API. Large bodies travel
// separately under the same authenticated command identity.
type APIRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	BodyRef *BodyReference    `json:"body_ref,omitempty"`
}

type BodyReference struct {
	SHA256   string `json:"sha256"`
	ByteSize int64  `json:"byte_size"`
}

type APIResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	BodyRef *BodyReference    `json:"body_ref,omitempty"`
}

func DecodeAPIRequest(raw []byte) (APIRequest, error) {
	var request APIRequest
	if err := decodeStrict(raw, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}

func (r APIRequest) Validate() error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return errors.New("unsupported API method")
	}
	parsed, err := url.ParseRequestURI(r.Path)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" ||
		!strings.HasPrefix(parsed.Path, "/v1/") || path.Clean(parsed.Path) != parsed.Path ||
		strings.ContainsAny(parsed.Path, "\\\x00\r\n") {
		return errors.New("API path must be a relative Coop /v1 path")
	}
	if len(r.Body) > 256<<10 || (len(r.Body) > 0 && !json.Valid(r.Body)) ||
		(r.BodyRef != nil && (len(r.Body) > 0 || r.BodyRef.Validate() != nil)) {
		return errors.New("invalid API body")
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && (len(r.Body) > 0 || r.BodyRef != nil) {
		return errors.New("read request cannot carry a body")
	}
	if len(r.Headers) > 16 {
		return errors.New("too many API headers")
	}
	total := 0
	for key, value := range r.Headers {
		switch key {
		case "content-type", "accept", "prefer", "if-match", "x-coop-expected-revision", "x-coop-workspace-checkpoint":
		default:
			return errors.New("unsupported API request header")
		}
		total += len(key) + len(value)
		if total > 256<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid API header value")
		}
	}
	return nil
}

func (r BodyReference) Validate() error {
	if !digestPattern.MatchString(r.SHA256) || r.ByteSize <= 0 || r.ByteSize == math.MaxInt64 {
		return errors.New("invalid transferred body identity")
	}
	return nil
}
