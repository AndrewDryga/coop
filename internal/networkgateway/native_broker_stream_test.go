package networkgateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// Only the short unfragmented text frames used by this fixture are accepted.
func nativeTestWSRead(reader io.Reader, masked bool) (string, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return "", err
	}
	if header[0] != 0x81 || (header[1]&0x80 != 0) != masked || header[1]&0x7f >= 126 {
		return "", fmt.Errorf("unexpected fixture websocket frame")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return "", err
		}
	}
	payload := make([]byte, int(header[1]&0x7f))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return string(payload), nil
}

func nativeTestWSWrite(writer io.Writer, payload string, masked bool) error {
	if len(payload) >= 126 {
		return fmt.Errorf("fixture frame too large")
	}
	frame := []byte{0x81, byte(len(payload))}
	if masked {
		frame[1] |= 0x80
		frame = append(frame, 0, 0, 0, 0)
	}
	frame = append(frame, []byte(payload)...)
	_, err := writer.Write(frame)
	return err
}

func TestNativeBrokerStreamsSurviveRotationAndCloseOnInvalidation(t *testing.T) {
	endings := []struct {
		name   string
		change func(*NativeAccessSnapshot)
	}{
		{"revoked", func(s *NativeAccessSnapshot) { s.Revoked = true }},
		{"expired", func(s *NativeAccessSnapshot) { s.Expires = time.Now().Add(-time.Second); s.Revision++ }},
		{"rollback", func(s *NativeAccessSnapshot) { s.Revision-- }},
		{"account-replaced", func(s *NativeAccessSnapshot) { s.Binding.Epoch++ }},
		{"same-revision-mutated", func(s *NativeAccessSnapshot) { s.Credential = "private-access-invalid" }},
	}
	for _, kind := range []string{"sse", "ws"} {
		for _, ending := range endings {
			t.Run(kind+"/"+ending.name, func(t *testing.T) {
				continueSSE, upstreamClosed := make(chan struct{}), make(chan struct{})
				f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/http":
						_, _ = io.WriteString(w, r.Header.Get("Authorization"))
					case "/sse":
						defer close(upstreamClosed)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: live\n\n")
						if http.NewResponseController(w).Flush() != nil {
							return
						}
						select {
						case <-r.Context().Done():
							return
						case <-continueSSE:
						}
						_, _ = io.WriteString(w, "data: rotated\n\n")
						if http.NewResponseController(w).Flush() != nil {
							return
						}
						<-r.Context().Done()
					case "/ws":
						conn, buffered, err := http.NewResponseController(w).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						defer close(upstreamClosed)
						defer conn.Close()
						_ = conn.SetDeadline(time.Now().Add(2 * wait.Deadline))
						if r.Header.Get("Sec-WebSocket-Key") != "dGhlIHNhbXBsZSBub25jZQ==" {
							t.Error("websocket handshake key changed")
							return
						}
						_, err = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
						if err != nil || buffered.Flush() != nil {
							return
						}
						if nativeTestWSWrite(conn, "live", false) != nil {
							return
						}
						for {
							text, err := nativeTestWSRead(buffered.Reader, true)
							if err != nil {
								return
							}
							if nativeTestWSWrite(conn, text, false) != nil {
								return
							}
						}
					default:
						http.NotFound(w, r)
					}
				}))
				f.client.Timeout = 2 * wait.Deadline
				var read func() (string, error)
				var write func(string) error
				if kind == "sse" {
					response, err := f.client.Do(f.request(t, "/sse"))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = response.Body.Close() })
					if response.StatusCode != http.StatusOK || response.ProtoMajor != 2 {
						t.Fatal("SSE did not use native h2")
					}
					reader := bufio.NewReader(response.Body)
					read = func() (string, error) {
						line, err := reader.ReadString('\n')
						if err != nil {
							return "", err
						}
						blank, err := reader.ReadString('\n')
						if err != nil {
							return "", err
						}
						if blank != "\n" || !strings.HasPrefix(line, "data: ") {
							return "", fmt.Errorf("invalid SSE fixture event")
						}
						return strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n"), nil
					}
					write = func(string) error { close(continueSSE); return nil }
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
					defer cancel()
					conn, err := f.tunnel(ctx, "example.com", []string{"http/1.1"})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
					_ = conn.SetDeadline(time.Now().Add(2 * wait.Deadline))
					request := f.request(t, "/ws")
					request.Header.Set("Connection", "Upgrade")
					request.Header.Set("Upgrade", "websocket")
					request.Header.Set("Sec-WebSocket-Version", "13")
					request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
					if err := request.Write(conn); err != nil {
						t.Fatal(err)
					}
					reader := bufio.NewReader(conn)
					response, err := http.ReadResponse(reader, request)
					if err != nil {
						t.Fatal(err)
					}
					if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
						t.Fatal("native websocket upgrade changed")
					}
					read = func() (string, error) { return nativeTestWSRead(reader, false) }
					write = func(s string) error { return nativeTestWSWrite(conn, s, true) }
				}
				if got, err := read(); err != nil || got != "live" {
					t.Fatalf("initial %s: %q %v", kind, got, err)
				}
				f.change(func(s *NativeAccessSnapshot) { s.Revision++; s.Credential = "private-access-two" })
				response, body := nativeResponse(t, f.client, f.request(t, "/http"))
				if response.StatusCode != http.StatusOK || body != "Bearer private-access-two" {
					t.Fatal("new request did not use rotated current access")
				}
				if err := write("rotated"); err != nil {
					t.Fatal(err)
				}
				if got, err := read(); err != nil || got != "rotated" {
					t.Fatalf("rotation killed existing %s: %q %v", kind, got, err)
				}
				downstreamClosed := make(chan error, 1)
				go func() { _, err := read(); downstreamClosed <- err }()
				f.change(ending.change)
				select {
				case err := <-downstreamClosed:
					if err == nil {
						t.Fatal("invalidated stream unexpectedly continued")
					}
				case <-time.After(wait.Deadline):
					t.Fatal("invalidated downstream stream remained open")
				}
				select {
				case <-upstreamClosed:
				case <-time.After(wait.Deadline):
					t.Fatal("invalidated upstream stream remained open")
				}
				wait.For(t, "protected broker cancellation", func() bool { return f.broker.ctx.Err() != nil })
				f.change(func(s *NativeAccessSnapshot) {
					s.Binding = f.broker.binding
					s.Revoked = false
					s.Revision += 10
					s.Expires = time.Now().Add(time.Hour)
					s.Credential = "private-access-three"
				})
				ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
				defer cancel()
				conn, err := f.tunnel(ctx, "example.com", []string{"http/1.1"})
				if err == nil {
					_ = conn.Close()
					t.Fatal("revoked epoch resumed native service")
				}
				ordinary, err := (&http.Client{Timeout: wait.Deadline}).Get(f.proxy.URL + "/ordinary")
				if err != nil {
					t.Fatal(err)
				}
				ordinaryBody, err := io.ReadAll(ordinary.Body)
				_ = ordinary.Body.Close()
				if err != nil || ordinary.StatusCode != http.StatusOK || string(ordinaryBody) != "ordinary" {
					t.Fatal("native invalidation disabled ordinary fallback")
				}
			})
		}
	}
}
