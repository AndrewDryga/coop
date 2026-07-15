package liveprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const childResultLimit int64 = 16 << 10

// ReadChildResult accepts exactly one bounded, root-owned result and validates it through the same
// summary contract used for persisted evidence.
func ReadChildResult(root, path, expectedProvider string) (ProviderResult, error) {
	if !agents.Valid(expectedProvider) {
		return ProviderResult{}, errors.New("invalid expected live provider")
	}
	file, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return ProviderResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 || info.Size() > childResultLimit {
		return ProviderResult{}, errors.New("live child result is empty, oversized, or unreadable")
	}
	decoder := json.NewDecoder(io.LimitReader(file, childResultLimit+1))
	decoder.DisallowUnknownFields()
	var result ProviderResult
	if err := decoder.Decode(&result); err != nil {
		return ProviderResult{}, errors.New("decode live child result")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ProviderResult{}, errors.New("live child result has trailing data")
	}
	if result.Provider != expectedProvider {
		return ProviderResult{}, errors.New("live child result provider mismatch")
	}
	if _, err := NewSummary(false, []agents.Target{{Provider: expectedProvider}}, []ProviderResult{result}); err != nil {
		return ProviderResult{}, errors.New("live child result violates summary contract")
	}
	return result, nil
}

// ControlFilePresent reports only a regular, single-link control file below root as present.
func ControlFilePresent(root, path string) bool {
	file, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return false
	}
	return file.Close() == nil
}

type ChildProcessObservation struct {
	Result           procharness.Result
	DeadlineExceeded bool
	Attempted        bool
}

// ProcessDeadlineExceeded derives timeout truth from the managed process result, not from a parent
// context that may expire after the child has already exited.
func ProcessDeadlineExceeded(result procharness.Result) bool {
	return errors.Is(result.Err, context.DeadlineExceeded)
}

// ClassifyChildProcess turns process state plus the separately-owned attempt marker into one stable
// result. A clean child wins over a racing expired context; otherwise deadlines retain whether the
// paid prompt had begun.
func ClassifyChildProcess(
	expectedProvider, root, resultPath string,
	observation ChildProcessObservation,
) ProviderResult {
	process := observation.Result
	clean := process.ExitCode == 0 && process.Err == nil &&
		!process.StdoutTruncated && !process.StderrTruncated
	if clean {
		result, err := ReadChildResult(root, resultPath, expectedProvider)
		if err != nil {
			return failedResult(expectedProvider, observation.Attempted, ReasonHarnessFailed, "harness", "harness", "child_result")
		}
		if result.Attempted != observation.Attempted {
			return failedResult(expectedProvider, result.Attempted || observation.Attempted,
				ReasonHarnessFailed, "harness", "harness", "attempt_mismatch")
		}
		return result
	}
	if observation.DeadlineExceeded {
		if observation.Attempted {
			result := failedResult(expectedProvider, true, ReasonPromptTimeout, "prompt", "timeout", "child_deadline")
			result.TimedOut = true
			return result
		}
		result := failedResult(expectedProvider, false, ReasonVersionProbe, "version", "timeout", "child_deadline")
		result.TimedOut = true
		return result
	}
	result := failedResult(expectedProvider, observation.Attempted, ReasonHarnessFailed, "harness", "harness", "child_process")
	result.ExitCode = process.ExitCode
	result.Truncated = process.StdoutTruncated || process.StderrTruncated
	return result
}

type VerificationFailures struct {
	CleanupFailed     bool
	SourceChanged     bool
	RepositoryChanged bool
	AttemptedObserved bool
}

// FinalizeResult applies the security precedence: cleanup, source integrity, repository integrity,
// then the child outcome. Higher-priority verification must not be masked by a provider failure.
func FinalizeResult(child ProviderResult, failures VerificationFailures) ProviderResult {
	attempted := child.Attempted || failures.AttemptedObserved
	override := func(reason, detail string) ProviderResult {
		result := failedResult(child.Provider, attempted, reason, "verification", "harness", detail)
		result.CLIVersion = child.CLIVersion
		return result
	}
	switch {
	case failures.CleanupFailed:
		return override(ReasonCleanupFailed, "runtime_cleanup")
	case failures.SourceChanged:
		return override(ReasonSourceChanged, "credential_source_changed")
	case failures.RepositoryChanged:
		return override(ReasonRepositoryChanged, "repository_verification")
	default:
		child.Attempted = attempted
		return child
	}
}

func failedResult(provider string, attempted bool, reason, phase, class, detail string) ProviderResult {
	return ProviderResult{
		Provider: provider, Attempted: attempted, Status: StatusFailed, ReasonCode: reason,
		Phase: phase, ErrorClass: class, DetailCode: detail,
	}
}
