package liveprovider

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/liveprocess"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const (
	// SummaryPrefix makes the one machine-readable line easy to extract from verbose go test output.
	SummaryPrefix = "COOP_PROVIDER_LIVE_SUMMARY "
	// SupervisorLabelKey is the test-only label used to reap an ACP process even after its outer
	// supervisor is force-killed before normal cleanup.
	SupervisorLabelKey = "coop.live-test"

	StatusPassed  = "passed"
	StatusSkipped = "skipped"
	StatusFailed  = "failed"

	ReasonMissingRuntime        = "missing_runtime"
	ReasonMissingImage          = "missing_image"
	ReasonMissingCredential     = "missing_credential"
	ReasonCredentialRefresh     = "credential_refresh_required"
	ReasonCredentialNotPortable = "credential_not_portable"
	ReasonMissingCLI            = "missing_cli"
	ReasonUnsafeCredential      = "unsafe_credential"
	ReasonVersionProbe          = "version_probe_failed"
	ReasonPromptExit            = "prompt_exit"
	ReasonPromptTimeout         = "prompt_timeout"
	ReasonMarkerMismatch        = "marker_mismatch"
	ReasonRepositoryChanged     = "repository_changed"
	ReasonSourceChanged         = "source_changed"
	ReasonCleanupFailed         = "cleanup_failed"
	ReasonHarnessFailed         = "harness_failed"
)

var allowedSkipReasons = map[string]bool{
	ReasonMissingRuntime: true, ReasonMissingImage: true,
	ReasonMissingCredential: true, ReasonCredentialRefresh: true,
	ReasonCredentialNotPortable: true, ReasonMissingCLI: true,
}

var allowedFailureReasons = map[string]bool{
	ReasonUnsafeCredential: true, ReasonVersionProbe: true, ReasonPromptExit: true,
	ReasonPromptTimeout: true, ReasonMarkerMismatch: true, ReasonRepositoryChanged: true,
	ReasonSourceChanged: true, ReasonCleanupFailed: true, ReasonHarnessFailed: true,
}

// ParseTargets resolves the explicit provider set for an opt-in live run. `all` is deliberately
// strict and registry-generated; explicit lists preserve their order and may allow prerequisite
// skips. Account ladders are rejected because one live prompt must consume one credential only.
func ParseTargets(raw string) ([]agents.Target, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false, errors.New("COOP_LIVE_TARGETS is required (provider targets or all)")
	}
	if raw == "all" {
		targets := make([]agents.Target, 0, len(agents.Names()))
		for _, provider := range agents.Names() {
			targets = append(targets, agents.Target{Provider: provider})
		}
		return targets, true, nil
	}
	parts := strings.Split(raw, ",")
	targets := make([]agents.Target, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "all" {
			return nil, false, errors.New("all cannot be combined with another live target")
		}
		target, err := agents.ParseTarget(part)
		if err != nil {
			return nil, false, err
		}
		if len(target.Accounts) > 1 {
			return nil, false, fmt.Errorf("%s live target selects more than one account", target.Provider)
		}
		if seen[target.Provider] {
			return nil, false, fmt.Errorf("%s appears more than once in COOP_LIVE_TARGETS", target.Provider)
		}
		seen[target.Provider] = true
		targets = append(targets, target)
	}
	return targets, false, nil
}

// ProviderResult is intentionally path/account/token-free so the summary can be retained as CI or
// task evidence. Attempted becomes true only when the marker command starts.
type ProviderResult struct {
	Provider   string `json:"provider"`
	CLIVersion string `json:"cli_version,omitempty"`
	Attempted  bool   `json:"attempted"`
	Passed     bool   `json:"passed"`
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code,omitempty"`
	Phase      string `json:"phase,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"output_truncated,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	DetailCode string `json:"detail_code,omitempty"`
}

type SummaryTotals struct {
	Requested int `json:"requested"`
	Attempted int `json:"attempted"`
	Passed    int `json:"passed"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
}

// Summary is the stable provider-live result schema.
type Summary struct {
	Schema  int              `json:"schema"`
	Strict  bool             `json:"strict"`
	Results []ProviderResult `json:"results"`
	Totals  SummaryTotals    `json:"totals"`
}

