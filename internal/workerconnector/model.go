package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func (t *HTTPTransport) ModelGrant(ctx context.Context, job string, binding workerproto.ModelGrantRequest) (workerproto.ModelGrant, error) {
	var grant workerproto.ModelGrant
	if !reference(job, 256) || binding.Validate() != nil {
		return grant, errors.New("invalid model grant request")
	}
	body, err := json.Marshal(binding)
	if err != nil {
		return grant, err
	}
	endpoint := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/jobs/" + url.PathEscape(job) + "/model-grants"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return grant, err
	}
	req.Header.Set("Content-Type", "application/json")
	client, err := t.clientFor(ctx)
	if err != nil {
		return grant, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return grant, errors.New("controller model access unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
			return grant, workerproto.ErrModelGrantRefused
		}
		return grant, errors.New("controller refused model access")
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8193))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&grant) != nil || decoder.Decode(new(any)) != io.EOF || grant.ModelGrantRequest != binding || grant.Validate(time.Now()) != nil {
		return workerproto.ModelGrant{}, errors.New("invalid controller model grant")
	}
	return grant, nil
}

// ForwardModel streams the existing native HTTP request. The authenticated
// worker identity stays in this host process throughout the stream.
func (t *HTTPTransport) ForwardModel(ctx context.Context, job, provider, host, token string, in *http.Request) (*http.Response, error) {
	if !reference(job, 256) || !reference(provider, 32) || !reference(host, 256) || len(token) < 32 || len(token) > 512 {
		return nil, errors.New("invalid model proxy binding")
	}
	target := t.baseURL.ResolveReference(&url.URL{Path: "/v1/coop-workers/jobs/" + url.PathEscape(job) + "/models/" + url.PathEscape(provider) + "/" + url.PathEscape(host) + in.URL.Path, RawQuery: in.URL.RawQuery})
	req, err := http.NewRequestWithContext(ctx, in.Method, target.String(), in.Body)
	if err != nil {
		return nil, err
	}
	req.Header = in.Header.Clone()
	for _, key := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Goog-Api-Key", "X-Coop-Model-Grant"} {
		req.Header.Del(key)
	}
	req.Header.Set("X-Coop-Model-Grant", token)
	req.ContentLength = in.ContentLength
	client, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	if transport, ok := client.Transport.(workerRoundTripper); ok {
		return forwardModelStream(req, transport)
	}
	stream := *client
	stream.Timeout = 0
	stream.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return stream.Do(req)
}
