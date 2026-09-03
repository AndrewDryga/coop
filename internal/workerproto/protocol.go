// Package workerproto defines the bounded v1 contract between an outbound Coop
// worker connector and the Responder control plane. It carries the exact
// bounded frozen model submission selected by Responder, but never provider
// credentials, repository content, or a generic command surface.
package workerproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"
)

const (
	Version          = 1
	MaxDocumentBytes = 1 << 20
	maxBatch         = 100
	maxPayloadBytes  = 768 << 10
)

var (
	referencePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	digestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commandKinds     = []string{"ensure_workspace", "create_session", "get_session", "submit_turn", "get_turn", "get_output_artifact", "get_changes", "get_changes_page", "run_review", "plan_discard", "discard_session", "get_review_patch", "validate_candidate", "cancel_turn", "fence_operation", "checkpoint_workspace", "close_session", "reconcile_operation"}
	workerStates     = []string{"eligible", "busy", "draining", "needs_auth"}
	capacityStates   = []string{"eligible", "busy", "cooldown", "needs_auth"}
	resultStates     = []string{"succeeded", "failed", "uncertain"}
	eventKinds       = []string{"operation", "session", "turn", "candidate", "validation", "workspace", "checkpoint", "capacity"}
)

type Envelope struct {
	Poll     Poll     `json:"poll"`
	Response Response `json:"response"`
}

type Poll struct {
	Version                int             `json:"version"`
	PollRef                string          `json:"poll_ref"`
	Worker                 WorkerHello     `json:"worker"`
	AcknowledgedCommandIDs []string        `json:"acknowledged_command_ids"`
	CommandResults         []CommandResult `json:"command_results"`
	EventBatches           []EventBatch    `json:"event_batches"`
}

type WorkerHello struct {
	ID                     string            `json:"id"`
	WorkspaceRef           string            `json:"workspace_ref"`
	ProtocolVersion        string            `json:"protocol_version"`
	BuildVersion           string            `json:"build_version"`
	ClockAt                time.Time         `json:"clock_at"`
	SandboxDigest          string            `json:"sandbox_digest"`
	PolicyDigests          map[string]string `json:"policy_digests"`
	PolicyAuthorityDigests map[string]string `json:"policy_authority_digests,omitempty"`
	Repositories           []Repository      `json:"repositories"`
	Capabilities           []Capability      `json:"capabilities"`
	Capacity               Capacity          `json:"capacity"`
	State                  string            `json:"state"`
}

type Repository struct {
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
}

type Capability struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Capacity struct {
	SessionSlotsFree    int        `json:"session_slots_free"`
	SessionSlotsTotal   int        `json:"session_slots_total"`
	TurnSlotsFree       int        `json:"turn_slots_free"`
	TurnSlotsTotal      int        `json:"turn_slots_total"`
	WorkspaceSlotsFree  int        `json:"workspace_slots_free"`
	WorkspaceSlotsTotal int        `json:"workspace_slots_total"`
	State               string     `json:"state"`
	CooldownUntil       *time.Time `json:"cooldown_until"`
}

type CommandResult struct {
	CommandID    string          `json:"command_id"`
	State        string          `json:"state"`
	OperationKey string          `json:"operation_key"`
	Resource     json.RawMessage `json:"resource"`
	Error        json.RawMessage `json:"error"`
}

type EventBatch struct {
	SessionRef          string  `json:"session_ref"`
	PlacementGeneration int     `json:"placement_generation"`
	AfterSequence       int64   `json:"after_sequence"`
	Events              []Event `json:"events"`
}

type Event struct {
	Sequence int64           `json:"sequence"`
	Kind     string          `json:"kind"`
	Payload  json.RawMessage `json:"payload"`
}

type Response struct {
	Version                      int                    `json:"version"`
	PollRef                      string                 `json:"poll_ref"`
	ServerTime                   time.Time              `json:"server_time"`
	AcknowledgedResultCommandIDs []string               `json:"acknowledged_result_command_ids"`
	Commands                     []Command              `json:"commands"`
	EventAcknowledgements        []EventAcknowledgement `json:"event_acknowledgements"`
}

type Command struct {
	CommandID           string          `json:"command_id"`
	WorkerID            string          `json:"worker_id"`
	SessionRef          string          `json:"session_ref"`
	PlacementGeneration int             `json:"placement_generation"`
	LeaseRef            string          `json:"lease_ref"`
	LeaseExpiresAt      time.Time       `json:"lease_expires_at"`
	Kind                string          `json:"kind"`
	CommandVersion      int             `json:"command_version"`
	Payload             json.RawMessage `json:"payload"`
	IdempotencyKey      string          `json:"idempotency_key"`
}

