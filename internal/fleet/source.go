package fleet

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// GitHubApp is configured by the controller operator. Only the resulting
// single-repository contents:read token crosses the worker connection.
type GitHubApp struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyFile string `json:"private_key_file"`
}

func (c *Controller) UseGitHubApp(app GitHubApp) error {
	if app.AppID <= 0 || app.InstallationID <= 0 {
		return errors.New("invalid GitHub App or installation ID")
	}
	f, err := os.Open(app.PrivateKeyFile)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 32<<10 {
		return errors.New("GitHub App key must be a private PEM file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 32<<10))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return errors.New("invalid GitHub App PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		v, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return errors.New("invalid GitHub App key")
		}
		key, _ = v.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 {
		return errors.New("GitHub App needs an RSA signing key")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c.sourceToken = func(ctx context.Context, repo int64) (sourceToken, error) {
		encode := base64.RawURLEncoding.EncodeToString
		now := time.Now()
		claims, _ := json.Marshal(map[string]any{"iss": fmt.Sprint(app.AppID), "iat": now.Unix() - 60, "exp": now.Unix() + 540})
		input := encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(claims)
		sum := sha256.Sum256([]byte(input))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			return sourceToken{}, err
		}
		data, _ := json.Marshal(map[string]any{"repository_ids": []int64{repo}, "permissions": map[string]string{"contents": "read"}})
		req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", app.InstallationID), bytes.NewReader(data))
		if err != nil {
			return sourceToken{}, err
		}
		req.Header.Set("Authorization", "Bearer "+input+"."+encode(sig))
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return sourceToken{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			return sourceToken{}, fmt.Errorf("GitHub token service returned %d", resp.StatusCode)
		}
		var token sourceToken
		if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&token); err != nil || len(token.Token) == 0 || len(token.Token) > 4096 || !token.ExpiresAt.After(now.Add(time.Minute)) {
			return sourceToken{}, errors.New("invalid GitHub grant response")
		}
		return token, nil
	}
	return nil
}

type sourceToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func hasRepository(s workerproto.JobSpec, want workerproto.RepositoryIdentity) bool {
	var visit func([]workerproto.JobSubmodule) bool
	visit = func(modules []workerproto.JobSubmodule) bool {
		for _, m := range modules {
			if m.RepositoryIdentity() == want || visit(m.Submodules) {
				return true
			}
		}
		return false
	}
	has := func(src workerproto.JobSource) bool { return src.RepositoryIdentity() == want || visit(src.Submodules) }
	if s.Source != nil && has(*s.Source) {
		return true
	}
	for _, other := range s.Companions {
		if has(other.Source) {
			return true
		}
	}
	return false
}

func (c *Controller) sourceGrant(w http.ResponseWriter, r *http.Request) {
	id, hash := peer(r)
	if id == "" {
		w.WriteHeader(403)
		return
	}
	var repo workerproto.RepositoryIdentity
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if dec.Decode(&repo) != nil || dec.Decode(&struct{}{}) != io.EOF || repo.Validate() != nil {
		w.WriteHeader(400)
		return
	}
	check := func() error {
		var b []byte
		err := c.db.QueryRowContext(r.Context(), `SELECT spec FROM jobs j JOIN workers w ON w.id=j.worker WHERE j.id=? AND j.worker=? AND w.fingerprint=? AND w.revoked=0 AND j.stage IN ('creating','starting','running')`, r.PathValue("job"), id, hash).Scan(&b)
		if err != nil {
			return err
		}
		spec, err := workerproto.DecodeJobSpec(b)
		if err != nil {
			return err
		}
		if !hasRepository(spec, repo) {
			return sql.ErrNoRows
		}
		return nil
	}
	allowed := func() bool {
		err := check()
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(403)
		} else if err != nil {
			w.WriteHeader(503)
		}
		return err == nil
	}
	if !allowed() {
		return
	}
	grant := struct {
		workerproto.RepositoryIdentity
		sourceToken
		Public bool `json:"public,omitempty"`
	}{RepositoryIdentity: repo}
	if c.sourceToken == nil {
		grant.Public = true
		grant.ExpiresAt = time.Now().Add(10 * time.Minute)
	} else {
		token, err := c.sourceToken(r.Context(), repo.GitHubRepositoryID)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		grant.sourceToken = token
		if !allowed() {
			return
		}
	}
	answer(w, 200, grant)
}
