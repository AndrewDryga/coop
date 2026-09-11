package sessionsvc

import (
	"context"
	"encoding/base64"
	"errors"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/secretscan"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// sessionNetworkReads is what one evidence capture reads from the owner-private network registry
// for a filtered session: the captured policy, the session's run inventory, the newest run's
// inspection and the session-wide aggregate. Each read fails on its own, so a registry that can
// answer the policy but not the runs still exports the policy.
type sessionNetworkReads struct {
	policy      egress.Snapshot
	policyErr   error
	records     []networkstate.Execution
	complete    bool
	recordsErr  error
	inspection  networkstate.Inspection
	inspectErr  error
	receipt     networkview.SessionReceipt
	receiptErr  error
	receiptRead bool
}

// SessionEvidence is the read behind GET /v1/sessions/{id}/evidence: one bounded, versioned
// account of the session for a control plane's inspection page. It reads the immutable session
// row, the retained network evidence and the bound task folder, and publishes nothing — no
// gateway probe, no runtime, no authority, no destination unless the policy opted in.
//
// A section that cannot be read says so with a reason instead of failing the whole read: the
// posture and the task binding on the session row are always known, and an operator looking at
// "receipt unavailable: registry unreadable" learns more than one from a 503.
func (s *Service) SessionEvidence(ctx context.Context, id string) (workerproto.SessionEvidence, error) {
	bound, err := s.store.GetSession(ctx, id)
	if err != nil {
		return workerproto.SessionEvidence{}, err
	}
	now := time.Now().UTC()
	out := workerproto.SessionEvidence{
		Version: workerproto.SessionEvidenceVersion, CapturedAt: now, SessionID: bound.ID,
		Revision: bound.Revision, State: string(bound.State),
		Network: s.sessionNetworkEvidence(bound, now),
		Task:    sessionTaskEvidence(bound),
	}
	if err := out.Validate(); err != nil {
		return workerproto.SessionEvidence{}, &session.Error{Code: session.CodeInternal,
			Detail: "session evidence does not satisfy its own contract: " + err.Error()}
	}
	return out, nil
}

func (s *Service) sessionNetworkEvidence(bound session.Session, now time.Time) workerproto.NetworkEvidence {
	mode := normalizedSessionNetworkMode(bound.NetworkMode)
	if mode != string(egress.Filtered) {
		return networkEvidenceNotFiltered(mode)
	}
	reads := s.readSessionNetwork(bound, now)
	return networkEvidenceFromReads(bound, reads)
}

// readSessionNetwork performs the registry reads for one filtered session. It is the only part
// of the evidence read that touches the host; the projection below is pure so tests can drive it
// with retained records instead of a live gateway.
func (s *Service) readSessionNetwork(bound session.Session, now time.Time) sessionNetworkReads {
	if s.testSessionNetworkReads != nil {
		return s.testSessionNetworkReads(bound, now)
	}
	var reads sessionNetworkReads
	reads.policy, reads.policyErr = loadSessionSnapshot(bound)
	if reads.policyErr != nil {
		return reads
	}
	evidence, err := sessionEvidence(bound)
	if err != nil {
		reads.recordsErr = err
		return reads
	}
	defer evidence.Close()
	reads.records, reads.complete, reads.recordsErr = sessionExecutions(evidence, bound.ID)
	if reads.recordsErr != nil {
		return reads
	}
	if len(reads.records) != 0 {
		newest := reads.records[len(reads.records)-1]
		reads.inspection, reads.inspectErr = evidence.Inspect(newest.ID, now, reads.policy.ExportDestinations)
	}
	identity := networkview.SessionNetworkIdentity{
		ID: bound.ID, PolicyFingerprint: bound.NetworkFingerprint, AuthorityDigest: bound.AuthorityDigest,
		Mode: egress.Filtered, StartedAt: bound.CreatedAt, RunsComplete: reads.complete,
	}
	if bound.State == session.SessionClosed || bound.State == session.SessionDiscarded {
		closed := bound.UpdatedAt
		identity.ClosedAt = &closed
	}
	reads.receiptRead = true
	reads.receipt, reads.receiptErr = networkview.AggregateSessionNetwork(identity,
		sessionRunObservations(evidence, bound, reads.records, reads.policy.ExportDestinations), reads.policy.ExportDestinations)
	return reads
}

func networkEvidenceNotFiltered(mode string) workerproto.NetworkEvidence {
	return workerproto.NetworkEvidence{
		Mode: mode,
		Access: workerproto.NetworkAccess{Status: workerproto.EvidenceStatusNotFiltered,
			Requested: []string{}, Effective: []string{}},
		Observation: emptyNetworkObservation(workerproto.EvidenceStatusNotFiltered, nil),
		Receipt:     workerproto.NetworkReceipt{Status: workerproto.EvidenceStatusNotFiltered, Runs: []workerproto.NetworkRunReference{}},
	}
}

// networkEvidenceFromReads projects the registry reads into the wire object. It is an allowlist
// over the daemon's own projected records: the snapshot handed in has already had destinations
// withheld or included by the policy, and nothing here re-reads the owner-private record.
func networkEvidenceFromReads(bound session.Session, reads sessionNetworkReads) workerproto.NetworkEvidence {
	fingerprint := bound.NetworkFingerprint
	out := workerproto.NetworkEvidence{Mode: string(egress.Filtered), Fingerprint: &fingerprint}
	if reads.policyErr != nil {
		reason := evidenceReason("captured network policy unreadable", reads.policyErr)
		out.Access = workerproto.NetworkAccess{Status: workerproto.EvidenceStatusUnavailable, Reason: &reason,
			Requested: []string{}, Effective: []string{}}
		out.Observation = emptyNetworkObservation(workerproto.EvidenceStatusUnavailable, &reason)
		out.Receipt = workerproto.NetworkReceipt{Status: workerproto.EvidenceStatusUnavailable, Reason: &reason,
			Runs: []workerproto.NetworkRunReference{}}
		return out
	}
	projection := workerproto.NetworkProjectionWithheld
	if reads.policy.ExportDestinations {
		projection = workerproto.NetworkProjectionIncluded
	}
	out.Access = workerproto.NetworkAccess{Status: workerproto.EvidenceStatusCaptured, Projection: &projection,
		Requested: []string{}, Effective: []string{}}
	if bound.NetworkQualification != "" {
		qualification := bound.NetworkQualification
		out.Access.Qualification = &qualification
	}
	if reads.policy.ExportDestinations {
		out.Access.Requested, out.Access.Effective = sessionPolicyRuleTexts(reads.policy)
		out.Access.Requested = boundedRuleTexts(out.Access.Requested)
		out.Access.Effective = boundedRuleTexts(out.Access.Effective)
	}
	switch {
	case reads.recordsErr != nil:
		reason := evidenceReason("network run inventory unreadable", reads.recordsErr)
		out.Observation = emptyNetworkObservation(workerproto.EvidenceStatusUnavailable, &reason)
	case len(reads.records) == 0:
		out.Observation = emptyNetworkObservation(workerproto.EvidenceStatusNoRun, nil)
	case reads.inspectErr != nil:
		reason := evidenceReason("newest network run unreadable", reads.inspectErr)
		out.Observation = emptyNetworkObservation(workerproto.EvidenceStatusUnavailable, &reason)
	default:
		out.Observation = networkObservationFromInspection(reads.inspection, reads.policy.ExportDestinations)
	}
	switch {
	case !reads.receiptRead:
		reason := evidenceReason("network receipt not aggregated", reads.recordsErr)
		out.Receipt = workerproto.NetworkReceipt{Status: workerproto.EvidenceStatusUnavailable, Reason: &reason,
			Runs: []workerproto.NetworkRunReference{}}
	case reads.receiptErr != nil:
		reason := evidenceReason("network receipt unavailable", reads.receiptErr)
		out.Receipt = workerproto.NetworkReceipt{Status: workerproto.EvidenceStatusUnavailable, Reason: &reason,
			Runs: []workerproto.NetworkRunReference{}}
	default:
		out.Receipt = networkReceiptFromAggregate(reads.receipt)
	}
	return out
}

func emptyNetworkObservation(status string, reason *string) workerproto.NetworkObservation {
	return workerproto.NetworkObservation{Status: status, Reason: reason, Sources: []workerproto.NetworkSource{},
		Denials: []workerproto.NetworkDenial{}, Connections: []workerproto.NetworkConnection{}, Alerts: []workerproto.NetworkAlert{}}
}

func networkObservationFromInspection(inspection networkstate.Inspection, exportDestinations bool) workerproto.NetworkObservation {
	observed := inspection.Observed
	receipt := inspection.AggregateObservation.Receipt
	projection := workerproto.NetworkProjectionWithheld
	if exportDestinations {
		projection = workerproto.NetworkProjectionIncluded
	}
	asOf := observed.AsOf.UTC()
	out := workerproto.NetworkObservation{
		Status: workerproto.EvidenceStatusObserved, Freshness: text(inspection.Freshness), RunID: text(observed.RunID),
		AttemptID: optionalText(receipt.AttemptID), GatewayEpoch: optionalText(observed.Epoch),
		Sequence: text(countText(observed.Sequence)), AsOf: &asOf, Availability: optionalText(observed.Availability),
		Scope: optionalText(observed.Scope), Sealed: inspection.Receipt != nil, CleanupOutcome: optionalText(inspection.Cleanup),
		Projection: &projection, Health: networkHealth(observed.Health), Coverage: networkCoverage(observed.Coverage),
		Counters: networkCounters(observed.Counters), Loss: networkLoss(observed.Loss),
		Sources: []workerproto.NetworkSource{}, Denials: []workerproto.NetworkDenial{},
		Connections: []workerproto.NetworkConnection{}, Alerts: []workerproto.NetworkAlert{},
	}
	for index, source := range observed.Sources {
		if index >= workerproto.MaxSessionEvidenceSources {
			break
		}
		out.Sources = append(out.Sources, workerproto.NetworkSource{
			ID: source.ID, Status: source.Status, Sequence: countText(source.Sequence), ObservedAt: utcTime(source.ObservedAt),
			LastEventAt: utcTime(source.LastEventAt), LostRecords: countText(source.Lost), UnknownLoss: source.Unknown,
			Reason: optionalText(source.Reason),
		})
	}
	for index, denial := range observed.Denials {
		if index >= workerproto.MaxSessionEvidenceDenials {
			out.OmittedDenials = len(observed.Denials) - workerproto.MaxSessionEvidenceDenials
			break
		}
		row := workerproto.NetworkDenial{
			ID: denial.ID, At: denial.At.UTC(), Kind: denial.Kind, Basis: box.NetworkDenialBasis(denial.Kind),
			Reason: denial.Reason, Source: denial.Source, SourceSequence: countText(denial.Sequence),
			DestinationWithheld: !exportDestinations, Port: denial.Port,
		}
		if exportDestinations {
			row.Destination = optionalText(firstText(denial.Name, denial.Peer))
		}
		out.Denials = append(out.Denials, row)
	}
	for index, connection := range observed.Connections {
		if index >= workerproto.MaxSessionEvidenceConnections {
			out.OmittedConnections = len(observed.Connections) - workerproto.MaxSessionEvidenceConnections
			break
		}
		row := workerproto.NetworkConnection{
			ID: connection.ID, State: connection.State, Reason: optionalText(connection.Reason), Transport: connection.Transport,
			DestinationWithheld: !exportDestinations, StartedAt: utcTime(connection.StartedAt), ObservedAt: connection.ObservedAt.UTC(),
			SentBytes: optionalCount(connection.SentBytes), ReceivedBytes: optionalCount(connection.ReceivedBytes), Partial: connection.Partial,
		}
		if exportDestinations {
			row.Destination = optionalText(firstText(connection.Name, connection.Peer))
			row.RuleID = optionalText(connection.RuleID)
		}
		out.Connections = append(out.Connections, row)
	}
	for index, alert := range observed.Alerts {
		if index >= workerproto.MaxSessionEvidenceAlerts {
			out.OmittedAlerts = len(observed.Alerts) - workerproto.MaxSessionEvidenceAlerts
			break
		}
		out.Alerts = append(out.Alerts, workerproto.NetworkAlert{
			ID: alert.ID, Category: alert.Category, Severity: alert.Severity, State: alert.State, Terminal: alert.Terminal,
			FirstSeen: alert.FirstSeen.UTC(), LastSeen: alert.LastSeen.UTC(), Reason: optionalText(alert.Facts.Reason),
			HealthStatus: optionalText(alert.Facts.HealthStatus),
		})
	}
	return out
}

func networkReceiptFromAggregate(receipt networkview.SessionReceipt) workerproto.NetworkReceipt {
	out := workerproto.NetworkReceipt{
		Status: workerproto.EvidenceStatusAvailable, PolicyFingerprint: text(receipt.PolicyFingerprint),
		AuthorityDigest: text(receipt.AuthorityDigest), StartedAt: utcTime(&receipt.StartedAt), ClosedAt: utcTime(receipt.ClosedAt),
		Finality: text(receipt.Finality), Completeness: text(receipt.Completeness), Scope: text(receipt.Scope),
		Counters: networkCounters(receipt.Counters), Coverage: networkCoverage(receipt.Coverage), Loss: networkLoss(receipt.Loss),
		Runs: []workerproto.NetworkRunReference{}, RunCount: text(countText(receipt.RunCount)),
		Projection: text(receipt.Projection), ReceiptDigest: text(receipt.Digest), DigestScope: text(receipt.DigestScope),
	}
	omitted := uint64(receipt.OmittedReferences)
	for index, run := range receipt.Runs {
		if index >= workerproto.MaxSessionEvidenceRunRefs {
			omitted += uint64(len(receipt.Runs) - workerproto.MaxSessionEvidenceRunRefs)
			break
		}
		out.Runs = append(out.Runs, workerproto.NetworkRunReference{
			RunID: run.RunID, GatewayEpoch: run.Epoch, Sequence: countText(run.Sequence), AsOf: run.AsOf.UTC(),
			Finality: run.Finality, Completeness: run.Completeness, ReceiptDigest: run.ReceiptDigest,
		})
	}
	out.OmittedRunReferences = text(strconv.FormatUint(omitted, 10))
	return out
}

func networkHealth(health networkview.HealthLayers) *workerproto.NetworkHealth {
	layer := func(value networkview.Health) workerproto.NetworkLayerHealth {
		status := value.Status
		if status == "" {
			status = "unknown"
		}
		return workerproto.NetworkLayerHealth{Status: status, Reason: optionalText(value.Reason)}
	}
	return &workerproto.NetworkHealth{Enforcer: layer(health.Enforcer), Gateway: layer(health.Gateway),
		Resolver: layer(health.Resolver), Collector: layer(health.Collector)}
}

// networkCoverage keeps the collector's own word for each metric. A status the contract does not
// know reads as unavailable with the original word as its reason, never as exact.
func networkCoverage(coverage networkview.Coverage) *workerproto.NetworkCoverage {
	metric := func(value networkview.MetricCoverage) workerproto.NetworkMetricCoverage {
		switch value.Status {
		case "exact", "lower-bound", "unavailable":
			return workerproto.NetworkMetricCoverage{Status: value.Status, Reason: optionalText(value.Reason)}
		default:
			reason := firstText(value.Reason, value.Status)
			return workerproto.NetworkMetricCoverage{Status: "unavailable", Reason: optionalText(reason)}
		}
	}
	return &workerproto.NetworkCoverage{
		ProxyBytes: metric(coverage.ProxyBytes), Connections: metric(coverage.Connections),
		UpstreamFailures: metric(coverage.UpstreamFailures), KernelPackets: metric(coverage.KernelPackets),
		GuardDenials: metric(coverage.GuardDenials), MaintenanceQueries: metric(coverage.MaintenanceQueries),
		MaintenanceBytes: metric(coverage.MaintenanceBytes), SocketInventory: metric(coverage.SocketInventory),
		BoundaryAttribution: metric(coverage.BoundaryAttribution),
	}
}

func networkCounters(counters *networkview.Counters) *workerproto.NetworkCounters {
	if counters == nil {
		return nil
	}
	return &workerproto.NetworkCounters{
		SentBytes: optionalCount(counters.SentBytes), ReceivedBytes: optionalCount(counters.ReceivedBytes),
		Connections: optionalCount(counters.Connections), UpstreamFailures: optionalCount(counters.UpstreamFailures),
		DeniedPackets: optionalCount(counters.DeniedPackets), ProtectedPackets: optionalCount(counters.ProtectedPackets),
		DeniedDNSQueries: optionalCount(counters.DeniedDNSQueries), DeniedTLSConnections: optionalCount(counters.DeniedTLS),
		MaintenanceQueries: optionalCount(counters.MaintenanceQueries), MaintenanceFailures: optionalCount(counters.MaintenanceFailures),
		IngressDeniedPackets: optionalCount(counters.IngressDenials), MaintenanceSentBytes: optionalCount(counters.MaintenanceSentBytes),
		MaintenanceReceivedBytes: optionalCount(counters.MaintenanceReceivedBytes),
	}
}

func networkLoss(loss networkview.Loss) *workerproto.NetworkLoss {
	reasons := make([]string, 0, len(loss.Reasons))
	for index, reason := range loss.Reasons {
		if index >= 32 {
			break
		}
		reasons = append(reasons, reason)
	}
	return &workerproto.NetworkLoss{Records: countText(loss.Records), Unknown: loss.Unknown, Reasons: reasons,
		DetailTruncated: loss.DetailTruncated, OmittedDetails: optionalCount(loss.OmittedDetails),
		SuppressedAlerts: countText(loss.SuppressedAlerts)}
}

// sessionTaskEvidence reads the bound task folder through the same projection a checkpoint uses,
// so the state digest here equals the one a checkpoint of the same folder state records.
func sessionTaskEvidence(bound session.Session) workerproto.TaskEvidence {
	if bound.WorkspaceTask == nil {
		return workerproto.TaskEvidence{Status: workerproto.EvidenceStatusUnbound}
	}
	binding := *bound.WorkspaceTask
	out := workerproto.TaskEvidence{
		Status: workerproto.EvidenceStatusBound, QueueID: text(binding.QueueID), TaskID: text(binding.TaskID),
		ID: text(binding.ID), OfferRef: text(binding.OfferRef), DraftSHA256: text(binding.DraftSHA256),
	}
	snapshot, err := sessionTaskSnapshot(bound)
	if err != nil {
		reason := evidenceReason("bound task unreadable", err)
		out.Status, out.Reason = workerproto.EvidenceStatusUnavailable, &reason
		return out
	}
	out.Snapshot = &snapshot
	return out
}

func sessionTaskSnapshot(bound session.Session) (workerproto.TaskSnapshot, error) {
	if err := requireSessionWorkspace(bound); err != nil {
		return workerproto.TaskSnapshot{}, err
	}
	projection, blobs, _, err := checkpointTaskProjection(bound)
	if err != nil {
		return workerproto.TaskSnapshot{}, err
	}
	queue := filepath.Join(bound.Workspace, tasks.TasksRoot)
	item, ok, err := tasks.CurrentTask(queue, bound.WorkspaceTask.ID)
	if err != nil {
		return workerproto.TaskSnapshot{}, err
	}
	if !ok {
		return workerproto.TaskSnapshot{}, errors.New("bound workspace task is missing")
	}
	snapshot := workerproto.TaskSnapshot{
		State: projection.State, StateSHA256: projection.StateSHA256, Title: boundedTitle(item.Title, bound.WorkspaceTask.ID),
		Checklist: []workerproto.TaskChecklistItem{}, Files: []workerproto.TaskFile{},
		StateNote: workerproto.TaskNote{Status: workerproto.EvidenceStatusAbsent}, HasDecision: item.HasDecision,
	}
	taskDocument, stateNote := "", ""
	taskDocumentFound, stateNoteFound := false, false
	for _, blob := range blobs {
		raw, err := base64.StdEncoding.DecodeString(blob.entry.PathB64)
		if err != nil || !utf8.Valid(raw) {
			return workerproto.TaskSnapshot{}, errors.New("task file path is not valid text")
		}
		name := string(raw)
		snapshot.Files = append(snapshot.Files, workerproto.TaskFile{Path: name, ByteSize: blob.entry.ByteSize, SHA256: blob.entry.SHA256})
		// Task files are queue-relative: <state>/<task id>/<file>. Only the task folder's own
		// top-level documents are read; a nested artifacts/ copy of either name is not the task.
		if strings.Count(name, "/") != 2 {
			continue
		}
		switch path.Base(name) {
		case "task.md":
			taskDocument, taskDocumentFound = string(blob.path), true
		case "state.md":
			stateNote, stateNoteFound = string(blob.path), true
		}
	}
	if !taskDocumentFound {
		return workerproto.TaskSnapshot{}, errors.New("bound workspace task has no task document")
	}
	_, body := tasks.SplitFrontmatter(taskDocument)
	for index, entry := range tasks.ScanChecklist(body) {
		if index >= workerproto.MaxSessionEvidenceChecklist {
			break
		}
		label := strings.TrimSpace(entry.Label)
		if label == "" {
			label = "(unlabeled)"
		}
		if len(label) > workerproto.MaxSessionEvidenceLabelBytes {
			label = truncateUTF8(label, workerproto.MaxSessionEvidenceLabelBytes)
		}
		snapshot.Checklist = append(snapshot.Checklist, workerproto.TaskChecklistItem{Label: label, Checked: entry.Checked})
	}
	if stateNoteFound {
		snapshot.StateNote = taskStateNote(stateNote)
	}
	return snapshot, nil
}

// taskStateNote bounds the agent-written resume note and withholds it whole when it looks like it
// carries a secret. The scan runs on the complete note, not the truncated head, so a token past
// the bound still withholds the head.
func taskStateNote(note string) workerproto.TaskNote {
	if !utf8.ValidString(note) || strings.ContainsRune(note, 0) {
		reason := "state note is not valid text"
		return workerproto.TaskNote{Status: workerproto.EvidenceStatusWithheld, Reason: &reason}
	}
	if strings.TrimSpace(note) == "" {
		return workerproto.TaskNote{Status: workerproto.EvidenceStatusAbsent}
	}
	if findings := secretscan.ScanSecrets(note); len(findings) > 0 {
		reason := "state note contains a likely " + findings[0].Kind + " on line " + strconv.Itoa(findings[0].Line)
		return workerproto.TaskNote{Status: workerproto.EvidenceStatusWithheld, Reason: &reason}
	}
	truncated := false
	if len(note) > workerproto.MaxSessionEvidenceNoteBytes {
		note = truncateUTF8(note, workerproto.MaxSessionEvidenceNoteBytes)
		truncated = true
	}
	return workerproto.TaskNote{Status: workerproto.EvidenceStatusCaptured, Text: &note, Truncated: truncated}
}

func boundedTitle(title, fallback string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = fallback
	}
	if len(title) > workerproto.MaxSessionEvidenceTitleBytes {
		title = truncateUTF8(title, workerproto.MaxSessionEvidenceTitleBytes)
	}
	return title
}

