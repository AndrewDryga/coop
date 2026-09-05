// Package workertls supplies ephemeral certificate fixtures for worker transport
// and real Unix-service integration tests. It never reads production trust or keys.
package workertls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

type CA struct {
	Certificate *x509.Certificate
	PEM         []byte
	key         *rsa.PrivateKey
}

func NewCA(t *testing.T) *CA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Worker Test CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{Certificate: certificate, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *CA) ServerCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Identity returns the wire response, not workerconnector's private response type:
// integration tests must still exercise that real decoder. Errors belong to the
// HTTP handler; Fatal from its goroutine would leave the request unanswered.
func (ca *CA) Identity(publicKeyPEM, workerID, workspaceRef string) (map[string]string, error) {
	block, rest := pem.Decode([]byte(publicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
		return nil, errors.New("missing or malformed worker public key")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: workerID},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute).Truncate(time.Second),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, publicKey, ca.key)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(der)
	return map[string]string{
		"ca_certificate_pem": string(ca.PEM), "certificate_expires_at": template.NotAfter.Format(time.RFC3339),
		"certificate_pem":    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"certificate_sha256": hex.EncodeToString(digest[:]), "worker_id": workerID, "workspace_ref": workspaceRef,
	}, nil
}