func NewSummary(strict bool, requested []agents.Target, results []ProviderResult) (Summary, error) {
	if err := ValidateStrictTargets(strict, requested); err != nil {
		return Summary{}, err
	}
	if len(results) != len(requested) {
		return Summary{}, fmt.Errorf("live result count %d does not match requested count %d", len(results), len(requested))
	}
	summary := Summary{Schema: 1, Strict: strict, Results: append([]ProviderResult(nil), results...)}
	summary.Totals.Requested = len(requested)
	seen := map[string]bool{}
	for i, result := range summary.Results {
		if result.Provider != requested[i].Provider || seen[result.Provider] {
			return Summary{}, errors.New("live results do not match the requested provider order")
		}
		seen[result.Provider] = true
		if !validCLIVersion(result.Provider, result.CLIVersion) {
			return Summary{}, fmt.Errorf("%s has an unsafe live CLI version", result.Provider)
		}
		for _, diagnostic := range []string{result.Phase, result.ErrorClass, result.DetailCode} {
			if !validDiagnosticCode(diagnostic) {
				return Summary{}, fmt.Errorf("%s has an invalid live diagnostic code", result.Provider)
			}
		}
		switch result.Status {
		case StatusPassed:
			if result.CLIVersion == "" || !result.Attempted || !result.Passed || result.ReasonCode != "" ||
				result.Phase != "" || result.ErrorClass != "" || result.DetailCode != "" ||
				result.TimedOut || result.Truncated || result.ExitCode != 0 {
				return Summary{}, fmt.Errorf("%s has an inconsistent passed result", result.Provider)
			}
			summary.Totals.Passed++
		case StatusSkipped:
			if result.Attempted || result.Passed || !allowedSkipReasons[result.ReasonCode] ||
				result.Phase != "" || result.ErrorClass != "" || result.DetailCode != "" ||
				result.TimedOut || result.Truncated || result.ExitCode != 0 {
				return Summary{}, fmt.Errorf("%s has an inconsistent skipped result", result.Provider)
			}
			summary.Totals.Skipped++
		case StatusFailed:
			if result.Passed || !validFailedResult(result) {
				return Summary{}, fmt.Errorf("%s has an inconsistent failed result", result.Provider)
			}
			summary.Totals.Failed++
		default:
			return Summary{}, fmt.Errorf("%s has unknown live status %q", result.Provider, result.Status)
		}
		if result.Attempted {
			summary.Totals.Attempted++
		}
	}
	return summary, nil
}

func validFailedResult(result ProviderResult) bool {
	if !allowedFailureReasons[result.ReasonCode] || result.Phase == "" || result.ErrorClass == "" {
		return false
	}
	switch result.ReasonCode {
	case ReasonUnsafeCredential:
		return !result.Attempted && result.Phase == "harness" && result.ErrorClass == "harness"
	case ReasonVersionProbe:
		return !result.Attempted && result.Phase == "version"
	case ReasonPromptExit:
		return result.Attempted && result.Phase == "prompt" && !result.TimedOut
	case ReasonPromptTimeout:
		return result.Attempted && result.Phase == "prompt" && result.TimedOut && result.ErrorClass == "timeout"
	case ReasonMarkerMismatch:
		return result.Attempted && result.Phase == "prompt" && !result.TimedOut
	case ReasonRepositoryChanged, ReasonSourceChanged, ReasonCleanupFailed:
		return result.Phase == "verification" && result.ErrorClass == "harness"
	case ReasonHarnessFailed:
		return result.Phase == "harness" && result.ErrorClass == "harness"
	default:
		return false
	}
}

func validCLIVersion(provider, version string) bool {
	if version == "" {
		return true
	}
	token, ok := strings.CutPrefix(version, provider+"-cli ")
	if !ok {
		return false
	}
	match := cliVersionToken.FindStringSubmatch(token)
	return len(match) == 2 && match[1] == token && !strings.Contains(token, "..")
}