func boundedRuleTexts(rules []string) []string {
	out := make([]string, 0, len(rules))
	for index, rule := range rules {
		if index >= workerproto.MaxSessionEvidenceRules {
			break
		}
		if len(rule) > workerproto.MaxSessionEvidenceTextBytes {
			rule = truncateUTF8(rule, workerproto.MaxSessionEvidenceTextBytes)
		}
		out = append(out, rule)
	}
	return out
}

// evidenceReason is one bounded, single-line reason: the error's own words, never a host path
// beyond what the error already carried into a log line.
func evidenceReason(prefix string, err error) string {
	reason := prefix
	if err != nil {
		reason += ": " + strings.Join(strings.Fields(err.Error()), " ")
	}
	if len(reason) > workerproto.MaxSessionEvidenceTextBytes {
		reason = truncateUTF8(reason, workerproto.MaxSessionEvidenceTextBytes)
	}
	return reason
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func text(value string) *string { return &value }

func optionalText(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if len(value) > workerproto.MaxSessionEvidenceTextBytes {
		value = truncateUTF8(value, workerproto.MaxSessionEvidenceTextBytes)
	}
	return &value
}

func firstText(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func countText(value networkview.Count) string { return strconv.FormatUint(uint64(value), 10) }

func optionalCount(value *networkview.Count) *string {
	if value == nil {
		return nil
	}
	return text(countText(*value))
}

func utcTime(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}
