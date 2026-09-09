package networkgateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

func testPolicy(t *testing.T) egress.Snapshot {
	t.Helper()
	policy, err := egress.Compile("test", egress.Filtered, []egress.Input{{
		Rules: []egress.Rule{{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{443}},
			{To: egress.Destination{Domain: "*.services.example.com"}, Protocol: "tls", Ports: []int{443}}},
		Origin: egress.Origin{Kind: "operator"},
	}}, nil, false, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

type wireConn struct {
	*bytes.Reader
	written   bytes.Buffer
	stopWrite bool
}

func (c *wireConn) Write(p []byte) (int, error) {
	_, _ = c.written.Write(p)
	if c.stopWrite {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}
func (*wireConn) Close() error                     { return nil }
func (*wireConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*wireConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*wireConn) SetDeadline(time.Time) error      { return nil }
func (*wireConn) SetReadDeadline(time.Time) error  { return nil }
func (*wireConn) SetWriteDeadline(time.Time) error { return nil }

func clientHello(t *testing.T, name string) []byte {
	t.Helper()
	conn := &wireConn{Reader: bytes.NewReader(nil), stopWrite: true}
	client := tls.Client(conn, &tls.Config{ServerName: name, InsecureSkipVerify: true}) // fixture never receives a server certificate
	if err := client.Handshake(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("fixture ClientHello: %v", err)
	}
	return bytes.Clone(conn.written.Bytes())
}

func TestInspectTLSUsesMaintainedParserWithoutWritingOrChangingBytes(t *testing.T) {
	policy := testPolicy(t)
	for _, name := range []string{"api.example.com", "API.EXAMPLE.COM", "api.example.com.", "one.services.example.com"} {
		t.Run(name, func(t *testing.T) {
			wire := clientHello(t, name)
			conn := &wireConn{Reader: bytes.NewReader(wire)}
			got, err := Inspect(context.Background(), conn, policy)
			if err != nil || got.Name == "" || got.RuleID == "" || !bytes.Equal(got.Bytes, wire) {
				t.Fatalf("inspection: %#v, %v", got, err)
			}
			if conn.written.Len() != 0 {
				t.Fatal("inspection wrote a TLS alert or server handshake")
			}
		})
	}
}

func TestInspectRefusesNamesAndInvalidModes(t *testing.T) {
	policy := testPolicy(t)
	for _, name := range []string{"", "other.example.com", "services.example.com", "a.b.services.example.com", "badservices.example.com", "127.0.0.1"} {
		conn := &wireConn{Reader: bytes.NewReader(clientHello(t, name))}
		if got, err := Inspect(context.Background(), conn, policy); err == nil || len(got.Bytes) != 0 || conn.written.Len() != 0 {
			t.Fatalf("admitted %q: %#v %v", name, got, err)
		}
	}
	for _, mode := range []egress.Mode{egress.None, "", "garbage"} {
		policy.Mode = mode
		if _, err := Inspect(context.Background(), &wireConn{Reader: bytes.NewReader(clientHello(t, "api.example.com"))}, policy); err == nil {
			t.Fatalf("admitted mode %q", mode)
		}
	}
}

// addExtension modifies only a generated test fixture, never production traffic.
func addExtension(t *testing.T, wire []byte, kind uint16, body []byte) []byte {
	t.Helper()
	result := bytes.Clone(wire)
	if len(result) < 44 || int(binary.BigEndian.Uint16(result[3:5])) != len(result)-5 {
		t.Fatal("expected one generated ClientHello record")
	}
	pos := 5 + 4 + 2 + 32
	pos += 1 + int(result[pos])
	pos += 2 + int(binary.BigEndian.Uint16(result[pos:]))
	pos += 1 + int(result[pos])
	extLen := int(binary.BigEndian.Uint16(result[pos:]))
	if pos+2+extLen != len(result) {
		t.Fatal("bad fixture extension offset")
	}
	extra := make([]byte, 4+len(body))
	binary.BigEndian.PutUint16(extra, kind)
	binary.BigEndian.PutUint16(extra[2:], uint16(len(body)))
	copy(extra[4:], body)
	result = append(result, extra...)
	binary.BigEndian.PutUint16(result[pos:], uint16(extLen+len(extra)))
	binary.BigEndian.PutUint16(result[3:], uint16(len(result)-5))
	handshake := len(result) - 9
	result[6], result[7], result[8] = byte(handshake>>16), byte(handshake>>8), byte(handshake)
	return result
}

func TestInspectRejectsEveryECHOffer(t *testing.T) {
	policy := testPolicy(t)
	for _, body := range [][]byte{nil, {0}, {1}, {0, 0, 1, 0, 1, 0, 0, 0, 0}, bytes.Repeat([]byte{1}, 128)} {
		wire := addExtension(t, clientHello(t, "api.example.com"), echExtension, body)
		conn := &wireConn{Reader: bytes.NewReader(wire)}
		if _, err := Inspect(context.Background(), conn, policy); err == nil {
			t.Fatal("admitted ECH extension")
		}
		if conn.written.Len() != 0 {
			t.Fatal("ECH inspection wrote to client")
		}
	}
	// Unknown ordinary GREASE is not ECH and must remain compatible.
	wire := addExtension(t, clientHello(t, "api.example.com"), 0x4a4a, []byte{0, 1})
	if _, err := Inspect(context.Background(), &wireConn{Reader: bytes.NewReader(wire)}, policy); err != nil {
		t.Fatalf("ordinary GREASE rejected: %v", err)
	}
}

func TestInspectFragmentedTruncatedAndOversizedHello(t *testing.T) {
	policy := testPolicy(t)
	wire := clientHello(t, "api.example.com")
	var fragmented []byte
	for payload := wire[5:]; len(payload) > 0; {
		n := min(7, len(payload))
		fragmented = append(fragmented, 22, 3, 1, 0, byte(n))
		fragmented = append(fragmented, payload[:n]...)
		payload = payload[n:]
	}
	got, err := Inspect(context.Background(), &wireConn{Reader: bytes.NewReader(fragmented)}, policy)
	if err != nil || !bytes.Equal(got.Bytes, fragmented) {
		t.Fatalf("fragmented hello: %v", err)
	}
	for _, invalid := range [][]byte{nil, []byte("GET / HTTP/1.1\r\n\r\n"), wire[:len(wire)-1], bytes.Repeat([]byte{22}, MaxHelloBytes+1)} {
		if _, err := Inspect(context.Background(), &wireConn{Reader: bytes.NewReader(invalid)}, policy); err == nil {
			t.Fatal("admitted invalid input")
		}
	}
	// Declare a structurally fragmented handshake larger than the admission budget.
	oversized := []byte{1, 1, 0, 0}
	oversized = append(oversized, bytes.Repeat([]byte{0}, MaxHelloBytes)...)
	var records []byte
	for len(oversized) > 0 {
		n := min(16000, len(oversized))
		records = append(records, 22, 3, 1, byte(n>>8), byte(n))
		records = append(records, oversized[:n]...)
		oversized = oversized[n:]
	}
	if _, err := Inspect(context.Background(), &wireConn{Reader: bytes.NewReader(records)}, policy); err == nil {
		t.Fatal("admitted over-budget fragmented hello")
	}
}

func TestInspectCancellationReleasesSilentClient(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, server, testPolicy(t)); err == nil {
		t.Fatal("cancelled inspection succeeded")
	}
}

func FuzzInspectNeverApprovesWithoutCapturedName(f *testing.F) {
	f.Add([]byte("not TLS"))
	f.Add([]byte{22, 3, 1, 0, 0})
	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) > MaxHelloBytes+1 {
			t.Skip()
		}
		conn := &wireConn{Reader: bytes.NewReader(wire)}
		got, err := Inspect(context.Background(), conn, testPolicy(t))
		if conn.written.Len() != 0 || len(got.Bytes) > MaxHelloBytes || (err == nil && (got.Name == "" || got.RuleID == "")) {
			t.Fatal("inspection violated its boundary")
		}
	})
}
