package workerconnector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

const maxWorkerConfigBytes = 256 << 10

type fileConfig struct {
	Version                int                      `json:"version"`
	WorkerID               string                   `json:"worker_id"`
	WorkspaceRef           string                   `json:"workspace_ref"`
	ResponderURL           string                   `json:"responder_url"`
	CAFile                 string                   `json:"ca_file"`
	IdentityFile           string                   `json:"identity_file"`
	EnrollmentTokenFile    string                   `json:"enrollment_token_file"`
	CoopSocket             string                   `json:"coop_socket"`
	JournalDir             string                   `json:"journal_dir"`
	SandboxDigest          string                   `json:"sandbox_digest"`
	PolicyDigests          map[string]string        `json:"policy_digests"`
	PolicyAuthorityDigests map[string]string        `json:"policy_authority_digests,omitempty"`
	Repositories           []workerproto.Repository `json:"repositories"`
	Capabilities           []workerproto.Capability `json:"capabilities"`
	Capacity               workerproto.Capacity     `json:"capacity"`
	PollIntervalMS         int                      `json:"poll_interval_ms"`
	RequestTimeoutMS       int                      `json:"request_timeout_ms"`
	RenewBeforeSeconds     int                      `json:"renew_before_seconds"`
}

type Config struct {
	CAFile              string
	CoopSocket          string
	EnrollmentTokenFile string
	Hello               workerproto.WorkerHello
	IdentityFile        string
	JournalDir          string
	PollInterval        time.Duration
	RequestTimeout      time.Duration
	ResponderURL        string
	RenewBefore         time.Duration
}

func LoadConfig(path, buildVersion string, now time.Time) (Config, error) {
	if !filepath.IsAbs(path) {
		return Config{}, errors.New("worker configuration path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Config{}, fmt.Errorf("stat worker configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxWorkerConfigBytes {
		return Config{}, errors.New("worker configuration must be a bounded regular file")
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read worker configuration: %w", err)
	}
	var raw fileConfig
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode worker configuration: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("worker configuration has trailing data")
	}
	if raw.Version != workerproto.Version {
		return Config{}, fmt.Errorf("unsupported worker configuration version %d", raw.Version)
	}
	for name, value := range map[string]string{
		"CA": raw.CAFile, "identity": raw.IdentityFile,
		"Coop socket": raw.CoopSocket, "journal directory": raw.JournalDir,
	} {
		if !filepath.IsAbs(value) {
			return Config{}, fmt.Errorf("worker %s path must be absolute", name)
		}
	}
	if raw.EnrollmentTokenFile != "" && !filepath.IsAbs(raw.EnrollmentTokenFile) {
		return Config{}, errors.New("worker enrollment token path must be absolute")
	}
	if raw.PollIntervalMS < 100 || raw.PollIntervalMS > 60_000 || raw.RequestTimeoutMS < 100 || raw.RequestTimeoutMS > 300_000 {
		return Config{}, errors.New("worker poll interval or request timeout is invalid")
	}
	if raw.RenewBeforeSeconds == 0 {
		raw.RenewBeforeSeconds = 3_600
	}
	if raw.RenewBeforeSeconds < 60 || raw.RenewBeforeSeconds > 86_400 {
		return Config{}, errors.New("worker certificate renewal window is invalid")
	}
	hello := workerproto.WorkerHello{
		ID: raw.WorkerID, WorkspaceRef: raw.WorkspaceRef, ProtocolVersion: "1", BuildVersion: buildVersion,
		ClockAt: now, SandboxDigest: raw.SandboxDigest, PolicyDigests: raw.PolicyDigests,
		PolicyAuthorityDigests: raw.PolicyAuthorityDigests,
		Repositories:           raw.Repositories, Capabilities: raw.Capabilities, Capacity: raw.Capacity, State: "eligible",
	}
	probe := workerproto.Poll{Version: workerproto.Version, PollRef: "poll:" + raw.WorkerID + ":config", Worker: hello}
	if err := probe.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate worker authority configuration: %w", err)
	}
	if _, err := parseControlPlaneURL(raw.ResponderURL, true); err != nil {
		return Config{}, err
	}
	return Config{
		CAFile: raw.CAFile, CoopSocket: raw.CoopSocket, EnrollmentTokenFile: raw.EnrollmentTokenFile,
		Hello: hello, IdentityFile: raw.IdentityFile, JournalDir: raw.JournalDir,
		PollInterval:   time.Duration(raw.PollIntervalMS) * time.Millisecond,
		RequestTimeout: time.Duration(raw.RequestTimeoutMS) * time.Millisecond,
		ResponderURL:   raw.ResponderURL,
		RenewBefore:    time.Duration(raw.RenewBeforeSeconds) * time.Second,
	}, nil
}
