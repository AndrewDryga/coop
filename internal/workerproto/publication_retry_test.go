package workerproto

import (
	"errors"
	"strings"
	"testing"
)

func TestPublicationRetryDiagnosticNeverPersistsArbitraryErrors(t *testing.T) {
	for _, err := range []error{errors.New("private-token-and-provider-body"), &PublicationRetryError{Stage: "private-token-and-provider-body", HTTPStatus: 503}} {
		code, detail := PublicationRetryDiagnostic(err)
		if code != "publication_retry_failed" || strings.Contains(detail, "private-token") {
			t.Fatalf("untrusted diagnostic: %q %q", code, detail)
		}
	}
	code, detail := PublicationRetryDiagnostic(&PublicationRetryError{Stage: "grant", HTTPStatus: 503})
	if code != "publication_grant_failed" || detail != "Publication stage: grant. HTTP status: 503." {
		t.Fatalf("diagnostic = %q %q", code, detail)
	}
}
