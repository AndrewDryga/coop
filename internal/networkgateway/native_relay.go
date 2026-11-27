package networkgateway

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// This socket is mounted only in the credential guard. The host relay holds
// worker TLS identity and the temporary controller grant.
type nativeRelayTransport struct {
	provider string
	upstream string
}

func (t *nativeRelayTransport) RoundTrip(in *http.Request) (*http.Response, error) {
	out := in.Clone(in.Context())
	out.URL.Scheme = "http"
	out.URL.Host = "localhost"
	out.Host = "localhost"
	out.URL.Path = "/" + t.provider + "/" + t.upstream + in.URL.Path
	out.URL.RawPath = ""
	for key := range out.Header {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "X-Api-Key") || strings.EqualFold(key, "X-Goog-Api-Key") {
			out.Header.Del(key)
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", filepath.Join(NativePrivateDirectory, "relay.sock"))
	}}
	// A per-request connection makes teardown and revoked-stream cancellation
	// independent of an idle pool living beyond the run.
	transport.DisableKeepAlives = true
	return transport.RoundTrip(out)
}