type EventAcknowledgement struct {
	SessionRef          string `json:"session_ref"`
	PlacementGeneration int    `json:"placement_generation"`
	Sequence            int64  `json:"sequence"`
}

// Validate rechecks one command before a connector persists or executes it.
func (c Command) Validate() error { return c.validate() }

// Validate rechecks one command result before a connector publishes it.
func (r CommandResult) Validate() error { return r.validate() }

func DecodePoll(document []byte) (Poll, error) {
	var poll Poll
	if err := decodeStrict(document, &poll); err != nil {
		return Poll{}, fmt.Errorf("invalid worker poll: %w", err)
	}
	if err := poll.Validate(); err != nil {
		return Poll{}, err
	}
	return poll, nil
}

func DecodeResponse(document []byte) (Response, error) {
	var response Response
	if err := decodeStrict(document, &response); err != nil {
		return Response{}, fmt.Errorf("invalid worker response: %w", err)
	}
	if err := response.Validate(); err != nil {
		return Response{}, err
	}
	return response, nil
}

func (p Poll) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("unsupported worker protocol version %d", p.Version)
	}
	if err := reference(p.PollRef, 256, "poll_ref"); err != nil {
		return err
	}
	if err := p.Worker.validate(); err != nil {
		return err
	}
	if len(p.AcknowledgedCommandIDs) > maxBatch || len(p.CommandResults) > maxBatch || len(p.EventBatches) > maxBatch {
		return errors.New("worker poll batch exceeds 100 items")
	}
	if err := uniqueReferences(p.AcknowledgedCommandIDs, "acknowledged command id"); err != nil {
		return err
	}
	for _, result := range p.CommandResults {
		if err := result.validate(); err != nil {
			return err
		}
	}
	for _, batch := range p.EventBatches {
		if err := batch.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r Response) Validate() error {
	if r.Version != Version {
		return fmt.Errorf("unsupported worker protocol version %d", r.Version)
	}
	if err := reference(r.PollRef, 256, "poll_ref"); err != nil {
		return err
	}
	if r.ServerTime.IsZero() {
		return errors.New("server_time is required")
	}
	if len(r.AcknowledgedResultCommandIDs) > maxBatch || len(r.Commands) > maxBatch || len(r.EventAcknowledgements) > maxBatch {
		return errors.New("worker response batch exceeds 100 items")
	}
	if err := uniqueReferences(r.AcknowledgedResultCommandIDs, "acknowledged result command id"); err != nil {
		return err
	}
	for _, command := range r.Commands {
		if err := command.validate(); err != nil {
			return err
		}
	}
	for _, ack := range r.EventAcknowledgements {
		if err := ack.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (w WorkerHello) validate() error {
	for field, value := range map[string]string{"worker id": w.ID, "workspace_ref": w.WorkspaceRef, "protocol_version": w.ProtocolVersion, "build_version": w.BuildVersion} {
		if err := reference(value, 256, field); err != nil {
			return err
		}
	}
	if w.ClockAt.IsZero() || !digestPattern.MatchString(w.SandboxDigest) {
		return errors.New("worker clock and sandbox digest are required")
	}
	if !slices.Contains(workerStates, w.State) {
		return errors.New("invalid worker state")
	}
	if len(w.PolicyDigests) > maxBatch || len(w.PolicyAuthorityDigests) > maxBatch || len(w.Repositories) > maxBatch || len(w.Capabilities) > maxBatch {
		return errors.New("worker advertisement exceeds 100 items")
	}
	for name, digest := range w.PolicyDigests {
		if reference(name, 256, "policy name") != nil || !digestPattern.MatchString(digest) {
			return errors.New("invalid policy advertisement")
		}
	}
	if len(w.PolicyAuthorityDigests) > 0 {
		if len(w.PolicyAuthorityDigests) != len(w.PolicyDigests) {
			return errors.New("policy authority advertisement does not match policy advertisement")
		}
		for name, digest := range w.PolicyAuthorityDigests {
			if _, ok := w.PolicyDigests[name]; !ok || reference(name, 256, "policy name") != nil || !digestPattern.MatchString(digest) {
				return errors.New("invalid policy authority advertisement")
			}
		}
	}
	for _, repository := range w.Repositories {
		if reference(repository.Ref, 256, "repository ref") != nil || reference(repository.Revision, 256, "repository revision") != nil {
			return errors.New("invalid repository advertisement")
		}
	}
	for _, capability := range w.Capabilities {
		if reference(capability.Name, 256, "capability name") != nil || reference(capability.Version, 128, "capability version") != nil {
			return errors.New("invalid capability advertisement")
		}
	}
	if !uniqueRepositoryRefs(w.Repositories) || !uniqueCapabilityNames(w.Capabilities) {
		return errors.New("duplicate worker authority advertisement")
	}
	return w.Capacity.validate()
}

func (c Capacity) validate() error {
	if !slots(c.SessionSlotsFree, c.SessionSlotsTotal) || !slots(c.TurnSlotsFree, c.TurnSlotsTotal) || !slots(c.WorkspaceSlotsFree, c.WorkspaceSlotsTotal) {
		return errors.New("invalid worker capacity slots")
	}
	if !slices.Contains(capacityStates, c.State) {
		return errors.New("invalid worker capacity state")
	}
	if (c.State == "cooldown") != (c.CooldownUntil != nil) {
		return errors.New("cooldown timestamp does not match capacity state")
	}
	return nil
}

func (r CommandResult) validate() error {
	if err := reference(r.CommandID, 256, "command id"); err != nil {
		return err
	}
	if err := reference(r.OperationKey, 512, "operation key"); err != nil {
		return err
	}
	if !slices.Contains(resultStates, r.State) {
		return errors.New("invalid command result state")
	}
	resource := nonNullObject(r.Resource)
	failure := nonNullObject(r.Error)
	if r.State == "succeeded" && resource && !failure {
		return nil
	}
	if r.State != "succeeded" && !resource && failure {
		return nil
	}
	return errors.New("command result resource/error shape does not match state")
}

func (b EventBatch) validate() error {
	if err := reference(b.SessionRef, 256, "session ref"); err != nil {
		return err
	}
	if b.PlacementGeneration <= 0 || b.AfterSequence < 0 || len(b.Events) > maxBatch {
		return errors.New("invalid event batch identity")
	}
	for index, event := range b.Events {
		if event.Sequence != b.AfterSequence+int64(index)+1 {
			return errors.New("event batch sequence is not contiguous")
		}
		if !slices.Contains(eventKinds, event.Kind) || !boundedObject(event.Payload) {
			return errors.New("invalid event")
		}
	}
	return nil
}

func (c Command) validate() error {
	for field, value := range map[string]string{"command id": c.CommandID, "worker id": c.WorkerID, "session ref": c.SessionRef, "lease ref": c.LeaseRef, "idempotency key": c.IdempotencyKey} {
		maximum := 256
		if field == "idempotency key" {
			maximum = 512
		}
		if err := reference(value, maximum, field); err != nil {
			return err
		}
	}
	if c.PlacementGeneration <= 0 || c.CommandVersion != Version || c.LeaseExpiresAt.IsZero() {
		return errors.New("invalid command generation, version, or lease")
	}
	if !slices.Contains(commandKinds, c.Kind) || !boundedObject(c.Payload) {
		return errors.New("invalid command kind or payload")
	}
	return nil
}

func (a EventAcknowledgement) validate() error {
	if err := reference(a.SessionRef, 256, "session ref"); err != nil {
		return err
	}
	if a.PlacementGeneration <= 0 || a.Sequence < 0 {
		return errors.New("invalid event acknowledgement")
	}
	return nil
}

func decodeStrict(document []byte, target any) error {
	if len(document) == 0 || len(document) > MaxDocumentBytes {
		return errors.New("document is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("document has trailing data")
	}
	return nil
}

func reference(value string, maximum int, field string) error {
	if len(value) == 0 || len(value) > maximum || !referencePattern.MatchString(value) {
		return fmt.Errorf("invalid %s", field)
	}
	return nil
}

func uniqueReferences(values []string, field string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := reference(value, 256, field); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("duplicate %s", field)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func uniqueRepositoryRefs(values []Repository) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value.Ref]; exists {
			return false
		}
		seen[value.Ref] = struct{}{}
	}
	return true
}

func uniqueCapabilityNames(values []Capability) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value.Name]; exists {
			return false
		}
		seen[value.Name] = struct{}{}
	}
	return true
}

func slots(free, total int) bool { return total >= 0 && total <= 10_000 && free >= 0 && free <= total }

func boundedObject(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > maxPayloadBytes {
		return false
	}
	var value map[string]any
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func nonNullObject(raw json.RawMessage) bool {
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && boundedObject(raw)
}
