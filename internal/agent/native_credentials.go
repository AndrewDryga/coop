package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// NativeCredentialArtifact is a bounded native input to the host-only account
// authority. Import retains renewal grants but strips settings and history.
type NativeCredentialArtifact struct {
	Name       string
	Limit      int64
	Required   bool
	Import     func([]byte) ([]byte, error)
	AccessOnly func([]byte) ([]byte, error) // explicit nonrenewable export for isolated live tests
}

// NativeCredentialState separates stable account identity from usability of a
// particular access grant. Short issued rotations still need durable custody.
type NativeCredentialState struct {
	Selection   string
	Principal   string
	AccessToken string
	AccountID   string
	ExpiresAt   time.Time
	Refreshable bool
	Ready       bool
	APIKey      bool
}

type NativeCredentialSpec struct {
	Environment  func(selection, key, value string) (NativeCredentialState, error)
	Defaults     func(string) (map[string][]byte, error)
	LegacyCheck  func(string) error
	LegacyGrants []string
	LegacyLocks  []string
	Artifacts    []NativeCredentialArtifact
	Select       func(map[string][]byte) (map[string][]byte, error)
	Inspect      func(map[string][]byte, time.Time) (NativeCredentialState, error)
	Renew        func(context.Context, map[string][]byte, time.Time, func([]byte) error) (map[string][]byte, error)
	Broker       NativeBrokerSpec
}

func nativeAPIEnvironment(family, envKey string) func(string, string, string) (NativeCredentialState, error) {
	return func(selection, key, value string) (NativeCredentialState, error) {
		if selection != "" && selection != family || key != envKey || !validNativeGrant(value) {
			return NativeCredentialState{}, errors.New("unsupported native API-key selection")
		}
		return NativeCredentialState{Selection: family, Principal: "opaque-api-key", AccessToken: value, Ready: true, APIKey: true}, nil
	}
}

// Seed accepts only the selected family, never credential bytes. These public
// selectors are not capabilities: the broker's private run namespace admits.
type NativeBrokerSpec struct {
	BindStorageIdentity bool
	MergeJSON           []string // public settings artifacts that may already contain native defaults
	Env                 func(selection, effort string) (map[string]string, error)
	CheckFiles          []NativeCredentialArtifact
	Seed                func(string) (NativeBrokerSeed, error)
	Routes              func(string) ([]NativeBrokerRoute, error)
	Check               func(map[string][]byte, NativeBrokerSeed) error
}
type NativeBrokerSeed struct {
	StorageHostname, StorageUsername string
	Family                           string // bounded, non-secret network family label
	Files                            map[string][]byte
	Env                              map[string]string
	Helper                           []byte
	Marker                           string
}
type NativeBrokerRoute struct {
	CredentialFree            bool
	Host, Method, Path, Query string
	Segment, Suffix           string
	Queries                   []NativeBrokerQuery
	Header, HeaderPrefix      string
	AccountHeader             string
}
type NativeBrokerQuery struct {
	Name     string
	Values   []string
	Optional bool
	MaxBytes int
}

const NativeBrokerAccount = "coop-broker-account"
const nativeBrokerUser = "coop-broker-user"
const nativeBrokerEmail = "account-selected-in-coop@broker.invalid"
const nativeBrokerExpiry int64 = 4102444800

func publicNativeSeed(marker string) NativeBrokerSeed {
	return NativeBrokerSeed{Files: map[string][]byte{}, Env: map[string]string{}, Marker: marker}
}

var errNativeBrokerDiverged = errors.New("native credentials changed or were removed; preserved for recovery - use host sign-in and explicitly recover this native home before retrying")

func nativePublicObject(data []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if len(data) == 0 || len(data) > 1<<20 || json.Unmarshal(data, &doc) != nil || doc == nil {
		return nil, errNativeBrokerDiverged
	}
	return doc, nil
}
func nativePublicString(doc map[string]json.RawMessage, key string) (string, error) {
	raw, ok := doc[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", errNativeBrokerDiverged
	}
	return value, nil
}
func brokerJWT(claims any) (string, error) {
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".Y29vcC1icm9rZXI", nil
}

func nativeJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil || len(data) >= 1<<20 {
		return nil, errors.New("native credential serialization failed")
	}
	return append(data, '\n'), nil
}

func cloneNativeFiles(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for name, data := range files {
		out[name] = bytes.Clone(data)
	}
	return out
}

func nativeTuple(parts ...string) string {
	data, _ := json.Marshal(parts)
	return string(data)
}

func validNativeGrant(value string) bool {
	return value != "" && len(value) <= 32<<10 && !strings.ContainsAny(value, "\x00\r\n")
}

func validNativeIdentity(value string) bool {
	return value != "" && len(value) <= 4096 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validNativeIssuedStrings(data []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return false
	}
	for _, name := range names {
		if raw, exists := fields[name]; exists {
			var value string
			if json.Unmarshal(raw, &value) != nil || !validNativeGrant(value) {
				return false
			}
		}
	}
	return true
}

type nativeJWTClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Expiry  int64  `json:"exp"`
	Auth    struct {
		AccountID string `json:"chatgpt_account_id"`
		UserID    string `json:"chatgpt_user_id"`
	} `json:"https://api.openai.com/auth"`
}

func nativeJWT(token string) (nativeJWTClaims, error) {
	var claims nativeJWTClaims
	if !validNativeGrant(token) {
		return claims, errors.New("invalid native token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("invalid native token shape")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) > 16<<10 || json.Unmarshal(raw, &claims) != nil {
		return claims, errors.New("invalid native token claims")
	}
	return claims, nil
}

var nativeRefreshClient = &http.Client{Timeout: 20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// The caller writes renewal intent before invoking this helper. Even incomplete
// received bytes are retained before parsing or checking status: retrying an old
// grant after an uncertain response can destroy the remaining recovery path.
func requestNativeRefresh(ctx context.Context, endpoint, contentType string, body []byte, retain func([]byte) error) ([]byte, error) {
	if retain == nil {
		return nil, errors.New("native renewal requires durable response custody")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid native renewal request")
	}
	request.Header.Set("Content-Type", contentType)
	response, err := nativeRefreshClient.Do(request)
	if err != nil {
		return nil, errors.New("native renewal request failed; authority is uncertain")
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if len(raw) != 0 {
		if err := retain(bytes.Clone(raw[:min(len(raw), 1<<20)])); err != nil {
			return nil, errors.New("native renewal response custody could not be confirmed")
		}
	}
	if readErr != nil || len(raw) == 0 || len(raw) > 1<<20 {
		return nil, errors.New("native renewal response incomplete; authority is uncertain")
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("native renewal refused")
	}
	return raw, nil
}
