package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const ControllerModelsEnv = "COOP_CONTROLLER_MODELS"

// ControllerModels names the owner-private worker socket. It carries no
// account credential and never enters the workload environment.
type ControllerModels struct {
	Socket    string `json:"socket"`
	Session   string `json:"session"`
	JobDigest string `json:"job_digest"`
}

func ControllerModelEnvironment(socket, session, digest string) (string, error) {
	b := ControllerModels{socket, session, digest}
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || session == "" || len(digest) != 64 {
		return "", errors.New("invalid controller model source")
	}
	raw, err := json.Marshal(b)
	return string(raw), err
}

func controllerModels() (*ControllerModels, error) {
	raw := os.Getenv(ControllerModelsEnv)
	if raw == "" {
		return nil, nil
	}
	var b ControllerModels
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if len(raw) > 8192 || d.Decode(&b) != nil || d.Decode(new(any)) != io.EOF || b.JobDigest != os.Getenv(ControllerJobEnv) {
		return nil, errors.New("controller model source does not match the job")
	}
	if _, err := ControllerModelEnvironment(b.Socket, b.Session, b.JobDigest); err != nil {
		return nil, err
	}
	return &b, nil
}

// ControllerModelAccount validates one requested account with the controller.
// Present bindings always fail closed; a refusal never falls back to a worker login.
func ControllerModelAccount(ctx context.Context, provider, account string) (bool, error) {
	source, err := controllerModels()
	if err != nil {
		return true, err
	}
	if source == nil {
		return false, nil
	}
	_, err = source.grant(ctx, "readiness", provider, account)
	return true, err
}

func (b *ControllerModels) client() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", b.Socket)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (b *ControllerModels) grant(ctx context.Context, run, provider, account string) (workerproto.ModelGrant, error) {
	var g workerproto.ModelGrant
	binding := workerproto.ModelGrantRequest{JobDigest: b.JobDigest, RunID: run, Provider: provider, Account: account}
	if err := binding.Validate(); err != nil {
		return g, err
	}
	body, err := json.Marshal(binding)
	if err != nil {
		return g, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/model-access/"+b.Session+"/grant", bytes.NewReader(body))
	if err != nil {
		return g, err
	}
	client := b.client()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return g, errors.New("worker model relay unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 403 {
			return g, workerproto.ErrModelGrantRefused
		}
		return g, errors.New("controller refused model access")
	}
	d := json.NewDecoder(io.LimitReader(resp.Body, 8193))
	d.DisallowUnknownFields()
	if d.Decode(&g) != nil || d.Decode(new(any)) != io.EOF || g.ModelGrantRequest != binding || g.Validate(time.Now()) != nil {
		return g, errors.New("invalid model grant")
	}
	return g, nil
}
