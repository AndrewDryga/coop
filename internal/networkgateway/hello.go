// Package networkgateway implements bounded admission at the container boundary.
// It never approves policy, terminates provider TLS, or parses application data.
package networkgateway

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	MaxHelloBytes = 16 << 10 // pinned Envoy tls_inspector's supported ceiling
	HelloTimeout  = 5 * time.Second
	echExtension  = 0xfe0d
)

// Failure is a stable safe reason, not a client-controlled parser error. The
// owner can explain it without reflecting raw bytes into logs or terminals.
type Failure string

func (f Failure) Error() string { return string(f) }

var errInspected = errors.New("accepted ClientHello inspection boundary")

type Hello struct {
	Name   string
	RuleID string
	Bytes  []byte
}

// Inspect uses Go's maintained TLS parser and returns the exact bytes to replay
// unchanged into the private forwarding connection. No TLS alert or server
// handshake reaches the client: only an explicit accepted callback can succeed.
func Inspect(ctx context.Context, conn net.Conn, policy egress.Snapshot) (Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, HelloTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetReadDeadline(deadline); err != nil {
		return Hello{}, Failure("tls_inspection_unavailable")
	}
	recorded := &helloConn{Conn: conn}
	var result Hello
	reason := Failure("tls_malformed")
	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetEncryptedClientHelloKeys: func(*tls.ClientHelloInfo) ([]tls.EncryptedClientHelloKey, error) {
			reason = Failure("tls_ech_unsupported")
			return nil, reason
		},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			// The keys callback runs only for nonempty ECH bodies. Checking
			// extension presence also refuses empty and GREASE ECH offers.
			if slices.Contains(hello.Extensions, uint16(echExtension)) {
				reason = Failure("tls_ech_unsupported")
				return nil, reason
			}
			if hello.ServerName == "" {
				reason = Failure("tls_name_missing")
				return nil, reason
			}
			name, err := egress.NormalizeDomain(hello.ServerName, false)
			if err != nil {
				reason = Failure("tls_name_invalid")
				return nil, reason
			}
			result.Name = name // safe observed SNI is evidence even when refused
			decision := policy.Domain(name, 443)
			if !decision.Allowed {
				reason = Failure(decision.Reason)
				return nil, reason
			}
			result.Name, result.RuleID = name, decision.RuleID
			return nil, errInspected
		},
	}
	err := tls.Server(recorded, config).HandshakeContext(ctx)
	if ctx.Err() != nil {
		return Hello{}, Failure("tls_inspection_timeout")
	}
	if !errors.Is(err, errInspected) || result.Name == "" {
		if recorded.exhausted {
			reason = Failure("tls_hello_too_large")
		}
		return result, reason // never replay bytes or a rule grant on failure
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return Hello{}, Failure("tls_inspection_unavailable")
	}
	result.Bytes = recorded.data
	return result, nil
}

type helloConn struct {
	net.Conn
	data      []byte
	exhausted bool
}

func (c *helloConn) Read(p []byte) (int, error) {
	remaining := MaxHelloBytes - len(c.data)
	if remaining == 0 {
		c.exhausted = true
		return 0, io.ErrShortBuffer
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := c.Conn.Read(p)
	c.data = append(c.data, p[:n]...)
	return n, err
}

func (*helloConn) Write(p []byte) (int, error) { return len(p), nil }
