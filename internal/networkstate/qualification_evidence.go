package networkstate

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/networkview"
)

const MaxQualificationEvidenceBytes = 2 << 20

// QualificationEvidence retains the exact sealed terminal frame plus bounded
// facts from the trusted host preflight (image identities, engine version,
// timings). It is private evidence, not a caller-supplied success assertion.
type QualificationEvidence struct {
	Version  int                  `json:"version"`
	Snapshot networkview.Snapshot `json:"network"`
	Facts    map[string]string    `json:"host_facts"`
}

// QualificationEvidenceBytes encodes what the setup workflow retains alongside
// its record. Storage still verifies the frame against the sealed receipt.
func QualificationEvidenceBytes(snapshot networkview.Snapshot, facts map[string]string) ([]byte, error) {
	return json.Marshal(QualificationEvidence{Version: 1, Snapshot: snapshot, Facts: facts})
}

func validateQualificationEvidence(data []byte, receipt *networkview.Receipt) error {
	if len(data) == 0 || len(data) > MaxQualificationEvidenceBytes || receipt == nil {
		return errors.New("qualification requires bounded evidence from its sealed receipt")
	}
	var evidence QualificationEvidence
	if strictJSON(data, &evidence) != nil || evidence.Version != 1 || !equalJSON(evidence.Snapshot, receipt.Snapshot) ||
		len(evidence.Facts) == 0 || len(evidence.Facts) > 128 {
		return errors.New("qualification evidence differs from its sealed terminal observation")
	}
	remaining := 64 << 10
	for name, value := range evidence.Facts {
		if name == "" || len(name) > 128 || len(value) > 8<<10 || !utf8.ValidString(name) || !utf8.ValidString(value) || strings.ContainsAny(name, "\x00\r\n") || strings.ContainsRune(value, '\x00') {
			return errors.New("invalid qualification host evidence fact")
		}
		remaining -= len(name) + len(value)
		if remaining < 0 {
			return errors.New("qualification host evidence facts exceed their byte limit")
		}
	}
	return nil
}