func validDiagnosticCode(code string) bool {
	if len(code) > 64 {
		return false
	}
	for _, char := range code {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

// ValidateStrictTargets rejects an invalid paid target set before runtime detection or any prompt.
func ValidateStrictTargets(strict bool, requested []agents.Target) error {
	if !strict {
		return nil
	}
	registry := agents.Names()
	if len(requested) != len(registry) {
		return errors.New("strict live mode requires the complete provider registry")
	}
	for i, provider := range registry {
		if requested[i].Provider != provider {
			return errors.New("strict live mode requires registry order")
		}
	}
	return nil
}

// Success applies the operator contract: standard mode tolerates prerequisite skips only; strict
// mode requires every registered provider attempted and passed with no skip or failure.
func (s Summary) Success() bool {
	if s.Totals.Failed != 0 {
		return false
	}
	if !s.Strict {
		return true
	}
	return s.Totals.Requested == len(agents.Names()) &&
		s.Totals.Attempted == s.Totals.Requested &&
		s.Totals.Passed == s.Totals.Requested && s.Totals.Skipped == 0
}

func (s Summary) Line() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return SummaryPrefix + string(data), nil
}

// RuntimeSettings is the narrow host-runtime capability copied from the user's resolved Coop
// config. ConnectionEnv may point at runtime-owned configuration, so it must reach only the runtime
// process; it is never forwarded as container environment or persisted in live evidence.
type RuntimeSettings struct {
	Name          string
	Image         string
	BaseImage     string
	HomeInBox     string
	AgentPackages string
	ConnectionEnv map[string]string
}

var runtimeConnectionKeys = map[string][]string{
	"docker": {
		"DOCKER_HOST", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "SSH_AUTH_SOCK",
	},
	"podman": {
		"CONTAINER_HOST", "CONTAINER_SSHKEY", "CONTAINERS_STORAGE_CONF",
		"STORAGE_DRIVER", "STORAGE_OPTS", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "SSH_AUTH_SOCK",
	},
}

const runtimeQueryLimit = 256 << 10

// CaptureRuntimeConnectionEnv resolves the selected host runtime's endpoint/TLS/SSH/storage
// capability before starting a scrubbed child. It never forwards Docker or Podman behavior-bearing
// config: those files may inject proxy env, mounts, hooks, or other defaults into the live box.
func CaptureRuntimeConnectionEnv(runtimeName string) (map[string]string, error) {
	return captureRuntimeConnectionEnv(runtimeName, os.LookupEnv, pathExists, func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stdout := NewBoundedBuffer(runtimeQueryLimit)
		stderr := NewBoundedBuffer(64 << 10)
		cmd := exec.CommandContext(ctx, runtimeName, args...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil || stdout.Truncated() || stderr.Truncated() {
			return nil, errors.New("resolve live runtime connection")
		}
		return []byte(stdout.String()), nil
	})
}

func captureRuntimeConnectionEnv(
	runtimeName string,
	lookup func(string) (string, bool),
	exists func(string) bool,
	query func(...string) ([]byte, error),
) (map[string]string, error) {
	name := filepath.Base(runtimeName)
	switch name {
	case "docker":
		return captureDockerConnectionEnv(lookup, exists, query)
	case "podman":
		return capturePodmanConnectionEnv(lookup, exists, query)
	default:
		return map[string]string{}, nil
	}
}

func captureDockerConnectionEnv(
	lookup func(string) (string, bool),
	exists func(string) bool,
	query func(...string) ([]byte, error),
) (map[string]string, error) {
	contextName, _ := lookup("DOCKER_CONTEXT")
	host, _ := lookup("DOCKER_HOST")
	if contextName == "" && host != "" {
		values := selectedEnvironment(lookup, runtimeConnectionKeys["docker"])
		if err := validateConnectionEnvironment(values); err != nil {
			return nil, err
		}
		return values, nil
	}
	args := []string{"context", "inspect"}
	if contextName != "" {
		args = append(args, contextName)
	}
	data, err := query(args...)
	if err != nil {
		return nil, err
	}
	var contexts []struct {
		Endpoints map[string]struct {
			Host          string `json:"Host"`
			SkipTLSVerify bool   `json:"SkipTLSVerify"`
		} `json:"Endpoints"`
		TLSMaterial map[string]json.RawMessage `json:"TLSMaterial"`
		Storage     struct {
			TLSPath string `json:"TLSPath"`
		} `json:"Storage"`
	}
	if json.Unmarshal(data, &contexts) != nil || len(contexts) != 1 {
		return nil, errors.New("resolve live Docker context")
	}
	endpoint, ok := contexts[0].Endpoints["docker"]
	if !ok || endpoint.Host == "" {
		return nil, errors.New("resolve live Docker endpoint")
	}
	values := map[string]string{"DOCKER_HOST": endpoint.Host}
	if sshSocket, ok := lookup("SSH_AUTH_SOCK"); ok && strings.HasPrefix(endpoint.Host, "ssh://") && sshSocket != "" {
		values["SSH_AUTH_SOCK"] = sshSocket
	}
	if material := contexts[0].TLSMaterial["docker"]; hasJSONValue(material) {
		certDir := filepath.Join(contexts[0].Storage.TLSPath, "docker")
		if contexts[0].Storage.TLSPath == "" || !exists(certDir) {
			return nil, errors.New("resolve live Docker TLS material")
		}
		values["DOCKER_TLS"] = "1"
		values["DOCKER_CERT_PATH"] = certDir
		if !endpoint.SkipTLSVerify {
			values["DOCKER_TLS_VERIFY"] = "1"
		}
	}
	if err := validateConnectionEnvironment(values); err != nil {
		return nil, err
	}
	return values, nil
}

func capturePodmanConnectionEnv(
	lookup func(string) (string, bool),
	exists func(string) bool,
	query func(...string) ([]byte, error),
) (map[string]string, error) {
	values := selectedEnvironment(lookup, []string{
		"CONTAINERS_STORAGE_CONF", "STORAGE_DRIVER", "STORAGE_OPTS",
		"XDG_DATA_HOME", "XDG_RUNTIME_DIR", "SSH_AUTH_SOCK",
	})
	home, _ := lookup("HOME")
	configRoot, _ := lookup("XDG_CONFIG_HOME")
	if configRoot == "" && home != "" {
		configRoot = filepath.Join(home, ".config")
	}
	if values["CONTAINERS_STORAGE_CONF"] == "" && configRoot != "" {
		candidate := filepath.Join(configRoot, "containers", "storage.conf")
		if exists(candidate) {
			values["CONTAINERS_STORAGE_CONF"] = candidate
		}
	}
	if values["XDG_DATA_HOME"] == "" && home != "" {
		candidate := filepath.Join(home, ".local", "share")
		if exists(candidate) {
			values["XDG_DATA_HOME"] = candidate
		}
	}
	host, _ := lookup("CONTAINER_HOST")
	identity, _ := lookup("CONTAINER_SSHKEY")
	if host != "" {
		values["CONTAINER_HOST"] = host
		if identity != "" {
			values["CONTAINER_SSHKEY"] = identity
		}
		if err := validateConnectionEnvironment(values); err != nil {
			return nil, err
		}
		return values, nil
	}
	connectionName, _ := lookup("CONTAINER_CONNECTION")
	data, err := query("system", "connection", "list", "--format", "json")
	if err != nil {
		if connectionName == "" {
			return values, validateConnectionEnvironment(values)
		}
		return nil, err
	}
	var connections []struct {
		Name     string `json:"Name"`
		URI      string `json:"URI"`
		Identity string `json:"Identity"`
		Default  bool   `json:"Default"`
		TLSCA    string `json:"TLSCA"`
		TLSCert  string `json:"TLSCert"`
		TLSKey   string `json:"TLSKey"`
	}
	if json.Unmarshal(data, &connections) != nil {
		return nil, errors.New("resolve live Podman connection")
	}
	selected := -1
	for i, connection := range connections {
		if (connectionName != "" && connection.Name == connectionName) || (connectionName == "" && connection.Default) {
			selected = i
			break
		}
	}
	if connectionName != "" && selected < 0 {
		return nil, errors.New("resolve selected live Podman connection")
	}
	if selected >= 0 {
		connection := connections[selected]
		if connection.URI == "" || connection.TLSCA != "" || connection.TLSCert != "" || connection.TLSKey != "" {
			return nil, errors.New("selected live Podman connection is not safely portable")
		}
		values["CONTAINER_HOST"] = connection.URI
		if identity != "" {
			values["CONTAINER_SSHKEY"] = identity
		} else if connection.Identity != "" {
			values["CONTAINER_SSHKEY"] = connection.Identity
		}
	}
	if err := validateConnectionEnvironment(values); err != nil {
		return nil, err
	}
	return values, nil
}

func selectedEnvironment(lookup func(string) (string, bool), keys []string) map[string]string {
	values := map[string]string{}
	for _, key := range keys {
		if value, ok := lookup(key); ok && value != "" {
			values[key] = value
		}
	}
	return values
}

func validateConnectionEnvironment(values map[string]string) error {
	for key, value := range values {
		if strings.ContainsAny(key, "=\x00\r\n") || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid live runtime connection")
		}
	}
	return nil
}

