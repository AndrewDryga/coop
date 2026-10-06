// Package dockersock answers the one request coop sends a Docker daemon's socket itself: GET /info,
// the daemon's identity. A test that fakes the docker CLI pairs it with this, since the identity
// check no longer runs a docker process.
package dockersock

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Info is what a fake daemon reports: the fields coop reads from /info.
type Info struct {
	ID, OSType, Architecture, ServerVersion, KernelVersion string
	SecurityOptions                                        []string
}

// ErrUnavailable makes Serve answer 500, as a daemon that cannot report itself would.
var ErrUnavailable = errors.New("fixture daemon unavailable")

// Serve answers GET /info on a new unix socket with info(), read at each request so a test can
// replace the daemon mid-run, and returns the socket's endpoint, unix:///<path>. The socket lives
// under /tmp: macOS caps a socket path at 104 bytes, which a test's temporary directory can exceed.
func Serve(t testing.TB, info func() (Info, error)) string {
	t.Helper()
	return ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/info" {
			http.NotFound(w, r)
			return
		}
		value, err := info()
		if err != nil {
			http.Error(w, "fixture daemon unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}))
}

// ServeHandler is Serve with any handler, for a daemon that answers badly.
func ServeHandler(t testing.TB, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "coop-sock-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = os.RemoveAll(dir)
	})
	return "unix://" + path
}
