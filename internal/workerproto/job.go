package workerproto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/session"
)

// JobSpec is controller-owned execution authority, not an instruction embedded in task text.
// Version 1 of this document belongs to worker protocol v2. No source path, remote URL or
// credential is accepted here; the trusted host fetches verified source with a separate grant.
type JobSpec struct {
	Version            int            `json:"version"`
	JobRef             string         `json:"job_ref"`
	Source             *JobSource     `json:"source"`
	Companions         []JobCompanion `json:"companions"`
	Targets            []string       `json:"targets"`
	Mode               string         `json:"mode"`
	ProjectEnv         bool           `json:"project_env"`
	ProjectMCP         bool           `json:"project_mcp"`
	RepositoryReadOnly bool           `json:"repository_read_only"`
	Egress             JobEgress      `json:"egress"`
	Limits             JobLimits      `json:"limits"`
}

type JobSource struct {
	RepositoryRef      string                `json:"repository_ref"`
	GitHubRepository   string                `json:"github_repository"`
	GitHubRepositoryID int64                 `json:"github_repository_id"`
	Binding            session.SourceBinding `json:"binding"`
	Submodules         []JobSubmodule        `json:"submodules"`
}

// RepositoryIdentity is the scope of a host-only GitHub credential. Exact
// commits and trees remain in the job; GitHub's token grants repository access.
type RepositoryIdentity struct {
	RepositoryRef      string `json:"repository_ref"`
	GitHubRepository   string `json:"github_repository"`
	GitHubRepositoryID int64  `json:"github_repository_id"`
}

func (r RepositoryIdentity) Validate() error {
	if reference(r.RepositoryRef, 256, "repository ref") != nil ||
		!validGitHubRepository(r.GitHubRepository) || r.GitHubRepositoryID <= 0 {
		return errors.New("invalid repository identity")
	}
	return nil
}

func (s JobSource) RepositoryIdentity() RepositoryIdentity {
	return RepositoryIdentity{s.RepositoryRef, s.GitHubRepository, s.GitHubRepositoryID}
}

func (s JobSubmodule) RepositoryIdentity() RepositoryIdentity {
	return RepositoryIdentity{s.RepositoryRef, s.GitHubRepository, s.GitHubRepositoryID}
}

type JobSubmodule struct {
	Path               string         `json:"path"`
	RepositoryRef      string         `json:"repository_ref"`
	GitHubRepository   string         `json:"github_repository"`
	GitHubRepositoryID int64          `json:"github_repository_id"`
	Commit             string         `json:"commit"`
	Tree               string         `json:"tree"`
	Submodules         []JobSubmodule `json:"submodules"`
}

type JobCompanion struct {
	Name   string    `json:"name"`
	Source JobSource `json:"source"`
}

type JobEgress struct {
	Mode               string    `json:"mode"`
	Rules              []JobRule `json:"rules"`
	ExportDestinations bool      `json:"export_destinations"`
}

type JobRule struct {
	To       JobDestination `json:"to"`
	Protocol string         `json:"protocol,omitempty"`
	Ports    []int          `json:"ports,omitempty"`
	Types    []string       `json:"types,omitempty"`
	Codes    []int          `json:"codes,omitempty"`
}

