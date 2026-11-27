package workerconnector

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const servicePacketBytes = 32 << 10

// One packet of credit in each direction keeps both the controller mailbox and
// the opposite socket bounded, even when one client stops reading indefinitely.
type serviceSocket struct {
	*websocket.Conn
	received        chan []byte
	ack             chan struct{}
	done            chan struct{}
	once            sync.Once
	readMu, writeMu sync.Mutex
	buffer          []byte
	readEnd         bool
}

func DialServiceSocket(ctx context.Context, address string, tlsConfig *tls.Config, bearer string) (net.Conn, error) {
	cfg, err := websocket.NewConfig(address, "https://coop.invalid")
	if err != nil || cfg.Location.Scheme != "wss" || cfg.Location.User != nil || tlsConfig == nil {
		return nil, errors.New("invalid service stream address")
	}
	cfg.TlsConfig = tlsConfig.Clone()
	cfg.TlsConfig.NextProtos = []string{"http/1.1"}
	if bearer != "" {
		cfg.Header.Set("Authorization", "Bearer "+bearer)
	}
	setup, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	ws, err := cfg.DialContext(setup)
	if err != nil {
		return nil, errors.New("service stream was refused")
	}
	ws.MaxPayloadBytes = servicePacketBytes + 1
	deadline, _ := setup.Deadline()
	_ = ws.SetDeadline(deadline)
	stop := context.AfterFunc(setup, func() { _ = ws.Close() })
	var ready []byte
	err = websocket.Message.Receive(ws, &ready)
	if !stop() || err != nil || !bytes.Equal(ready, []byte{4}) {
		_ = ws.Close()
		return nil, errors.New("service worker did not join the stream")
	}
	_ = ws.SetDeadline(time.Time{})
	s := &serviceSocket{Conn: ws, received: make(chan []byte, 1), ack: make(chan struct{}, 1), done: make(chan struct{})}
	go s.receive()
	return s, nil
}

func (s *serviceSocket) receive() {
	defer s.Close()
	fin := false
	for {
		var frame []byte
		if websocket.Message.Receive(s.Conn, &frame) != nil || len(frame) == 0 {
			return
		}
		switch frame[0] {
		case 1, 3:
			if fin || (frame[0] == 3 && len(frame) != 1) || (frame[0] == 1 && len(frame) == 1) {
				return
			}
			fin = frame[0] == 3
			select {
			case s.received <- frame[1:]:
			case <-s.done:
				return
			}
		case 2:
			if len(frame) != 1 {
				return
			}
			select {
			case s.ack <- struct{}{}:
			default:
				return
			}
		default:
			return
		}
	}
}

func (s *serviceSocket) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if s.readEnd {
		return 0, io.EOF
	}
	if len(s.buffer) == 0 {
		select {
		case s.buffer = <-s.received:
		case <-s.done:
			select {
			case s.buffer = <-s.received:
			default:
				return 0, io.ErrUnexpectedEOF
			}
		}
		if len(s.buffer) == 0 {
			s.readEnd = true
			return 0, io.EOF
		}
	}
	n := copy(p, s.buffer)
	s.buffer = s.buffer[n:]
	if len(s.buffer) == 0 {
		if err := websocket.Message.Send(s.Conn, []byte{2}); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *serviceSocket) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), servicePacketBytes)
		frame := make([]byte, n+1)
		frame[0] = 1
		copy(frame[1:], p[:n])
		if err := websocket.Message.Send(s.Conn, frame); err != nil {
			return written, err
		}
		select {
		case <-s.ack:
		case <-s.done:
			return written, io.ErrClosedPipe
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (s *serviceSocket) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return websocket.Message.Send(s.Conn, []byte{3})
}

func (s *serviceSocket) Done() <-chan struct{} { return s.done }

func (s *serviceSocket) Close() error {
	s.once.Do(func() {
		close(s.done)
		_ = s.Conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
		_ = s.Conn.Close()
	})
	return nil
}

// ConnectServiceRelay always uses the enrolled controller and its current client certificate.
func (t *HTTPTransport) ConnectServiceRelay(ctx context.Context, id string) (net.Conn, error) {
	if len(id) != 36 || strings.ContainsAny(id, "/\\?#") {
		return nil, errors.New("invalid relay reference")
	}
	client, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	rt, ok := client.Transport.(workerRoundTripper)
	if !ok || rt.TLSClientConfig == nil {
		return nil, errors.New("authenticated service transport unavailable")
	}
	target := *t.baseURL
	target.Scheme = "wss"
	target.Path = "/v1/coop-workers/service-relays/" + id
	return DialServiceSocket(ctx, target.String(), rt.Transport.Clone().TLSClientConfig, "")
}
