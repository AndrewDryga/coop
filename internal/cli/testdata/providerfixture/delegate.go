package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const (
	maxDelegateCalls          = 4
	maxDelegateSteps          = 16
	delegateCursorFileName    = "delegate-cursor.json"
	delegateReadyFileName     = "delegate-ready"
	delegateReleaseFileName   = "delegate-release"
	delegateContenderFileName = "delegate-contender"
)

type delegateTrace struct {
	Step       int    `json:"step"`
	Provider   string `json:"provider"`
	Result     string `json:"result"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	PromptHash string `json:"prompt_hash,omitempty"`
}

type delegateScenario struct {
	Contract   string               `json:"contract"`
	Calls      []delegateCall       `json:"calls"`
	Steps      []delegateStep       `json:"steps"`
	Consult    *delegateConsultPlan `json:"consult,omitempty"`
	Concurrent bool                 `json:"concurrent,omitempty"`
}

type delegateConsultPlan struct {
	Steps []consultStep `json:"steps"`
}

type delegateCall struct {
	Role       string `json:"role"`
	Prompt     string `json:"prompt,omitempty"`
	PromptKind string `json:"prompt_kind,omitempty"`
	ExitCode   int    `json:"exit_code"`
}

type delegateStep struct {
	Provider string `json:"provider"`
	Result   string `json:"result"`
	Prompt   string `json:"prompt"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

type delegateCursor struct {
	Index int `json:"index"`
}

type delegateInvocation struct {
	Prompt string
	Model  string
	Effort string
}

func validateDelegateWrapperMount(root string, m mount) error {
	return validateGeneratedWrapperMount(root, m, preset.DelegateWrapperPath, preset.DelegateWrapper(), "delegate")
}

func serveDelegateRuntime(root, image, trace, scenarioPath string, run runCommand, s scenario) (int, error) {
	delegate := s.Delegate
	if delegate == nil {
		return 1, errors.New("delegate runtime requires a delegate scenario")
	}
	wrapper := ""
	for _, m := range run.Mounts {
		if m.Target != preset.DelegateWrapperPath {
			continue
		}
		if wrapper != "" {
			return 1, errors.New("run has more than one delegate wrapper mount")
		}
		if err := validateDelegateWrapperMount(root, m); err != nil {
			return 1, err
		}
		wrapper = m.Source
	}
	if wrapper == "" {
		return 1, errors.New("delegate scenario requires the generated delegate wrapper mount")
	}
	if err := validateDelegateContractMount(root, run, delegate.Contract); err != nil {
		return 1, err
	}
	if err := resetDelegateCursor(root); err != nil {
		return 1, err
	}
	if delegate.Consult != nil {
		if err := resetConsultCursor(root); err != nil {
			return 1, err
		}
	}

	lastExit := 0
	if delegate.Concurrent {
		var err error
		lastExit, err = serveConcurrentDelegates(root, image, trace, scenarioPath, wrapper, run, *delegate)
		if err != nil {
			return lastExit, err
		}
	} else {
		for index, call := range delegate.Calls {
			cmd, meta, err := newDelegateCommand(root, image, trace, scenarioPath, wrapper, run, index, call)
			if err != nil {
				return 1, err
			}
			lastExit, err = finishDelegateCommand(root, trace, index, call, meta, cmd.Run())
			if err != nil {
				return lastExit, err
			}
		}
	}
	cursor, err := readDelegateCursor(root)
	if err != nil {
		return 1, err
	}
	if cursor.Index != len(delegate.Steps) {
		return 1, fmt.Errorf("delegate consumed %d of %d planned peer steps", cursor.Index, len(delegate.Steps))
	}
	if delegate.Consult != nil {
		consultCursor, err := readConsultCursor(root)
		if err != nil {
			return 1, err
		}
		if consultCursor.Index != len(delegate.Consult.Steps) {
			return 1, fmt.Errorf("delegate consult consumed %d of %d planned peer steps", consultCursor.Index, len(delegate.Consult.Steps))
		}
	}
	if lastExit != 0 {
		return lastExit, fmt.Errorf("final delegate call exited %d", lastExit)
	}
	return 0, nil
}

func newDelegateCommand(root, image, trace, scenarioPath, wrapper string, run runCommand, index int, call delegateCall) (*exec.Cmd, *delegateTrace, error) {
	prompt := delegateCallPrompt(call)
	argv := []string{call.Role}
	if call.PromptKind == "" {
		argv = append(argv, prompt)
	}
	meta := &delegateTrace{Step: index, PromptHash: traceValue(prompt)}
	if err := record(root, trace, traceRecord{Source: "delegate", Event: "start", PID: os.Getpid(), Argv: traceProviderArgv(append([]string{"coop-delegate"}, argv...)), Delegate: meta}); err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(wrapper, argv...)
	cmd.Dir = run.HostWorkdir
	env, err := delegateEnvironment(root, image, trace, scenarioPath, run, wrapper, call.Role)
	if err != nil {
		return nil, nil, err
	}
	cmd.Env = append(env, "COOP_PROVIDER_FIXTURE_DELEGATE_CALL="+strconv.Itoa(index))
	if call.PromptKind == "oversized-input" {
		cmd.Stdin = strings.NewReader(prompt)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd, meta, nil
}

func finishDelegateCommand(root, trace string, index int, call delegateCall, meta *delegateTrace, runErr error) (int, error) {
	exitCode, _ := commandExit(runErr)
	if exitCode < 0 {
		exitCode = 1
	}
	if err := record(root, trace, traceRecord{Source: "delegate", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Delegate: meta}); err != nil {
		return 1, err
	}
	if exitCode != call.ExitCode {
		return exitCode, fmt.Errorf("delegate call %d exit %d, want %d", index, exitCode, call.ExitCode)
	}
	return exitCode, nil
}

func serveConcurrentDelegates(root, image, trace, scenarioPath, wrapper string, run runCommand, delegate delegateScenario) (int, error) {
	first, firstMeta, err := newDelegateCommand(root, image, trace, scenarioPath, wrapper, run, 0, delegate.Calls[0])
	if err != nil {
		return 1, err
	}
	if err := first.Start(); err != nil {
		return finishDelegateCommand(root, trace, 0, delegate.Calls[0], firstMeta, err)
	}
	defer func() { _ = first.Process.Kill() }()
	if err := awaitDelegateFile(root, delegateReadyFileName, 5*time.Second); err != nil {
		return 1, err
	}
	second, secondMeta, err := newDelegateCommand(root, image, trace, scenarioPath, wrapper, run, 1, delegate.Calls[1])
	if err != nil {
		return 1, err
	}
	if err := second.Start(); err != nil {
		return finishDelegateCommand(root, trace, 1, delegate.Calls[1], secondMeta, err)
	}
	defer func() { _ = second.Process.Kill() }()
	if err := awaitDelegateFile(root, delegateContenderFileName, 5*time.Second); err != nil {
		return 1, err
	}
	cursor, err := readDelegateCursor(root)
	if err != nil {
		return 1, err
	}
	if cursor.Index != 1 {
		return 1, fmt.Errorf("concurrent delegate dispatched %d providers before the first released its lock", cursor.Index)
	}
	if err := os.WriteFile(filepath.Join(root, "state", delegateReleaseFileName), []byte("release\n"), 0o600); err != nil {
		return 1, err
	}
	if _, err := finishDelegateCommand(root, trace, 0, delegate.Calls[0], firstMeta, first.Wait()); err != nil {
		return 1, err
	}
	return finishDelegateCommand(root, trace, 1, delegate.Calls[1], secondMeta, second.Wait())
}

func awaitDelegateFile(root, name string, timeout time.Duration) error {
	path := filepath.Join(root, "state", name)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("concurrent delegate marker %q did not appear", name)
}

func validateDelegateContractMount(root string, run runCommand, want string) error {
	matches := 0
	for key, value := range run.Env {
		if !strings.HasPrefix(key, "COOP_DELEGATE_") || !strings.HasSuffix(key, "_CONTRACT") {
			continue
		}
		for _, m := range run.Mounts {
			if m.Target != value || !m.ReadOnly || m.Named {
				continue
			}
			f, err := procharness.OpenRegularFile(root, m.Source, os.O_RDONLY)
			if err != nil {
				return err
			}
			data, readErr := io.ReadAll(io.LimitReader(f, 32<<10))
			closeErr := f.Close()
			if readErr != nil || closeErr != nil {
				return errors.New("cannot read delegate contract mount")
			}
			if string(data) != want {
				return errors.New("delegate contract mount bytes do not match the closed scenario")
			}
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("delegate scenario has %d exact contract mounts, want one", matches)
	}
	return nil
}

func serveDelegatePeer(root, trace, provider string, args []string, delegate delegateScenario) error {
	invocation, parseErr := parseDelegateInvocation(provider, args)
	if parseErr != nil && delegate.Consult != nil {
		return serveConsultPeerPlan(root, trace, provider, args, delegate.Consult.Steps)
	}
	if parseErr != nil {
		return parseErr
	}
	stepIndex, step, err := consumeDelegateStep(root, provider, invocation, delegate.Steps)
	if err != nil {
		return err
	}
	meta := &delegateTrace{Step: stepIndex, Provider: provider, Result: step.Result, Model: invocation.Model, Effort: invocation.Effort, PromptHash: traceValue(invocation.Prompt)}
	if err := record(root, trace, traceRecord{Source: "delegate-peer", Event: "start", PID: os.Getpid(), ParentPID: os.Getppid(), Argv: traceProviderArgv(append([]string{provider}, args...)), Environment: traceEnvironment(environmentMap(os.Environ())), Delegate: meta}); err != nil {
		return err
	}
	if step.Result == "wait" {
		if err := os.WriteFile(filepath.Join(root, "state", delegateReadyFileName), []byte("ready\n"), 0o600); err != nil {
			return err
		}
		if err := record(root, trace, traceRecord{Source: "delegate-peer", Event: "ready", PID: os.Getpid(), Delegate: meta}); err != nil {
			return err
		}
	}
	exitCode, signal, err := renderDelegateStep(root, step, delegate)
	if err != nil {
		return err
	}
	if err := record(root, trace, traceRecord{Source: "delegate-peer", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Signal: signal, Delegate: meta}); err != nil {
		return err
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func consumeDelegateStep(root, provider string, invocation delegateInvocation, steps []delegateStep) (int, delegateStep, error) {
	path := filepath.Join(root, "state", delegateCursorFileName)
	f, err := procharness.OpenRegularFile(root, path, os.O_RDWR)
	if err != nil {
		return 0, delegateStep{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, delegateStep{}, err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	cursor, err := decodeDelegateCursor(f)
	if err != nil {
		return 0, delegateStep{}, err
	}
	if cursor.Index >= len(steps) {
		return 0, delegateStep{}, errors.New("delegate provider invoked after the planned steps were exhausted")
	}
	index := cursor.Index
	step := steps[index]
	if step.Provider != provider || step.Prompt != invocation.Prompt || step.Model != invocation.Model || step.Effort != invocation.Effort {
		return 0, delegateStep{}, fmt.Errorf("delegate step %d invocation does not match the closed scenario", index)
	}
	cursor.Index++
	if err := encodeDelegateCursor(f, cursor); err != nil {
		return 0, delegateStep{}, err
	}
	return index, step, nil
}

func parseDelegateInvocation(provider string, args []string) (delegateInvocation, error) {
	switch provider {
	case "claude":
		return parsePlainDelegateArgs(args, []string{"-p", "--dangerously-skip-permissions"}, "--effort", "")
	case "gemini":
		return parsePlainDelegateArgs(args, []string{"--yolo"}, "", "-p")
	case "grok":
		return parsePlainDelegateArgs(args, []string{"--permission-mode", "bypassPermissions"}, "--reasoning-effort", "-p")
	case "codex":
		if len(args) < 2 || !slices.Equal(args[:2], []string{"exec", "--dangerously-bypass-approvals-and-sandbox"}) {
			return delegateInvocation{}, errors.New("codex delegate argv has an unexpected prefix")
		}
		invocation, rest := delegateInvocation{}, args[2:]
		if len(rest) >= 2 && rest[0] == "--model" {
			invocation.Model, rest = rest[1], rest[2:]
		}
		if len(rest) >= 2 && rest[0] == "-c" && strings.HasPrefix(rest[1], "model_reasoning_effort=") {
			invocation.Effort, rest = strings.TrimPrefix(rest[1], "model_reasoning_effort="), rest[2:]
		}
		if len(rest) != 1 || !safeConsultValue(rest[0], 96<<10) {
			return delegateInvocation{}, errors.New("codex delegate argv has unexpected trailing values")
		}
		invocation.Prompt = rest[0]
		return invocation, nil
	default:
		return delegateInvocation{}, fmt.Errorf("unsupported delegate provider %q", provider)
	}
}

func parsePlainDelegateArgs(args, prefix []string, effortFlag, promptFlag string) (delegateInvocation, error) {
	if len(args) < len(prefix)+1 || !slices.Equal(args[:len(prefix)], prefix) {
		return delegateInvocation{}, errors.New("plain delegate argv has an unexpected prefix")
	}
	invocation, rest := delegateInvocation{}, args[len(prefix):]
	if len(rest) >= 2 && rest[0] == "--model" {
		invocation.Model, rest = rest[1], rest[2:]
	}
	if effortFlag != "" && len(rest) >= 2 && rest[0] == effortFlag {
		invocation.Effort, rest = rest[1], rest[2:]
	}
	if promptFlag != "" {
		if len(rest) < 1 || rest[0] != promptFlag {
			return delegateInvocation{}, errors.New("plain delegate argv has no exact prompt flag")
		}
		rest = rest[1:]
	}
	if len(rest) != 1 || !safeConsultValue(rest[0], 96<<10) || !safeConsultValue(invocation.Model, 256) || !safeConsultValue(invocation.Effort, 64) {
		return delegateInvocation{}, errors.New("plain delegate argv has unexpected trailing values")
	}
	invocation.Prompt = rest[0]
	return invocation, nil
}

func renderDelegateStep(root string, step delegateStep, delegate delegateScenario) (int, string, error) {
	switch step.Result {
	case "success":
		fmt.Printf("fixture delegate success %s\n", step.Provider)
		return 0, "", nil
	case "edit":
		name := "delegate-edit-" + step.Provider + ".txt"
		if err := os.WriteFile(name, []byte("fixture delegate edit "+step.Provider+"\n"), 0o600); err != nil {
			return 1, "", err
		}
		fmt.Printf("fixture delegate edit %s\n", step.Provider)
		return 0, "", nil
	case "ordinary":
		fmt.Fprintln(os.Stderr, "fixture ordinary delegate failure")
		return defaultDelegateExit(step.ExitCode, 23), "", nil
	case "limited", "limited-edit", "limited-staged", "limited-ignored", "limited-ref", "limited-commit-reset":
		if err := applyDelegateMutation(step.Result); err != nil {
			return 1, "", err
		}
		fmt.Fprintln(os.Stderr, "usage limit reached")
		return defaultDelegateExit(step.ExitCode, 75), "", nil
	case "commit":
		if err := runFixtureGit("commit", "--allow-empty", "-qm", "fixture delegate commit"); err != nil {
			return 1, "", err
		}
		return 0, "", nil
	case "commit-reset":
		if err := applyDelegateMutation("limited-commit-reset"); err != nil {
			return 1, "", err
		}
		return 0, "", nil
	case "recursive":
		wrapper := os.Getenv("COOP_PROVIDER_FIXTURE_DELEGATE_WRAPPER")
		role := os.Getenv("COOP_PROVIDER_FIXTURE_DELEGATE_ROLE")
		cmd := exec.Command(wrapper, role, "nested fixture work")
		cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdout, os.Stderr, os.Environ()
		err := cmd.Run()
		if commandExitCodeFixture(err) != 2 {
			return 1, "", errors.New("recursive delegate did not fail with exit 2")
		}
		return 0, "", nil
	case "consult":
		if delegate.Consult == nil {
			return 1, "", errors.New("delegate consult result has no nested consult plan")
		}
		wrapper := os.Getenv("COOP_PROVIDER_FIXTURE_CONSULT_WRAPPER")
		cmd := exec.Command(wrapper, "advisor", "--fresh", "delegate nested question")
		cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdout, os.Stderr, os.Environ()
		if err := cmd.Run(); err != nil {
			return commandExitCodeFixture(err), "", err
		}
		return 0, "", nil
	case "wait":
		if err := awaitDelegateFile(root, delegateReleaseFileName, 5*time.Second); err != nil {
			return 1, "", err
		}
		return 0, "", nil
	}
	return 1, "", fmt.Errorf("unsupported delegate result %q", step.Result)
}

func applyDelegateMutation(result string) error {
	switch result {
	case "limited":
		return nil
	case "limited-edit":
		return os.WriteFile("delegate-partial.txt", []byte("partial\n"), 0o600)
	case "limited-staged":
		if err := os.WriteFile("delegate-staged.txt", []byte("staged\n"), 0o600); err != nil {
			return err
		}
		return runFixtureGit("add", "delegate-staged.txt")
	case "limited-ignored":
		return os.WriteFile(".delegate-ignored", []byte("ignored\n"), 0o600)
	case "limited-ref":
		return runFixtureGit("tag", "fixture-delegate-mutated")
	case "limited-commit-reset":
		if err := runFixtureGit("commit", "--allow-empty", "-qm", "fixture transient commit"); err != nil {
			return err
		}
		return runFixtureGit("reset", "--hard", "HEAD^")
	default:
		return fmt.Errorf("unsupported delegate mutation %q", result)
	}
}

func runFixtureGit(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func defaultDelegateExit(got, fallback int) int {
	if got != 0 {
		return got
	}
	return fallback
}

func commandExitCodeFixture(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}

func delegateCallPrompt(call delegateCall) string {
	if call.PromptKind == "oversized-input" {
		return strings.Repeat("p", (96<<10)+1)
	}
	return call.Prompt
}

func validateDelegateScenario(lead string, homes map[string]bool, delegate delegateScenario) error {
	if !homes[lead] {
		return fmt.Errorf("delegate lead %q is missing from provider_homes", lead)
	}
	if len(delegate.Calls) == 0 || len(delegate.Calls) > maxDelegateCalls || len(delegate.Steps) > maxDelegateSteps {
		return errors.New("delegate scenario calls or steps are outside bounds")
	}
	if !safeConsultValue(delegate.Contract, 16<<10) {
		return errors.New("delegate contract is invalid or exceeds 16 KiB")
	}
	for index, call := range delegate.Calls {
		if !safeRoleName(call.Role) || call.ExitCode < 0 || call.ExitCode > 255 {
			return fmt.Errorf("delegate call %d contains an invalid role or exit code", index)
		}
		switch call.PromptKind {
		case "":
			if !safeConsultValue(call.Prompt, 32<<10) || call.Prompt == "" {
				return fmt.Errorf("delegate call %d prompt is invalid", index)
			}
		case "oversized-input":
			if call.Prompt != "" {
				return fmt.Errorf("delegate call %d oversized prompt must be generated", index)
			}
		default:
			return fmt.Errorf("delegate call %d prompt kind %q is unsupported", index, call.PromptKind)
		}
	}
	for index, step := range delegate.Steps {
		if !slices.Contains(agents.Names(), step.Provider) || !homes[step.Provider] {
			return fmt.Errorf("delegate step %d provider %q is unavailable", index, step.Provider)
		}
		if !safeConsultValue(step.Prompt, 64<<10) || !safeConsultValue(step.Model, 256) || !safeConsultValue(step.Effort, 64) || step.ExitCode < 0 || step.ExitCode > 255 {
			return fmt.Errorf("delegate step %d contains invalid text or exit code", index)
		}
		switch step.Result {
		case "success", "edit", "commit", "commit-reset", "recursive", "consult", "wait":
			if step.ExitCode != 0 {
				return fmt.Errorf("delegate step %d success-shaped result has a nonzero exit code", index)
			}
		case "ordinary", "limited", "limited-edit", "limited-staged", "limited-ignored", "limited-ref", "limited-commit-reset":
		default:
			return fmt.Errorf("delegate step %d result %q is unsupported", index, step.Result)
		}
		if step.Result == "consult" && delegate.Consult == nil {
			return fmt.Errorf("delegate step %d consult result has no consult plan", index)
		}
	}
	if delegate.Consult != nil {
		if len(delegate.Consult.Steps) == 0 {
			return errors.New("delegate nested consult plan has no steps")
		}
		if err := validateConsultSteps(homes, delegate.Consult.Steps); err != nil {
			return fmt.Errorf("delegate nested consult: %w", err)
		}
	}
	if delegate.Concurrent {
		if len(delegate.Calls) != 2 || len(delegate.Steps) != 2 || delegate.Calls[0].ExitCode != 0 || delegate.Calls[1].ExitCode != 0 || delegate.Steps[0].Result != "wait" || delegate.Steps[1].Result != "success" {
			return errors.New("concurrent delegate scenario requires the fixed wait-then-success plan")
		}
	}
	return nil
}

func safeRoleName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func resetDelegateCursor(root string) error {
	path := filepath.Join(root, "state", delegateCursorFileName)
	for _, name := range []string{delegateReadyFileName, delegateReleaseFileName, delegateContenderFileName} {
		if err := os.Remove(filepath.Join(root, "state", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(delegateCursor{})
}

func readDelegateCursor(root string) (delegateCursor, error) {
	f, err := procharness.OpenRegularFile(root, filepath.Join(root, "state", delegateCursorFileName), os.O_RDONLY)
	if err != nil {
		return delegateCursor{}, err
	}
	defer f.Close()
	return decodeDelegateCursor(f)
}

func decodeDelegateCursor(r io.Reader) (delegateCursor, error) {
	var cursor delegateCursor
	decoder := json.NewDecoder(io.LimitReader(r, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return delegateCursor{}, err
	}
	if cursor.Index < 0 || cursor.Index > maxDelegateSteps {
		return delegateCursor{}, errors.New("delegate cursor is outside bounds")
	}
	return cursor, nil
}

func encodeDelegateCursor(f *os.File, cursor delegateCursor) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(cursor); err != nil {
		return err
	}
	return f.Sync()
}

func delegateEnvironment(root, image, trace, scenarioPath string, run runCommand, wrapper, role string) ([]string, error) {
	values := cloneMap(run.Env)
	for key, value := range values {
		if !(strings.HasPrefix(key, "COOP_DELEGATE_") || strings.HasPrefix(key, "COOP_CONSULT_")) || !strings.HasSuffix(key, "_CONTRACT") {
			continue
		}
		matches := 0
		for _, m := range run.Mounts {
			if m.Target == value && m.ReadOnly && !m.Named {
				values[key] = m.Source
				matches++
			}
		}
		if matches != 1 {
			return nil, fmt.Errorf("peer contract %s has %d exact read-only mounts", key, matches)
		}
	}
	values["COOP_DELEGATE_LOCK"] = filepath.Join(root, "tmp", "coop-delegate.lock")
	values["COOP_DELEGATE_TIMEOUT"] = "2"
	values["COOP_PROVIDER_FIXTURE_DELEGATE_WRAPPER"] = wrapper
	values["COOP_PROVIDER_FIXTURE_DELEGATE_ROLE"] = role
	if run.Env["COOP_CONSULT_ADVISOR_TARGETS"] != "" {
		for _, m := range run.Mounts {
			if m.Target == fusion.ConsultWrapperPath {
				values["COOP_PROVIDER_FIXTURE_CONSULT_WRAPPER"] = m.Source
			}
		}
	}
	return providerEnvironment(root, image, trace, scenarioPath, values), nil
}