func hasJSONValue(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null" && value != "{}" && value != "[]"
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ChildSpec is the complete authority granted to one clean live helper process.
type ChildSpec struct {
	Path            string
	Target          string
	Marker          string
	ResultFile      string
	AttemptFile     string
	Supervisor      string
	PreflightReason string
	CIDDir          string
	ControlFD       int
	RevokePath      string
	Runtime         RuntimeSettings
}

// ChildEnvironment builds an allowlist-only environment. It never reads os.Environ; callers must
// pass PATH and resolved runtime settings explicitly. Provider tokens live only in layout.Config/env.
func ChildEnvironment(layout procharness.Layout, spec ChildSpec) ([]string, error) {
	if spec.PreflightReason != "" && !map[string]bool{
		ReasonMissingCredential:     true,
		ReasonCredentialRefresh:     true,
		ReasonCredentialNotPortable: true,
	}[spec.PreflightReason] {
		return nil, errors.New("invalid live child preflight reason")
	}
	values := processEnvironmentValues(layout, spec.Path, spec.Runtime)
	if spec.ControlFD != 0 {
		if spec.ControlFD != 3 || !liveprocess.ValidRevocationPath(layout.Config, spec.RevokePath) {
			return nil, errors.New("invalid live process control descriptor")
		}
		values[liveprocess.ControlFDEnv] = "3"
		values[liveprocess.RevokePathEnv] = spec.RevokePath
	} else if spec.RevokePath != "" {
		return nil, errors.New("live credential revocation path requires process control")
	}
	for key, value := range map[string]string{
		"COOP_TEST_LIVE_CHILD": "1", "COOP_TEST_LIVE_TARGET": spec.Target,
		"COOP_TEST_LIVE_MARKER": spec.Marker, "COOP_TEST_LIVE_RESULT": spec.ResultFile,
		"COOP_TEST_LIVE_ATTEMPT":    spec.AttemptFile,
		"COOP_TEST_LIVE_SUPERVISOR": spec.Supervisor,
		"COOP_TEST_LIVE_PREFLIGHT":  spec.PreflightReason,
		"COOP_TEST_LIVE_CID_DIR":    spec.CIDDir,
	} {
		values[key] = value
	}
	return encodeEnvironment(values)
}

// ProcessSpec is the authority granted to a live ACP supervisor. ProcessDir is a private,
// append-only registry activated only when the tagged binary also receives ControlFD.
type ProcessSpec struct {
	Supervisor string
	ProcessDir string
	ControlFD  int
}

// ProcessEnvironment is the shared allowlist-only environment for a live test process. It grants
// runtime connectivity and the isolated Coop layout, but no ambient Coop overrides or provider keys.
func ProcessEnvironment(layout procharness.Layout, path string, runtime RuntimeSettings, spec ProcessSpec) ([]string, error) {
	values := processEnvironmentValues(layout, path, runtime)
	if spec.Supervisor != "" {
		if !liveprocess.ValidCleanupID(spec.Supervisor) {
			return nil, errors.New("invalid live process supervisor")
		}
		values["COOP_RUN_ARGS"] = "--label " + SupervisorLabelKey + "=" + spec.Supervisor
	}
	if spec.ProcessDir != "" || spec.ControlFD != 0 {
		if spec.ControlFD != 3 || spec.ProcessDir == "" {
			return nil, errors.New("incomplete live ACP process control")
		}
		if err := validatePrivateControlDir(layout.Root, spec.ProcessDir); err != nil {
			return nil, err
		}
		values[liveprocess.ControlFDEnv] = "3"
		values[liveprocess.ProcessDirEnv] = spec.ProcessDir
		values[liveprocess.CleanupIDEnv] = spec.Supervisor
	}
	return encodeEnvironment(values)
}

// NewProcessControl creates the authenticated descriptor shared by tagged live helpers. ACP also
// requests a private generation registry; direct probes need only the descriptor.
func NewProcessControl(layout procharness.Layout, registry bool) (*os.File, string, error) {
	controlPath := filepath.Join(layout.State, "live-process-control")
	control, err := os.OpenFile(controlPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", errors.New("create live process control")
	}
	if _, err := io.WriteString(control, liveprocess.ControlMarker); err != nil {
		control.Close()
		return nil, "", errors.New("write live process control")
	}
	if err := control.Sync(); err != nil {
		control.Close()
		return nil, "", errors.New("sync live process control")
	}
	if err := control.Close(); err != nil {
		return nil, "", errors.New("close live process control")
	}
	control, err = procharness.OpenRegularFile(layout.Root, controlPath, os.O_RDONLY)
	if err != nil {
		return nil, "", errors.New("open live process control")
	}
	processDir := ""
	if registry {
		processDir = filepath.Join(layout.XDGState, "live-process-groups")
		if err := os.Mkdir(processDir, 0o700); err != nil {
			control.Close()
			return nil, "", errors.New("create live process registry")
		}
		if err := validatePrivateControlDir(layout.Root, processDir); err != nil {
			control.Close()
			return nil, "", err
		}
	}
	return control, processDir, nil
}

func validatePrivateControlDir(root, path string) error {
	clean, err := procharness.CanonicalUnderRoot(root, path)
	if err != nil {
		return errors.New("invalid live process registry")
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("invalid live process registry")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return errors.New("invalid live process registry")
	}
	return nil
}

func processEnvironmentValues(layout procharness.Layout, path string, runtime RuntimeSettings) map[string]string {
	values := map[string]string{
		"PATH": path, "HOME": layout.Home,
		"XDG_CONFIG_HOME": layout.XDGConfig, "XDG_CACHE_HOME": layout.XDGCache,
		"XDG_STATE_HOME": layout.XDGState, "TMPDIR": layout.Tmp,
		"GIT_CONFIG_GLOBAL": layout.GitConfig, "GIT_CONFIG_NOSYSTEM": "1",
		"LANG": "C", "LC_ALL": "C", "TERM": "dumb", "TZ": "UTC",
		"COOP_CONF": filepath.Join(layout.Config, "missing.conf"), "COOP_CONFIG_DIR": layout.Config,
		"COOP_MCP_FILE": filepath.Join(layout.Config, "missing-mcp.json"), "COOP_REPO": layout.Repo,
		"COOP_WORKDIR": "", "COOP_HOMES": "1", "COOP_NETWORK": "0", "COOP_AUTO_UP": "0",
		"COOP_CACHE": "0", "COOP_CAFFEINATE": "0", "COOP_EGRESS": "open",
		"COOP_NO_UPDATE_CHECK": "1", "COOP_RUN_ARGS": "", "COOP_MEMORY": "", "COOP_CPUS": "",
		"COOP_ACP_WARM": "0",
	}
	for key, value := range map[string]string{
		"COOP_RUNTIME": runtime.Name, "COOP_IMAGE": runtime.Image,
		"COOP_BASE_IMAGE": runtime.BaseImage, "COOP_HOME_IN_BOX": runtime.HomeInBox,
		"COOP_AGENT_PACKAGES": runtime.AgentPackages,
	} {
		if value != "" {
			values[key] = value
		}
	}
	for _, key := range runtimeConnectionKeys[filepath.Base(runtime.Name)] {
		if value := runtime.ConnectionEnv[key]; value != "" {
			values[key] = value
		}
	}
	return values
}

func encodeEnvironment(values map[string]string) ([]string, error) {
	for key, value := range values {
		if strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("invalid live child environment")
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

// BoundedBuffer captures subprocess output without applying backpressure after the cap. Write
// always reports success so a verbose provider cannot turn output truncation into a pipe failure.
type BoundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	max       int
	truncated bool
}

func NewBoundedBuffer(max int) *BoundedBuffer {
	if max <= 0 {
		max = 1 << 20
	}
	return &BoundedBuffer{max: max}
}

func (b *BoundedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.max - len(b.data)
	if remaining > 0 {
		keep := len(data)
		if keep > remaining {
			keep = remaining
		}
		b.data = append(b.data, data[:keep]...)
	}
	if len(data) > remaining {
		b.truncated = true
	}
	return len(data), nil
}

func (b *BoundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}

func (b *BoundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

var cliVersionToken = regexp.MustCompile(`^v?([0-9]{1,10}\.[0-9]{1,10}\.[0-9]{1,10}(?:\.[0-9]{1,10})?(?:-[0-9A-Za-z][0-9A-Za-z.-]{0,31})?(?:\+[0-9A-Za-z][0-9A-Za-z.-]{0,31})?)$`)

// CLIVersion extracts only a semver-like token and composes it with a registry-trusted label. Raw
// --version lines can contain paths, control sequences, environment echoes, or other secret data;
// none of that belongs in the stable live summary.
func CLIVersion(provider string, outputs ...string) string {
	if !agents.Valid(provider) {
		return ""
	}
	for _, output := range outputs {
		for _, line := range strings.Split(output, "\n") {
			if len(line) > 512 || !utf8.ValidString(line) || strings.Contains(line, "=") {
				continue
			}
			for _, token := range strings.FieldsFunc(line, func(r rune) bool {
				return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
					(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '+')
			}) {
				match := cliVersionToken.FindStringSubmatch(token)
				if len(match) == 2 && !strings.Contains(match[1], "..") {
					return provider + "-cli " + match[1]
				}
			}
		}
	}
	return ""
}

// RepositorySnapshot covers the Git and working-tree state a read-only live command must preserve.
type RepositorySnapshot struct {
	Head, Status, Refs, Reflog string
	Tree                       [32]byte
}

// InitRepository creates the clean committed repo used by one provider. Git config is isolated by
// the process layout, so ambient aliases/hooks/signing cannot affect the fixture.
func InitRepository(layout procharness.Layout) error {
	if err := os.WriteFile(filepath.Join(layout.Repo, "README.md"), []byte("# provider live compatibility\n"), 0o600); err != nil {
		return fmt.Errorf("write live repository fixture: %w", err)
	}
	commands := [][]string{
		{"init", "-q"},
		{"config", "user.name", "Coop Live Test"},
		{"config", "user.email", "coop-live@example.invalid"},
		{"add", "README.md"},
		{"commit", "-qm", "initial"},
	}
	for _, args := range commands {
		if _, err := runGit(layout, args...); err != nil {
			return err
		}
	}
	return nil
}

func SnapshotRepository(layout procharness.Layout) (RepositorySnapshot, error) {
	var snapshot RepositorySnapshot
	commands := []struct {
		field *string
		args  []string
	}{
		{&snapshot.Head, []string{"rev-parse", "HEAD"}},
		{&snapshot.Status, []string{"status", "--porcelain=v1", "--untracked-files=all"}},
		{&snapshot.Refs, []string{"for-each-ref", "--format=%(refname)%00%(objectname)"}},
		{&snapshot.Reflog, []string{"reflog", "show", "--all", "--format=%gD%x00%H%x00%gs"}},
	}
	for _, command := range commands {
		output, err := runGit(layout, command.args...)
		if err != nil {
			return RepositorySnapshot{}, err
		}
		*command.field = string(output)
	}
	tree, err := snapshotTree(layout.Repo)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	snapshot.Tree = tree
	return snapshot, nil
}

func (s RepositorySnapshot) Equal(other RepositorySnapshot) bool { return s == other }

func runGit(layout procharness.Layout, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"-C", layout.Repo}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Env = []string{
		"HOME=" + layout.Home, "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + layout.GitConfig, "GIT_CONFIG_NOSYSTEM=1",
		"LANG=C", "LC_ALL=C", "TZ=UTC",
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git live fixture command failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func snapshotTree(root string) ([32]byte, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == ".git" {
			return filepath.SkipDir
		}
		if relative == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		writeTreeField(h, filepath.ToSlash(relative))
		writeTreeInt(h, uint64(info.Mode()))
		switch {
		case info.Mode().IsRegular():
			writeTreeInt(h, uint64(info.Size()))
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(h, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			writeTreeField(h, target)
		}
		return nil
	})
	if err != nil {
		return [32]byte{}, fmt.Errorf("snapshot live repository: %w", err)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func writeTreeField(w io.Writer, value string) {
	writeTreeInt(w, uint64(len(value)))
	_, _ = io.WriteString(w, value)
}

func writeTreeInt(w io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = w.Write(encoded[:])
}
