package workerproto

import (
	"errors"
	"fmt"
)

// PublicationRetryError carries only host-owned protocol facts. Provider bodies,
// Git output and credentials must never become a persisted publication diagnostic.
type PublicationRetryError struct {
	Stage      string
	HTTPStatus int
}

func (e *PublicationRetryError) Error() string {
	code, detail := PublicationRetryDiagnostic(e)
	return code + ": " + detail
}

func PublicationRetryDiagnostic(err error) (string, string) {
	code, detail := "publication_retry_failed", "The worker could not finish publishing; the saved intent will be retried."
	var retry *PublicationRetryError
	if !errors.As(err, &retry) || retry == nil {
		return code, detail
	}
	switch retry.Stage {
	case "credentials", "grant", "candidate", "repository", "pull_request_lookup", "branch_lookup", "lfs_upload", "git_push", "pull_request_write", "pull_request_confirmation":
		code = "publication_" + retry.Stage + "_failed"
		detail = "Publication stage: " + retry.Stage + "."
	default:
		return code, detail
	}
	if retry.HTTPStatus >= 100 && retry.HTTPStatus <= 599 {
		detail += fmt.Sprintf(" HTTP status: %d.", retry.HTTPStatus)
	}
	return code, detail
}
