package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type consultTrace struct {
	Step        int    `json:"step"`
	Provider    string `json:"provider"`
	Delivery    string `json:"delivery"`
	Result      string `json:"result"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	SessionHash string `json:"session_hash,omitempty"`
	PromptHash  string `json:"prompt_hash,omitempty"`
}

type consultScenario struct {
	Calls     []consultCall `json:"calls"`
	Steps     []consultStep `json:"steps"`
	BlockTask string        `json:"block_task,omitempty"`
}

type consultCall struct {
	Target     string `json:"target"`
	Mode       string `json:"mode"`
	Prompt     string `json:"prompt"`
	PromptKind string `json:"prompt_kind,omitempty"`
	ExitCode   int    `json:"exit_code"`
	DropID     bool   `json:"drop_id,omitempty"`
}

type consultStep struct {
	Provider    string `json:"provider"`
	Delivery    string `json:"delivery"`
	Result      string `json:"result"`
	Prompt      string `json:"prompt"`
	Reply       string `json:"reply,omitempty"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Diagnostics string `json:"diagnostics,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
	UsageIn     int    `json:"usage_in,omitempty"`
	UsageOut    int    `json:"usage_out,omitempty"`
}

type consultCursor struct {
	Index    int               `json:"index"`
	Sessions map[string]string `json:"sessions,omitempty"`
}

func validateConsultWrapperMount(root string, m mount) error {
	if !m.ReadOnly || m.Named || m.Target != fusion.ConsultWrapperPath {
		return errors.New("consult wrapper must be an exact read-only bind mount")
	}
	f, err := procharness.OpenRegularFile(root, m.Source, os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("consult wrapper source: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o755 {
		return fmt.Errorf("consult wrapper mode is %04o, want 0755", info.Mode().Perm())
	}
	want := fusion.ConsultWrapper()
	data, err := io.ReadAll(io.LimitReader(f, int64(len(want))+1))
	if err != nil {
		return err
	}
	if string(data) != want {
		return errors.New("consult wrapper bytes do not match the generated wrapper")
	}
	return nil
}

func validateConsultMountCardinality(run runCommand, consult bool) error {
	count := 0
	for _, m := range run.Mounts {
		if m.Target == fusion.ConsultWrapperPath {
			count++
		}
	}
	want := 0
	if consult {
		want = 1
	}
	if count != want {
		return fmt.Errorf("run has %d consult wrapper mounts, want %d", count, want)
	}
	return nil
}

func consultPersonaTarget(target string) bool {
	dir, base := filepath.Split(filepath.Clean(target))
	if strings.TrimRight(dir, string(filepath.Separator)) != "/home/node/.coop/consult" || !strings.HasSuffix(base, ".md") {
		return false
	}
	_, err := providerToken(strings.TrimSuffix(base, ".md"))
	return err == nil
}

func serveConsultRuntime(root, image, trace, scenarioPath string, run runCommand, s scenario) (int, error) {
	wrapper := ""
	for _, m := range run.Mounts {
		if m.Target != fusion.ConsultWrapperPath {
			continue
		}
		if wrapper != "" {
			return 1, errors.New("run has more than one consult wrapper mount")
		}
		if err := validateConsultWrapperMount(root, m); err != nil {
			return 1, err
		}
		wrapper = m.Source
	}
	if wrapper == "" {
		return 1, errors.New("consult scenario requires the generated consult wrapper mount")
	}
	if err := resetConsultCursor(root); err != nil {
		return 1, err
	}

	lastExit := 0
	for index, call := range s.Consult.Calls {
		if call.DropID {
			if err := dropConsultID(root, call.Target); err != nil {
				return 1, err
			}
		}
		prompt := consultCallPrompt(call)
		argv := []string{call.Target, "--" + call.Mode}
		if call.PromptKind == "" {
			argv = append(argv, prompt)
		}
		callTrace := &consultTrace{Step: index, Provider: call.Target, Delivery: call.Mode, PromptHash: traceValue(prompt)}
		if err := record(root, trace, traceRecord{Source: "consult", Event: "start", PID: os.Getpid(), Argv: traceProviderArgv(append([]string{"coop-consult"}, argv...)), Consult: callTrace}); err != nil {
			return 1, err
		}
		cmd := exec.Command(wrapper, argv...)
		cmd.Dir = run.HostWorkdir
		env, err := consultEnvironment(root, image, trace, scenarioPath, run)
		if err != nil {
			return 1, err
		}
		cmd.Env = env
		if call.PromptKind == "oversized-input" {
			cmd.Stdin = strings.NewReader(prompt)
		}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		var signal string
		lastExit, signal = commandExit(err)
		if lastExit < 0 {
			lastExit = 1
		}
		if err := record(root, trace, traceRecord{Source: "consult", Event: "exit", PID: os.Getpid(), ExitCode: &lastExit, Signal: signal, Consult: callTrace}); err != nil {
			return 1, err
		}
		if lastExit != call.ExitCode {
			return lastExit, fmt.Errorf("consult call %d exit %d, want %d", index, lastExit, call.ExitCode)
		}
	}
	cursor, err := readConsultCursor(root)
	if err != nil {
		return 1, err
	}
	if cursor.Index != len(s.Consult.Steps) {
		return 1, fmt.Errorf("consult consumed %d of %d planned peer steps", cursor.Index, len(s.Consult.Steps))
	}
	if s.Consult.BlockTask != "" {
		if err := blockConsultTask(root, run.HostWorkdir, s.Consult.BlockTask); err != nil {
			return 1, err
		}
	}
	if lastExit != 0 {
		return lastExit, fmt.Errorf("final consult call exited %d", lastExit)
	}
	return 0, nil
}

func blockConsultTask(root, workdir, id string) error {
	if !safeTaskID(id) {
		return errors.New("consult block task id is invalid")
	}
	tasks := filepath.Join(workdir, ".agent", "tasks")
	source := filepath.Join(tasks, "10_in_progress", id)
	destinationDir := filepath.Join(tasks, "50_blocked")
	destination := filepath.Join(destinationDir, id)
	if err := os.MkdirAll(destinationDir, 0o755); err != nil {
		return err
	}
	for label, path := range map[string]string{"task": source, "blocked state": destinationDir} {
		canonical, err := procharness.CanonicalUnderRoot(root, path)
		if err != nil || canonical != path {
			return fmt.Errorf("consult %s path is unsafe", label)
		}
	}
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("consult task to block is not one regular in-progress directory")
	}
	decision, err := os.OpenFile(filepath.Join(source, "decision.md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("consult task to block cannot publish its fixed decision")
	}
	decisionBody := "# Decision: deterministic fixture stop\n\n**Resolution:** <!-- intentionally unresolved -->\n"
	if _, err := io.WriteString(decision, decisionBody); err != nil {
		decision.Close()
		return err
	}
	if err := decision.Close(); err != nil {
		return err
	}
	blockedInfo, err := os.Lstat(destinationDir)
	if err != nil || !blockedInfo.IsDir() || blockedInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("consult blocked state is not a regular directory")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return errors.New("consult blocked task destination already exists")
	}
	return os.Rename(source, destination)
}

func safeTaskID(id string) bool {
	if id == "" || len(id) > 160 || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func dropConsultID(root, target string) error {
	key := strings.ToUpper(strings.ReplaceAll(target, "-", "_"))
	path := filepath.Join(root, "tmp", "coop-consult-state", key+".state")
	f, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("drop consult id: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, (512<<10)+2049))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	parts := bytes.SplitN(data, []byte("\n"), 6)
	if len(parts) != 6 || string(parts[0]) != "COOP-CONSULT-STATE 1" || !bytes.HasPrefix(parts[3], []byte("id=")) || len(parts[4]) != 0 {
		return errors.New("drop consult id: continuation record is invalid")
	}
	parts[3] = []byte("id=")
	tmp := path + ".fixture"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = out.Close()
		if remove {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := out.Write(bytes.Join(parts, []byte("\n"))); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	remove = false
	return nil
}

func consultCallPrompt(call consultCall) string {
	if call.PromptKind == "oversized-input" {
		return strings.Repeat("q", (512<<10)+1)
	}
	return call.Prompt
}

func serveConsultPeer(root, trace, scenarioPath, provider string, args []string) error {
	if _, err := providerToken(provider); err != nil {
		return err
	}
	s, err := readScenario(root, scenarioPath)
	if err != nil {
		return err
	}
	if s.Consult == nil {
		return errors.New("provider alias requires a consult scenario")
	}
	stepIndex, step, invocation, err := consumeConsultStep(root, provider, args, s.Consult.Steps)
	if err != nil {
		return err
	}
	meta := &consultTrace{
		Step: stepIndex, Provider: provider, Delivery: invocation.Delivery, Result: step.Result,
		Model: invocation.Model, Effort: invocation.Effort, PromptHash: traceValue(invocation.Prompt),
	}
	if invocation.Session != "" {
		meta.SessionHash = traceValue(invocation.Session)
	} else if provider == "codex" && (step.Result == "usable" || step.Result == "large-reply") && step.Delivery == "fresh" {
		meta.SessionHash = traceValue(codexFixtureSession(stepIndex))
	}
	if err := record(root, trace, traceRecord{Source: "peer", Event: "start", PID: os.Getpid(), ParentPID: os.Getppid(), Argv: traceProviderArgv(append([]string{provider}, args...)), Environment: traceEnvironment(environmentMap(os.Environ())), Consult: meta}); err != nil {
		return err
	}
	if step.Result == "timeout" {
		if err := record(root, trace, traceRecord{Source: "peer", Event: "ready", PID: os.Getpid(), Consult: meta}); err != nil {
			return err
		}
	}
	exitCode, signal, err := renderConsultStep(stepIndex, step)
	if err != nil {
		return err
	}
	if err := record(root, trace, traceRecord{Source: "peer", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Signal: signal, Consult: meta}); err != nil {
		return err
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

type consultInvocation struct {
	Delivery string
	Prompt   string
	Session  string
	Model    string
	Effort   string
}

func consumeConsultStep(root, provider string, args []string, steps []consultStep) (int, consultStep, consultInvocation, error) {
	path := filepath.Join(root, "state", consultCursorFileName)
	f, err := procharness.OpenRegularFile(root, path, os.O_RDWR)
	if err != nil {
		return 0, consultStep{}, consultInvocation{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, consultStep{}, consultInvocation{}, err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	cursor, err := decodeConsultCursor(f)
	if err != nil {
		return 0, consultStep{}, consultInvocation{}, err
	}
	if cursor.Index >= len(steps) {
		return 0, consultStep{}, consultInvocation{}, errors.New("consult provider invoked after the planned steps were exhausted")
	}
	stepIndex := cursor.Index
	step := steps[stepIndex]
	if step.Provider != provider {
		return 0, consultStep{}, consultInvocation{}, fmt.Errorf("consult step %d expected provider %q, got %q", stepIndex, step.Provider, provider)
	}
	invocation, err := parseConsultInvocation(provider, args)
	if err != nil {
		return 0, consultStep{}, consultInvocation{}, err
	}
	if invocation.Delivery != step.Delivery || invocation.Prompt != step.Prompt || invocation.Model != step.Model || invocation.Effort != step.Effort {
		return 0, consultStep{}, consultInvocation{}, fmt.Errorf("consult step %d invocation does not match the closed scenario", stepIndex)
	}
	if cursor.Sessions == nil {
		cursor.Sessions = map[string]string{}
	}
	key := consultSessionKey(provider, invocation.Model, invocation.Effort)
	if invocation.Delivery == "resume" {
		if invocation.Session == "" || cursor.Sessions[key] != invocation.Session {
			return 0, consultStep{}, consultInvocation{}, fmt.Errorf("consult step %d resumed an unknown %s session", stepIndex, provider)
		}
		if step.Result != "usable" {
			delete(cursor.Sessions, key)
		}
	} else if provider != "codex" && (step.Result == "usable" || step.Result == "large-reply") {
		cursor.Sessions[key] = invocation.Session
	} else if provider == "codex" && (step.Result == "usable" || step.Result == "large-reply") {
		cursor.Sessions[key] = codexFixtureSession(stepIndex)
	}
	cursor.Index++
	if err := encodeConsultCursor(f, cursor); err != nil {
		return 0, consultStep{}, consultInvocation{}, err
	}
	return stepIndex, step, invocation, nil
}

func parseConsultInvocation(provider string, args []string) (consultInvocation, error) {
	switch provider {
	case "claude":
		return parsePlainConsultArgs(args, []string{"-p", "--permission-mode", "plan"}, "--session-id", "--resume", "--effort", "")
	case "gemini":
		return parsePlainConsultArgs(args, []string{"--approval-mode", "plan"}, "--session-id", "--resume", "", "-p")
	case "grok":
		if len(args) < 2 || args[0] != "--tools" || args[1] == "" {
			return consultInvocation{}, errors.New("grok consult is missing its read-only tool set")
		}
		return parsePlainConsultArgs(args[2:], nil, "--session-id", "--resume", "--reasoning-effort", "-p")
	case "codex":
		return parseCodexConsultArgs(args)
	default:
		return consultInvocation{}, fmt.Errorf("unsupported consult provider %q", provider)
	}
}

func parsePlainConsultArgs(args, prefix []string, freshFlag, resumeFlag, effortFlag, promptFlag string) (consultInvocation, error) {
	if len(args) < len(prefix)+3 || !slices.Equal(args[:len(prefix)], prefix) {
		return consultInvocation{}, errors.New("plain consult argv has an unexpected prefix")
	}
	args = args[len(prefix):]
	var invocation consultInvocation
	switch args[0] {
	case freshFlag:
		invocation.Delivery = "fresh"
	case resumeFlag:
		invocation.Delivery = "resume"
	default:
		return consultInvocation{}, errors.New("plain consult argv has no exact fresh/resume flag")
	}
	if len(args) < 2 || !safeConsultValue(args[1], 256) {
		return consultInvocation{}, errors.New("plain consult argv has an invalid session id")
	}
	invocation.Session = args[1]
	args = args[2:]
	if len(args) >= 2 && args[0] == "--model" {
		invocation.Model, args = args[1], args[2:]
	}
	if effortFlag != "" && len(args) >= 2 && args[0] == effortFlag {
		invocation.Effort, args = args[1], args[2:]
	}
	if promptFlag != "" {
		if len(args) < 1 || args[0] != promptFlag {
			return consultInvocation{}, errors.New("plain consult argv has no exact prompt flag")
		}
		args = args[1:]
	}
	if len(args) != 1 || !safeConsultValue(args[0], 512<<10) || !safeConsultValue(invocation.Model, 256) || !safeConsultValue(invocation.Effort, 64) {
		return consultInvocation{}, errors.New("plain consult argv has unexpected trailing values")
	}
	invocation.Prompt = args[0]
	return invocation, nil
}

func parseCodexConsultArgs(args []string) (consultInvocation, error) {
	var invocation consultInvocation
	switch {
	case len(args) >= 3 && slices.Equal(args[:3], []string{"exec", "-s", "read-only"}):
		invocation.Delivery = "fresh"
		args = args[3:]
	case len(args) >= 5 && args[0] == "exec" && args[1] == "resume" && safeConsultValue(args[2], 256) && args[3] == "-c" && args[4] == "sandbox_mode=read-only":
		invocation.Delivery, invocation.Session = "resume", args[2]
		args = args[5:]
	default:
		return consultInvocation{}, errors.New("codex consult argv has no exact read-only fresh/resume prefix")
	}
	if len(args) >= 2 && args[0] == "--model" {
		invocation.Model, args = args[1], args[2:]
	}
	if len(args) >= 2 && args[0] == "-c" && strings.HasPrefix(args[1], "model_reasoning_effort=") {
		invocation.Effort = strings.TrimPrefix(args[1], "model_reasoning_effort=")
		args = args[2:]
	}
	if len(args) != 2 || args[0] != "--json" || !safeConsultValue(args[1], 512<<10) || !safeConsultValue(invocation.Model, 256) || !safeConsultValue(invocation.Effort, 64) {
		return consultInvocation{}, errors.New("codex consult argv has unexpected trailing values")
	}
	invocation.Prompt = args[1]
	return invocation, nil
}

func renderConsultStep(stepIndex int, step consultStep) (int, string, error) {
	if step.Result == "timeout" {
		return waitForConsultSignal()
	}
	if step.Diagnostics != "" {
		fmt.Fprintln(os.Stderr, step.Diagnostics)
	}
	if step.Result == "diagnostic-overflow" {
		_, err := io.WriteString(os.Stderr, strings.Repeat("d", consultOverflowBytes))
		return 0, "", err
	}
	if step.Provider == "codex" {
		return renderCodexConsultStep(stepIndex, step)
	}
	switch step.Result {
	case "usable":
		fmt.Fprintln(os.Stdout, step.Reply)
		return 0, "", nil
	case "large-reply":
		_, err := io.WriteString(os.Stdout, strings.Repeat("r", largeConsultReplyBytes))
		return 0, "", err
	case "empty", "stderr-only":
		return 0, "", nil
	case "ordinary":
		return defaultConsultExit(step.ExitCode, 23), "", nil
	case "failed-resume":
		return defaultConsultExit(step.ExitCode, 42), "", nil
	case "limited":
		if step.Diagnostics == "" {
			fmt.Fprintln(os.Stderr, "You've hit your weekly limit")
		}
		return defaultConsultExit(step.ExitCode, 75), "", nil
	case "overflow":
		_, err := io.WriteString(os.Stdout, strings.Repeat("x", consultOverflowBytes))
		return 0, "", err
	}
	return 1, "", fmt.Errorf("unsupported consult result %q", step.Result)
}

func renderCodexConsultStep(stepIndex int, step consultStep) (int, string, error) {
	encoder := json.NewEncoder(os.Stdout)
	switch step.Result {
	case "usable":
		if step.Delivery == "fresh" {
			if err := encoder.Encode(map[string]any{"type": "thread.started", "thread_id": codexFixtureSession(stepIndex)}); err != nil {
				return 1, "", err
			}
		}
		if err := encoder.Encode(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": step.Reply}}); err != nil {
			return 1, "", err
		}
		if step.UsageIn != 0 || step.UsageOut != 0 {
			if err := encoder.Encode(map[string]any{"type": "turn.completed", "usage": map[string]int{"input_tokens": step.UsageIn, "output_tokens": step.UsageOut}}); err != nil {
				return 1, "", err
			}
		}
		return 0, "", nil
	case "large-reply":
		if step.Delivery == "fresh" {
			if err := encoder.Encode(map[string]any{"type": "thread.started", "thread_id": codexFixtureSession(stepIndex)}); err != nil {
				return 1, "", err
			}
		}
		return 0, "", encoder.Encode(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": strings.Repeat("r", largeConsultReplyBytes)}})
	case "empty", "stderr-only":
		return 0, "", nil
	case "malformed":
		_, err := fmt.Fprintln(os.Stdout, "{not-json}")
		return 0, "", err
	case "ordinary", "failed-resume", "limited":
		message := "fixture ordinary failure"
		fallback := 23
		if step.Result == "failed-resume" {
			message, fallback = "fixture native session is missing", 42
		} else if step.Result == "limited" {
			message, fallback = "You've hit your weekly limit", 75
		}
		if err := encoder.Encode(map[string]any{"type": "error", "message": message}); err != nil {
			return 1, "", err
		}
		return defaultConsultExit(step.ExitCode, fallback), "", nil
	case "overflow":
		_, err := io.WriteString(os.Stdout, strings.Repeat("x", consultOverflowBytes))
		return 0, "", err
	}
	return 1, "", fmt.Errorf("unsupported Codex consult result %q", step.Result)
}

func defaultConsultExit(got, fallback int) int {
	if got != 0 {
		return got
	}
	return fallback
}

func codexFixtureSession(step int) string {
	return fmt.Sprintf("fixture-codex-session-%d", step)
}

func consultSessionKey(provider, model, effort string) string {
	digest := sha256.Sum256([]byte(provider + "\x00" + model + "\x00" + effort))
	return fmt.Sprintf("%x", digest[:])
}

func resetConsultCursor(root string) error {
	path := filepath.Join(root, "state", consultCursorFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(consultCursor{Sessions: map[string]string{}})
}

func readConsultCursor(root string) (consultCursor, error) {
	f, err := procharness.OpenRegularFile(root, filepath.Join(root, "state", consultCursorFileName), os.O_RDONLY)
	if err != nil {
		return consultCursor{}, err
	}
	defer f.Close()
	return decodeConsultCursor(f)
}

func decodeConsultCursor(r io.Reader) (consultCursor, error) {
	var cursor consultCursor
	decoder := json.NewDecoder(io.LimitReader(r, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return consultCursor{}, err
	}
	if cursor.Index < 0 || cursor.Index > maxConsultSteps || len(cursor.Sessions) > maxConsultSteps {
		return consultCursor{}, errors.New("consult cursor is outside its bounds")
	}
	return cursor, nil
}

func encodeConsultCursor(f *os.File, cursor consultCursor) error {
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

func safeConsultValue(value string, max int) bool {
	if len(value) > max || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	for _, r := range value {
		if r == 0x7f || (r < 0x20 && r != '\n' && r != '\t') {
			return false
		}
	}
	return true
}

func validateConsultScenario(lead string, homes map[string]bool, consult consultScenario) error {
	if len(consult.Calls) == 0 || len(consult.Calls) > maxConsultCalls {
		return fmt.Errorf("consult scenario has %d calls, want 1..%d", len(consult.Calls), maxConsultCalls)
	}
	if len(consult.Steps) > maxConsultSteps {
		return fmt.Errorf("consult scenario has %d steps, max %d", len(consult.Steps), maxConsultSteps)
	}
	if !homes[lead] {
		return fmt.Errorf("consult lead %q is missing from provider_homes", lead)
	}
	if consult.BlockTask != "" && !safeTaskID(consult.BlockTask) {
		return errors.New("consult block_task is not a safe task id")
	}
	for i, call := range consult.Calls {
		if _, err := providerToken(call.Target); err != nil {
			return fmt.Errorf("consult call %d target: %w", i, err)
		}
		if call.Mode != "fresh" && call.Mode != "continue" {
			return fmt.Errorf("consult call %d mode %q is unsupported", i, call.Mode)
		}
		if call.DropID && (call.Mode != "continue" || i == 0) {
			return fmt.Errorf("consult call %d can drop an id only on a later continue", i)
		}
		switch call.PromptKind {
		case "":
			if !safeConsultValue(call.Prompt, 32<<10) {
				return fmt.Errorf("consult call %d prompt is invalid or exceeds 32 KiB", i)
			}
		case "oversized-input":
			if call.Prompt != "" {
				return fmt.Errorf("consult call %d oversized-input prompt must be generated, not supplied", i)
			}
		default:
			return fmt.Errorf("consult call %d prompt kind %q is unsupported", i, call.PromptKind)
		}
		if call.ExitCode < 0 || call.ExitCode > 255 {
			return fmt.Errorf("consult call %d exit code %d is outside 0..255", i, call.ExitCode)
		}
	}
	for i, step := range consult.Steps {
		if !slices.Contains(agents.Names(), step.Provider) {
			return fmt.Errorf("consult step %d provider %q is not registered", i, step.Provider)
		}
		if !homes[step.Provider] {
			return fmt.Errorf("consult step %d provider %q is missing from provider_homes", i, step.Provider)
		}
		if step.Delivery != "fresh" && step.Delivery != "resume" {
			return fmt.Errorf("consult step %d delivery %q is unsupported", i, step.Delivery)
		}
		if !safeConsultValue(step.Prompt, 32<<10) || !safeConsultValue(step.Reply, 4<<10) || !safeConsultValue(step.Diagnostics, 4<<10) ||
			!safeConsultValue(step.Model, 256) || !safeConsultValue(step.Effort, 64) {
			return fmt.Errorf("consult step %d contains invalid or oversized text", i)
		}
		if step.ExitCode < 0 || step.ExitCode > 255 || step.UsageIn < 0 || step.UsageOut < 0 || step.UsageIn > 1_000_000_000 || step.UsageOut > 1_000_000_000 {
			return fmt.Errorf("consult step %d contains an out-of-range number", i)
		}
		if step.Provider != "codex" && (step.UsageIn != 0 || step.UsageOut != 0) {
			return fmt.Errorf("consult step %d fabricates usage for plain-output provider %q", i, step.Provider)
		}
		if err := validateConsultResult(i, step); err != nil {
			return err
		}
	}
	return nil
}

func validateConsultResult(index int, step consultStep) error {
	blankReply := strings.TrimSpace(step.Reply) == ""
	switch step.Result {
	case "usable":
		if blankReply || step.ExitCode != 0 {
			return fmt.Errorf("consult step %d usable result requires a reply and zero exit code", index)
		}
	case "empty":
		if !blankReply || step.Diagnostics != "" || step.ExitCode != 0 || step.UsageIn != 0 || step.UsageOut != 0 {
			return fmt.Errorf("consult step %d empty result has incompatible fields", index)
		}
	case "stderr-only":
		if !blankReply || strings.TrimSpace(step.Diagnostics) == "" || step.ExitCode != 0 || step.UsageIn != 0 || step.UsageOut != 0 {
			return fmt.Errorf("consult step %d stderr-only result has incompatible fields", index)
		}
	case "ordinary", "limited":
		if !blankReply || step.UsageIn != 0 || step.UsageOut != 0 {
			return fmt.Errorf("consult step %d failure result has incompatible fields", index)
		}
	case "failed-resume":
		if step.Delivery != "resume" || !blankReply || step.UsageIn != 0 || step.UsageOut != 0 {
			return fmt.Errorf("consult step %d failed-resume result requires resume delivery", index)
		}
	case "timeout", "overflow", "diagnostic-overflow", "large-reply":
		if !blankReply || step.Diagnostics != "" || step.ExitCode != 0 || step.UsageIn != 0 || step.UsageOut != 0 {
			return fmt.Errorf("consult step %d %s result has incompatible fields", index, step.Result)
		}
	case "malformed":
		if step.Provider != "codex" || !blankReply || step.Diagnostics != "" || step.ExitCode != 0 || step.UsageIn != 0 || step.UsageOut != 0 {
			return fmt.Errorf("consult step %d malformed result is Codex-only", index)
		}
	default:
		return fmt.Errorf("consult step %d result %q is unsupported", index, step.Result)
	}
	return nil
}

func consultEnvironment(root, image, trace, scenarioPath string, run runCommand) ([]string, error) {
	values := cloneMap(run.Env)
	for key, value := range values {
		if !strings.HasPrefix(key, "COOP_CONSULT_") || !strings.HasSuffix(key, "_CONTRACT") {
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
			return nil, fmt.Errorf("consult contract %s has %d exact read-only mounts", key, matches)
		}
	}
	return providerEnvironment(root, image, trace, scenarioPath, values), nil
}
