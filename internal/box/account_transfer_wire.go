package box

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
)

// Only the public invitation leaves the receiving installation. The reply
// is compact JWE (RSA-OAEP-256,
// A256GCM). Its authenticated kid binds the entire invitation, including target.
const (
	accountInvitePrefix = "coop-import-1."
	AccountBundleLimit  = 4 << 20
)

var transferBase64 = base64.RawURLEncoding.Strict()
var errTransfer = errors.New("invalid account transfer or wrong receive code")

type transferInvitation struct {
	Destination string `json:"destination"`
	Provider    string `json:"provider"`
	Account     string `json:"account"`
	Key         string `json:"key"`
}

type AccountReceiver struct {
	Code    string
	private *rsa.PrivateKey
	mailbox string
}

// The receiving installation retains a private key for each named inbox,
// independently of account sign-in and removal. Two offline sources can both
// export to the same code; neither loses its move if the first occupies the name.
func NewAccountReceiver(ctx context.Context, cfg *config.Config, provider, account string) (*AccountReceiver, error) {
	if _, ok := agents.Get(provider); !ok || !accountNameValid(account) {
		return nil, errTransfer
	}
	root, err := openPrivateAccountTree(cfg.NativeAuthorityConfig().ConfigDir, "account-imports", provider, account)
	if err != nil {
		return nil, err
	}
	defer root.close()
	lock, err := root.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	der, err := safefile.ReadRegular(root.dir(), "receiver.der", 8192)
	var key *rsa.PrivateKey
	if errors.Is(err, os.ErrNotExist) {
		key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err == nil {
			err = writeAccountPrivate(root, "receiver.der", x509.MarshalPKCS1PrivateKey(key))
		}
	} else if err == nil {
		key, err = x509.ParsePKCS1PrivateKey(der)
	}
	if err != nil {
		return nil, err
	}
	if err = root.dir().Sync(); err != nil {
		return nil, err
	}
	inv := transferInvitation{"coop", provider, account, transferBase64.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey))}
	raw, err := json.Marshal(inv)
	if err != nil {
		return nil, err
	}
	return &AccountReceiver{Code: accountInvitePrefix + transferBase64.EncodeToString(raw), private: key, mailbox: root.path}, nil
}

func parseTransferInvitation(code, provider string) (transferInvitation, *rsa.PublicKey, error) {
	var inv transferInvitation
	encoded, ok := strings.CutPrefix(code, accountInvitePrefix)
	if !ok || len(code) > 4096 {
		return inv, nil, errTransfer
	}
	raw, err := transferBase64.DecodeString(encoded)
	if err != nil || json.Unmarshal(raw, &inv) != nil || inv.Provider != provider || !accountNameValid(inv.Account) || (inv.Destination != "coop" && inv.Destination != "ryker") {
		return inv, nil, errTransfer
	}
	der, err := transferBase64.DecodeString(inv.Key)
	if err != nil {
		return inv, nil, errTransfer
	}
	key, err := x509.ParsePKCS1PublicKey(der)
	if err != nil || key.N.BitLen() != 2048 || key.E != 65537 {
		return inv, nil, errTransfer
	}
	return inv, key, nil
}

type transferHeader struct {
	Algorithm  string `json:"alg"`
	Encryption string `json:"enc"`
	KeyID      string `json:"kid"`
}

func accountTransferHeader(code string) transferHeader {
	hash := sha256.Sum256([]byte(code))
	return transferHeader{"RSA-OAEP-256", "A256GCM", transferBase64.EncodeToString(hash[:])}
}

func sealTransfer(code string, public *rsa.PublicKey, plain []byte) (string, error) {
	header, err := json.Marshal(accountTransferHeader(code))
	if err != nil {
		return "", err
	}
	protected := transferBase64.EncodeToString(header)
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return "", err
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, public, key, nil)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, aead.NonceSize())
	if _, err = rand.Read(iv); err != nil {
		return "", err
	}
	body := aead.Seal(nil, iv, plain, []byte(protected))
	end := len(body) - aead.Overhead()
	parts := []string{protected}
	for _, data := range [][]byte{wrapped, iv, body[:end], body[end:]} {
		parts = append(parts, transferBase64.EncodeToString(data))
	}
	out := strings.Join(parts, ".")
	if len(out) > AccountBundleLimit {
		return "", errors.New("account transfer exceeds size limit")
	}
	return out, nil
}

func (r *AccountReceiver) open(provider, account, bundle string) ([]byte, error) {
	if r.private == nil || len(bundle) > AccountBundleLimit {
		return nil, errTransfer
	}
	inv, _, err := parseTransferInvitation(r.Code, provider)
	if err != nil || inv.Account != account {
		return nil, errTransfer
	}
	parts := strings.SplitN(bundle, ".", 6)
	if len(parts) != 5 {
		return nil, errTransfer
	}
	var fields [5][]byte
	for i, p := range parts {
		fields[i], err = transferBase64.DecodeString(p)
		if err != nil {
			return nil, errTransfer
		}
	}
	var h transferHeader
	if json.Unmarshal(fields[0], &h) != nil || h != accountTransferHeader(r.Code) || len(fields[2]) != 12 || len(fields[4]) != 16 {
		return nil, errTransfer
	}
	key, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, r.private, fields[1], nil)
	if err != nil || len(key) != 32 {
		return nil, errTransfer
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errTransfer
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errTransfer
	}
	data, err := aead.Open(nil, fields[2], append(fields[3], fields[4]...), []byte(parts[0]))
	if err != nil {
		return nil, errTransfer
	}
	return data, nil
}