type JobDestination struct {
	Domain   string   `json:"domain,omitempty"`
	IP       string   `json:"ip,omitempty"`
	CIDR     string   `json:"cidr,omitempty"`
	Service  string   `json:"service,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Features []string `json:"features,omitempty"`
}

type JobLimits struct {
	MaxTurns          int   `json:"max_turns"`
	MaxQueuedTurns    int   `json:"max_queued_turns"`
	MaxQueuedBytes    int   `json:"max_queued_bytes"`
	TurnTimeoutMS     int64 `json:"turn_timeout_ms"`
	WarmIdleTimeoutMS int64 `json:"warm_idle_timeout_ms"`
	MaxPatchBytes     int   `json:"max_patch_bytes"`
}

const (
	maxJobDocumentBytes = 256 << 10
)

var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// DecodeJobSpec refuses unknown fields at every level before any job can be journaled.
func DecodeJobSpec(document []byte) (JobSpec, error) {
	if len(document) == 0 || len(document) > maxJobDocumentBytes {
		return JobSpec{}, errors.New("job specification is empty or oversized")
	}
	if err := rejectDuplicateJobKeys(document); err != nil {
		return JobSpec{}, err
	}
	if err := requireJobFields(document); err != nil {
		return JobSpec{}, err
	}
	var spec JobSpec
	if err := decodeStrict(document, &spec); err != nil {
		return JobSpec{}, fmt.Errorf("decode job specification: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return JobSpec{}, err
	}
	canonical, err := spec.CanonicalDocument()
	if err != nil {
		return JobSpec{}, err
	}
	var supplied, normalized map[string]any
	if err := json.Unmarshal(document, &supplied); err != nil {
		return JobSpec{}, err
	}
	if err := json.Unmarshal(canonical, &normalized); err != nil || !reflect.DeepEqual(supplied, normalized) {
		return JobSpec{}, errors.New("job specification has fields that change when normalized")
	}
	return spec, nil
}

// A missing boolean or zero-valued limit must not inherit a worker default. Refuse absent
// authority before decoding into Go values, where omission and an explicit zero look identical.
func requireJobFields(document []byte) error {
	object := func(raw []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil || value == nil {
			return nil, errors.New("job specification contains a non-object")
		}
		for _, field := range required {
			member, ok := value[field]
			if !ok || bytes.Equal(bytes.TrimSpace(member), []byte("null")) {
				return nil, fmt.Errorf("job specification is missing %s", field)
			}
		}
		for field := range value {
			if !slices.Contains(required, field) && !slices.Contains(optional, field) {
				return nil, fmt.Errorf("job specification contains unknown field %s", field)
			}
		}
		return value, nil
	}
	root, err := object(document, []string{"version", "job_ref", "companions", "targets", "mode",
		"project_env", "project_mcp", "repository_read_only", "egress", "limits"}, "source")
	if err != nil {
		return err
	}
	if _, ok := root["source"]; !ok {
		return errors.New("job specification is missing source")
	}
	var checkSubmodules func([]byte, int) error
	checkSubmodules = func(raw []byte, depth int) error {
		var modules []json.RawMessage
		if depth > 16 || json.Unmarshal(raw, &modules) != nil || modules == nil {
			return errors.New("invalid job submodule array or nesting")
		}
		for _, raw := range modules {
			module, err := object(raw, []string{"path", "repository_ref", "github_repository", "github_repository_id", "commit", "tree", "submodules"})
			if err != nil {
				return err
			}
			if err := checkSubmodules(module["submodules"], depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	checkSource := func(raw []byte) error {
		source, err := object(raw, []string{"repository_ref", "github_repository", "github_repository_id", "binding", "submodules"})
		if err != nil {
			return err
		}
		binding, err := object(source["binding"], []string{"version", "kind", "requested", "remote_identity", "default_ref", "default_commit", "selected_commit", "base_commit", "admitted_tree", "resolved_at"}, "selected_ref", "pull_request_number", "pull_request_expected_head")
		if err != nil {
			return err
		}
		// Exact commits have no advertised ref; the field is required but nullable.
		if _, ok := binding["selected_ref"]; !ok {
			return errors.New("job specification is missing selected_ref")
		}
		if _, err := object(binding["requested"], []string{"kind"}, "name", "number", "sha", "expected_head_commit"); err != nil {
			return err
		}
		return checkSubmodules(source["submodules"], 0)
	}
	if !bytes.Equal(bytes.TrimSpace(root["source"]), []byte("null")) {
		if err := checkSource(root["source"]); err != nil {
			return err
		}
	}
	var companions []json.RawMessage
	if err := json.Unmarshal(root["companions"], &companions); err != nil {
		return fmt.Errorf("decode job companions: %w", err)
	}
	for _, raw := range companions {
		companion, err := object(raw, []string{"name", "source"})
		if err != nil {
			return err
		}
		if err := checkSource(companion["source"]); err != nil {
			return err
		}
	}
	egress, err := object(root["egress"], []string{"mode", "rules", "export_destinations"})
	if err != nil {
		return err
	}
	var rules []json.RawMessage
	if err := json.Unmarshal(egress["rules"], &rules); err != nil {
		return fmt.Errorf("decode job egress rules: %w", err)
	}
	for _, raw := range rules {
		rule, err := object(raw, []string{"to"}, "protocol", "ports", "types", "codes")
		if err != nil {
			return err
		}
		if _, err := object(rule["to"], nil, "domain", "ip", "cidr", "service", "provider", "features"); err != nil {
			return err
		}
	}
	_, err = object(root["limits"], []string{"max_turns", "max_queued_turns", "max_queued_bytes",
		"turn_timeout_ms", "warm_idle_timeout_ms", "max_patch_bytes"})
	return err
}

func rejectDuplicateJobKeys(document []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("job specification has a duplicate or invalid key")
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return errors.New("job specification has invalid JSON nesting")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return fmt.Errorf("decode job specification: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("job specification has trailing data")
	}
	return nil
}

func (s JobSpec) Validate() error {
	if s.Version != 1 || reference(s.JobRef, 256, "job ref") != nil {
		return errors.New("invalid job version or reference")
	}
	if !slices.Contains([]string{"normal", "readonly", "bare"}, s.Mode) {
		return errors.New("invalid job mode")
	}
	if s.ProjectEnv || s.ProjectMCP {
		return errors.New("job cannot project worker-local project environment or MCP settings")
	}
	if s.Companions == nil || s.Egress.Rules == nil || len(s.Targets) == 0 || len(s.Targets) > 4 {
		return errors.New("job target ladder must contain 1..4 targets")
	}
	for _, target := range s.Targets {
		if len(target) == 0 || len(target) > 256 {
			return errors.New("invalid job target")
		}
	}
	if s.Mode == "bare" {
		if s.Source != nil || len(s.Companions) != 0 || s.ProjectEnv || s.ProjectMCP || s.RepositoryReadOnly {
			return errors.New("bare job cannot carry repository or project authority")
		}
	}
	if s.Mode == "readonly" && !s.RepositoryReadOnly {
		return errors.New("readonly job cannot write its repository")
	}
	if s.Mode != "normal" && (s.Egress.Mode == "filtered" || s.Limits.WarmIdleTimeoutMS != 0) {
		return errors.New("restricted job cannot request filtered networking or warm execution")
	}
	if s.Source != nil {
		if err := s.Source.validate(); err != nil {
			return err
		}
	}
	if len(s.Companions) > 32 {
		return errors.New("job has too many companion repositories")
	}
	seen := make(map[string]bool, len(s.Companions))
	for _, companion := range s.Companions {
		if reference(companion.Name, 256, "companion name") != nil || seen[companion.Name] {
			return errors.New("invalid or duplicate job companion")
		}
		seen[companion.Name] = true
		if err := companion.Source.validate(); err != nil {
			return err
		}
	}
	if !s.Egress.valid() || !s.Limits.valid() {
		return errors.New("invalid job egress or limits")
	}
	return nil
}

func (s JobSource) validate() error {
	if s.RepositoryIdentity().Validate() != nil ||
		validateJobSubmodules(s.Submodules, 0) != nil ||
		s.Binding.RemoteIdentity != "origin" || !gitCommitPattern.MatchString(s.Binding.AdmittedTree) ||
		session.ValidateSourceBinding(s.Binding) != nil {
		return errors.New("invalid job source identity")
	}
	return nil
}

func validateJobSubmodules(modules []JobSubmodule, depth int) error {
	if modules == nil || len(modules) > 1024 || depth > 16 {
		return errors.New("invalid submodule manifest size or nesting")
	}
	seen := make(map[string]bool, len(modules))
	for _, module := range modules {
		if !validJobSubmodulePath(module.Path) || seen[module.Path] ||
			module.RepositoryIdentity().Validate() != nil ||
			!gitCommitPattern.MatchString(module.Commit) || !gitCommitPattern.MatchString(module.Tree) {
			return errors.New("invalid submodule identity or path")
		}
		seen[module.Path] = true
		if err := validateJobSubmodules(module.Submodules, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func validJobSubmodulePath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\\") || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func validGitHubRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 100 {
			return false
		}
		for _, char := range part {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.') {
				return false
			}
		}
	}
	return true
}

// StagingKey names one private source slot by the complete descriptor.
func (s JobSource) StagingKey() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	document, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(document)
	return hex.EncodeToString(sum[:]), nil
}

func (e JobEgress) valid() bool {
	if !slices.Contains([]string{"open", "none", "filtered"}, e.Mode) || len(e.Rules) > 128 {
		return false
	}
	if e.Mode != "filtered" && len(e.Rules) != 0 {
		return false
	}
	if e.Mode != "filtered" && e.ExportDestinations {
		return false
	}
	for _, rule := range e.Rules {
		if rule.To.Service != "" {
			return false // no controller-fenced Compose service definition in this protocol
		}
		selectors := 0
		for _, value := range []string{rule.To.Domain, rule.To.IP, rule.To.CIDR, rule.To.Service, rule.To.Provider} {
			if value != "" {
				selectors++
			}
		}
		if selectors != 1 || len(rule.To.Features) > 128 || len(rule.Ports) > 128 || len(rule.Types) > 128 || len(rule.Codes) > 128 {
			return false
		}
	}
	return true
}

func (l JobLimits) valid() bool {
	return l.MaxTurns > 0 && l.MaxTurns <= 10_000 &&
		l.MaxQueuedTurns > 0 && l.MaxQueuedTurns <= 1_000 &&
		l.MaxQueuedBytes > 0 && l.MaxQueuedBytes <= 64<<20 &&
		l.TurnTimeoutMS > 0 && l.TurnTimeoutMS <= 86_400_000 &&
		l.WarmIdleTimeoutMS >= 0 && l.WarmIdleTimeoutMS <= 3_600_000 &&
		l.MaxPatchBytes > 0 && l.MaxPatchBytes <= 1<<20
}

// CanonicalDocument is the exact authority the session journal and database retain. It uses
// sorted-key Go JSON; Ryker must match Go's HTML and separator escaping when digesting it.
func (s JobSpec) CanonicalDocument() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	document, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(document, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// Digest identifies one complete immutable job, independent of its incoming JSON key order.
func (s JobSpec) Digest() (string, error) {
	canonical, err := s.CanonicalDocument()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
