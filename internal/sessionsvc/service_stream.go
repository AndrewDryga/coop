package sessionsvc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

type serviceRelayTransport interface {
	ConnectServiceRelay(context.Context, string) (net.Conn, error)
}

type serviceRelayRequest struct {
	Connection string `json:"connection"`
	Container  string `json:"container"`
	JobDigest  string `json:"job_digest"`
	Port       int    `json:"port"`
	Expires    int64  `json:"expires"`
}

func (h *sessionHTTPHandler) openServiceConnection(w http.ResponseWriter, r *http.Request, sessionID, name string) {
	var req serviceRelayRequest
	if !decodeSessionJSONLimit(w, r, &req, 2048) {
		return
	}
	if err := h.service.openServiceRelay(r.Context(), sessionID, name, req); err != nil {
		writeSessionHTTPError(w, http.StatusConflict, "service_unavailable", "service connection unavailable")
		return
	}
	writeSessionJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Service) openServiceRelay(ctx context.Context, id, name string, req serviceRelayRequest) error {
	project, err := s.controllerProject(ctx, id)
	transport, ok := s.modelAccess.(serviceRelayTransport)
	deadline := time.Unix(req.Expires, 0)
	if !ok || err != nil || project == nil || project.JobDigest != req.JobDigest || len(req.Connection) != 36 || len(req.Container) != 64 || req.Port < 1 || req.Port > 65535 || !deadline.After(time.Now()) || deadline.After(time.Now().Add(10*time.Minute+30*time.Second)) {
		return errors.New("service relay refused")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || len(s.serviceRelays) >= 16 {
		return errors.New("service relay unavailable")
	}
	if s.serviceRelays == nil {
		s.serviceRelays = make(map[string]struct{})
	}
	if _, ok := s.serviceRelays[req.Connection]; ok {
		return nil
	}
	s.serviceRelays[req.Connection] = struct{}{}
	s.wg.Add(1)
	lifetime, cancel := context.WithDeadline(s.ctx, deadline)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() { s.mu.Lock(); delete(s.serviceRelays, req.Connection); s.mu.Unlock() }()
		socket, err := transport.ConnectServiceRelay(lifetime, req.Connection)
		if err != nil {
			return
		}
		defer socket.Close()
		stop := context.AfterFunc(lifetime, func() { _ = socket.Close() })
		defer stop()
		duplex, ok := socket.(interface {
			CloseWrite() error
			Done() <-chan struct{}
		})
		if !ok {
			return
		}
		go func() {
			select {
			case <-duplex.Done():
				cancel()
			case <-lifetime.Done():
			}
		}()
		if err := project.ForwardService(lifetime, s.rt, name, req.Container, req.Port, socket, socket, duplex.CloseWrite); err != nil && lifetime.Err() == nil {
			s.log.Warn("job service connection failed", "session_id", id)
		}
	}()
	return nil
}
