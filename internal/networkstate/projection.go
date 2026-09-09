package networkstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// MCPProjection binds a validated private transport shape to this owner.
// A public fingerprint cannot be used as a dictionary oracle for private URLs.
func (s *Store) MCPProjection(shape []byte) (string, error) {
	if len(shape) == 0 || len(shape) > MaxInputBytes {
		return "", errors.New("invalid MCP qualification shape")
	}
	if err := s.intactAuthority(); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("network-mcp-projection-v1\x00"))
	_, _ = mac.Write(shape)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
