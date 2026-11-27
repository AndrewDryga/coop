package sessionsvc

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"runtime"
	"strings"

	"github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// ModelAccess is owned by this connection's controller transport. No provider
// credential crosses it. ForwardModel returns a streaming response.
type ModelAccess interface {
	ModelGrant(context.Context, string, workerproto.ModelGrantRequest) (workerproto.ModelGrant, error)
	ForwardModel(context.Context, string, string, string, string, *http.Request) (*http.Response, error)
}

// Custom runners own their model transport; the shipped Docker runner needs
// its host relay and guard to share Linux Unix sockets on one filesystem.
func (s *Service) supportsControllerModels() bool {
	if s.modelAccess == nil {
		return false
	}
	if _, native := s.runner.(*sessionTurnRunner); native || s.runner == nil {
		return runtime.GOOS == "linux" && s.rt.IsDocker()
	}
	return true
}

func (h *sessionHTTPHandler) modelAccess(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/model-access/"), "/", 4)
	if len(parts) < 2 || !h.service.supportsControllerModels() {
		http.Error(w, "model access unavailable", http.StatusServiceUnavailable)
		return
	}
	s := h.service
	bound, err := s.store.GetSession(r.Context(), parts[0])
	if err != nil || s.validateSessionForkAuthority(r.Context(), bound) != nil ||
		bound.State == session.SessionDiscarded {
		http.Error(w, "model session refused", http.StatusForbidden)
		return
	}
	job, err := sessionJobSpec(bound)
	if err != nil || job.ModelCredentials != "controller" {
		http.Error(w, "model authority refused", http.StatusForbidden)
		return
	}
	s.runtimeMu.Lock()
	poisoned, held := s.runtimeSlots[bound.ID]
	s.runtimeMu.Unlock()
	if !held || poisoned {
		http.Error(w, "model runtime unavailable", http.StatusForbidden)
		return
	}
	switch parts[1] {
	case "grant":
		if len(parts) != 2 || r.Method != "POST" {
			http.Error(w, "model grant refused", 400)
			return
		}
		var req workerproto.ModelGrantRequest
		if !decodeSessionJSONLimit(w, r, &req, 8192) {
			return
		}
		if req.Validate() != nil || req.JobDigest != bound.JobDigest || !jobUsesAccount(job, req.Provider, req.Account) {
			http.Error(w, "model account refused", http.StatusForbidden)
			return
		}
		grant, err := s.modelAccess.ModelGrant(r.Context(), job.JobRef, req)
		if err != nil {
			if errors.Is(err, workerproto.ErrModelGrantRefused) {
				http.Error(w, "controller refused model access", http.StatusForbidden)
				return
			}
			http.Error(w, "controller model access unavailable", http.StatusServiceUnavailable)
			return
		}
		writeSessionJSON(w, 200, grant)
	case "forward":
		if len(parts) != 4 {
			http.Error(w, "model route refused", 400)
			return
		}
		target := strings.SplitN(parts[3], "/", 2)
		if len(target) != 2 {
			http.Error(w, "model origin refused", 400)
			return
		}
		in := r.Clone(r.Context())
		in.URL.Path = "/" + target[1]
		in.URL.RawPath = ""
		in.Body = http.MaxBytesReader(w, in.Body, 32<<20)
		token := r.Header.Get("X-Coop-Model-Grant")
		proxy := &httputil.ReverseProxy{Transport: modelRoundTripper(func(out *http.Request) (*http.Response, error) {
			return s.modelAccess.ForwardModel(out.Context(), job.JobRef, parts[2], target[0], token, out)
		}), Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.Host = "localhost"
			p.Out.URL.Scheme = "http"
			p.Out.URL.Host = "localhost"
		},
			ModifyResponse: func(response *http.Response) error { response.Header.Del("Set-Cookie"); return nil },
			ErrorLog:       log.New(io.Discard, "", 0), FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "controller model unavailable", http.StatusBadGateway)
			}}
		proxy.ServeHTTP(w, in)
	default:
		http.Error(w, "unknown model route", 404)
	}
}

func jobUsesAccount(job workerproto.JobSpec, provider, account string) bool {
	targets := append([]string(nil), job.Targets...)
	if job.Preset != nil {
		for _, role := range job.Preset.Roles {
			targets = append(targets, role.Targets...)
		}
	}
	for _, spec := range targets {
		t, err := agent.ParseTarget(spec)
		if err == nil && t.Provider == provider && t.Account() == account {
			return true
		}
	}
	return false
}

type modelRoundTripper func(*http.Request) (*http.Response, error)

func (t modelRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }
