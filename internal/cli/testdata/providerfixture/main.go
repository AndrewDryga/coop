package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const (
	traceVersion           = 1
	maxConsultCalls        = 4
	maxConsultSteps        = 16
	consultOverflowBytes   = (1 << 20) + 1
	largeConsultReplyBytes = 270000
	consultCursorFileName  = "consult-cursor.json"
)

type runtimeCommand struct {
	Kind string
	Run  runCommand
}

type runCommand struct {
	Image        string            `json:"image"`
	Workdir      string            `json:"workdir"`
	HostWorkdir  string            `json:"host_workdir"`
	Provider     string            `json:"provider"`
	ProviderArgv []string          `json:"provider_argv"`
	Mounts       []mount           `json:"mounts"`
	Environment  []envTrace        `json:"environment,omitempty"`
	Env          map[string]string `json:"-"`
	EnvFiles     []string          `json:"env_files,omitempty"`
	Labels       []string          `json:"labels,omitempty"`
	Network      string            `json:"network,omitempty"`
	Interactive  bool              `json:"interactive,omitempty"`
	TTY          bool              `json:"tty,omitempty"`
}

type mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	Named    bool   `json:"named,omitempty"`
}

type envTrace struct {
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

type traceRecord struct {
	Version     int            `json:"version"`
	Sequence    int            `json:"sequence"`
	Source      string         `json:"source"`
	Event       string         `json:"event"`
	PID         int            `json:"pid"`
	ParentPID   int            `json:"parent_pid,omitempty"`
	Argv        []string       `json:"argv,omitempty"`
	Cwd         string         `json:"cwd,omitempty"`
	Run         *runCommand    `json:"run,omitempty"`
	Environment []envTrace     `json:"environment,omitempty"`
	ExitCode    *int           `json:"exit_code,omitempty"`
	Signal      string         `json:"signal,omitempty"`
	Consult     *consultTrace  `json:"consult,omitempty"`
	Delegate    *delegateTrace `json:"delegate,omitempty"`
}

type scenario struct {
	Version       int               `json:"version"`
	Provider      string            `json:"provider"`
	ProviderHomes []string          `json:"provider_homes,omitempty"`
	Marker        string            `json:"marker"`
	Output        *string           `json:"output,omitempty"`
	Behavior      string            `json:"behavior,omitempty"`
	ExitCode      int               `json:"exit_code"`
	Consult       *consultScenario  `json:"consult,omitempty"`
	Delegate      *delegateScenario `json:"delegate,omitempty"`
	Loop          *loopScenario     `json:"loop,omitempty"`
}

func main() {
	root, image, trace, scenarioPath, err := fixtureConfig()
	if err != nil {
		fatalf("configuration: %v", err)
	}
	alias := filepath.Base(os.Args[0])
	if alias == "timeout" {
		if err := serveTimeout(root, trace, scenarioPath, os.Args[1:]); err != nil {
			exitFixtureAlias("timeout", err)
		}
		return
	} else if alias == "flock" {
		if err := serveFlock(root, trace, os.Args[1:]); err != nil {
			exitFixtureAlias("flock", err)
		}
		return
	} else if alias == "setsid" {
		if err := serveSetsid(root, trace, os.Args[1:]); err != nil {
			exitFixtureAlias("setsid", err)
		}
		return
	} else if slices.Contains(agents.Names(), alias) {
		if err := serveConsultPeer(root, trace, scenarioPath, alias, os.Args[1:]); err != nil {
			exitFixtureAlias("consult peer", err)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "provider" {
		if err := serveProvider(root, trace, scenarioPath, os.Args[2:]); err != nil {
			fatalf("provider: %v", err)
		}
		return
	}
	if err := serveRuntime(root, image, trace, scenarioPath, os.Args[1:]); err != nil {
		var status *fixtureExitError
		if errors.As(err, &status) {
			fmt.Fprintf(os.Stderr, "providerfixture: runtime: %v\n", err)
			os.Exit(status.Code)
		}
		_ = record(root, trace, traceRecord{Source: "runtime", Event: "error", PID: os.Getpid(), Argv: traceRuntimeArgv(root, image, os.Args[1:])})
		fatalf("runtime: %v", err)
	}
}

func fixtureConfig() (root, image, trace, scenarioPath string, err error) {
	root = os.Getenv("COOP_PROVIDER_FIXTURE_ROOT")
	if root == "" {
		return "", "", "", "", errors.New("COOP_PROVIDER_FIXTURE_ROOT is empty")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", "", "", fmt.Errorf("resolve root: %w", err)
	}
	self, selfErr := os.Executable()
	if selfErr != nil {
		return "", "", "", "", selfErr
	}
	if _, err = procharness.CanonicalUnderRoot(root, self); err != nil {
		return "", "", "", "", fmt.Errorf("fixture executable: %w", err)
	}
	image = os.Getenv("COOP_PROVIDER_FIXTURE_IMAGE")
	if image == "" {
		return "", "", "", "", errors.New("COOP_PROVIDER_FIXTURE_IMAGE is empty")
	}
	trace, err = procharness.CanonicalUnderRoot(root, os.Getenv("COOP_PROVIDER_FIXTURE_TRACE"))
	if err != nil {
		return "", "", "", "", fmt.Errorf("trace: %w", err)
	}
	scenarioPath, err = procharness.CanonicalUnderRoot(root, os.Getenv("COOP_PROVIDER_FIXTURE_SCENARIO"))
	if err != nil {
		return "", "", "", "", fmt.Errorf("scenario: %w", err)
	}
	for label, path := range map[string]string{"trace": trace, "scenario": scenarioPath} {
		f, openErr := procharness.OpenRegularFile(root, path, os.O_RDONLY)
		if openErr != nil {
			return "", "", "", "", fmt.Errorf("%s: %w", label, openErr)
		}
		f.Close()
	}
	return root, image, trace, scenarioPath, nil
}

func serveRuntime(root, image, trace, scenarioPath string, args []string) error {
	var activeScenario scenario
	var err error
	if len(args) > 0 && args[0] == "run" {
		activeScenario, err = readScenario(root, scenarioPath)
		if err != nil {
			return errors.New("scenario rejected")
		}
	}
	expectedProvider := activeScenario.Provider
	if activeScenario.Loop != nil {
		expectedProvider = ""
	}
	parsed, err := parseRuntimeForProvider(root, image, args, expectedProvider, activeScenario.ProviderHomes...)
	if err != nil {
		return errors.New("runtime invocation rejected")
	}
	if parsed.Kind == "run" {
		if err := validateWrapperMountCardinality(parsed.Run, fusion.ConsultWrapperPath, activeScenario.Consult != nil || (activeScenario.Delegate != nil && activeScenario.Delegate.Consult != nil)); err != nil {
			return errors.New("runtime invocation rejected")
		}
		if err := validateWrapperMountCardinality(parsed.Run, preset.DelegateWrapperPath, activeScenario.Delegate != nil); err != nil {
			return errors.New("runtime invocation rejected")
		}
	}
	if err := record(root, trace, traceRecord{Source: "runtime", Event: "invoke", PID: os.Getpid(), ParentPID: os.Getppid(), Argv: traceRuntimeArgv(root, image, args)}); err != nil {
		return err
	}
	switch parsed.Kind {
	case "version":
		fmt.Println("coop-provider-fixture 1")
		return nil
	case "info", "inspect", "ps", "remove", "kill":
		return nil
	case "run":
		if activeScenario.Loop == nil && activeScenario.Provider != parsed.Run.Provider {
			return fmt.Errorf("scenario provider %q does not match runtime provider %q", activeScenario.Provider, parsed.Run.Provider)
		}
		parsed.Run.Environment = traceEnvironment(parsed.Run.Env)
		traceRun := traceRunCommand(root, parsed.Run)
		if err := record(root, trace, traceRecord{Source: "runtime", Event: "run", PID: os.Getpid(), Run: &traceRun}); err != nil {
			return err
		}
		if activeScenario.Delegate != nil {
			exitCode, runErr := serveDelegateRuntime(root, image, trace, scenarioPath, parsed.Run, activeScenario)
			_ = record(root, trace, traceRecord{Source: "runtime", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode})
			if runErr != nil {
				return &fixtureExitError{Code: exitCode, Err: runErr}
			}
			return nil
		}
		if activeScenario.Consult != nil {
			exitCode, runErr := serveConsultRuntime(root, image, trace, scenarioPath, parsed.Run, activeScenario)
			_ = record(root, trace, traceRecord{Source: "runtime", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode})
			if runErr != nil {
				return &fixtureExitError{Code: exitCode, Err: runErr}
			}
			return nil
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		childArgs := []string{"provider", parsed.Run.Provider, "--"}
		childArgs = append(childArgs, parsed.Run.ProviderArgv...)
		cmd := exec.Command(self, childArgs...)
		cmd.Dir = parsed.Run.HostWorkdir
		cmd.Env = providerEnvironment(root, image, trace, scenarioPath, parsed.Run.Env)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err = cmd.Run()
		exitCode, signal := commandExit(err)
		_ = record(root, trace, traceRecord{Source: "runtime", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Signal: signal})
		if err != nil {
			if exitCode < 1 || exitCode > 255 {
				exitCode = 1
			}
			return &fixtureExitError{Code: exitCode, Err: fmt.Errorf("provider %s: %w", parsed.Run.Provider, err)}
		}
		return nil
	default:
		return fmt.Errorf("unsupported parsed command %q", parsed.Kind)
	}
}

func parseRuntime(root, image string, args []string, providerHomes ...string) (runtimeCommand, error) {
	return parseRuntimeForProvider(root, image, args, "", providerHomes...)
}

func parseRuntimeForProvider(root, image string, args []string, provider string, providerHomes ...string) (runtimeCommand, error) {
	if len(args) == 1 && args[0] == "--version" {
		return runtimeCommand{Kind: "version"}, nil
	}
	if len(args) == 1 && args[0] == "info" {
		return runtimeCommand{Kind: "info"}, nil
	}
	if len(args) == 3 && args[0] == "image" && args[1] == "inspect" && args[2] == image {
		return runtimeCommand{Kind: "inspect"}, nil
	}
	if len(args) > 0 && args[0] == "ps" {
		if err := parsePS(args[1:]); err != nil {
			return runtimeCommand{}, err
		}
		return runtimeCommand{Kind: "ps"}, nil
	}
	if len(args) > 1 && (args[0] == "rm" || args[0] == "kill") {
		if err := parseRemoval(args); err != nil {
			return runtimeCommand{}, err
		}
		kind := "remove"
		if args[0] == "kill" {
			kind = "kill"
		}
		return runtimeCommand{Kind: kind}, nil
	}
	if len(args) == 0 || args[0] != "run" {
		return runtimeCommand{}, fmt.Errorf("unsupported runtime command %q", strings.Join(args, " "))
	}
	run, err := parseRun(root, image, args[1:], provider, providerHomes)
	if err != nil {
		return runtimeCommand{}, err
	}
	return runtimeCommand{Kind: "run", Run: run}, nil
}

func parseRun(root, image string, args []string, provider string, providerHomes []string) (runCommand, error) {
	run := runCommand{Env: map[string]string{}}
	seen := map[string]bool{}
	imageAt := -1
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == image {
			imageAt = i
			break
		}
		switch arg {
		case "--rm":
			if seen[arg] {
				return runCommand{}, fmt.Errorf("duplicate runtime flag %s", arg)
			}
			seen[arg] = true
			i++
		case "-i":
			if run.Interactive {
				return runCommand{}, errors.New("duplicate interactive runtime flag")
			}
			run.Interactive = true
			i++
		case "-t":
			if run.TTY {
				return runCommand{}, errors.New("duplicate TTY runtime flag")
			}
			run.TTY = true
			i++
		case "-it":
			if run.Interactive || run.TTY {
				return runCommand{}, errors.New("duplicate interactive/TTY runtime flag")
			}
			run.Interactive, run.TTY = true, true
			i++
		case "--label", "-e", "-v", "--env-file", "--network", "-w", "--security-opt", "--cap-drop", "--pids-limit", "--memory", "--cpus":
			if i+1 >= len(args) || args[i+1] == image {
				return runCommand{}, fmt.Errorf("runtime flag %s has no value", arg)
			}
			value := args[i+1]
			if arg != "--label" && arg != "-e" && arg != "-v" && seen[arg] {
				return runCommand{}, fmt.Errorf("duplicate runtime flag %s", arg)
			}
			seen[arg] = true
			var err error
			switch arg {
			case "--label":
				if !strings.Contains(value, "=") {
					return runCommand{}, fmt.Errorf("invalid label %q", value)
				}
				run.Labels = append(run.Labels, value)
			case "-e":
				err = applyEnv(run.Env, value)
			case "-v":
				var m mount
				m, err = parseMount(root, value)
				if err == nil {
					run.Mounts = append(run.Mounts, m)
				}
			case "--env-file":
				var path string
				path, err = procharness.CanonicalUnderRoot(root, value)
				if err == nil {
					err = readEnvFile(root, run.Env, path)
					run.EnvFiles = append(run.EnvFiles, path)
				}
			case "--network":
				run.Network = value
			case "-w":
				run.Workdir = value
			}
			if err != nil {
				return runCommand{}, fmt.Errorf("runtime flag %s: %w", arg, err)
			}
			i += 2
		default:
			return runCommand{}, fmt.Errorf("unsupported runtime flag or positional %q", arg)
		}
	}
	if imageAt < 0 {
		return runCommand{}, fmt.Errorf("run command has no exact image %q", image)
	}
	if imageAt+1 >= len(args) {
		return runCommand{}, errors.New("run command has no provider argv")
	}
	tail := append([]string(nil), args[imageAt+1:]...)
	for _, arg := range tail {
		if arg == image {
			return runCommand{}, fmt.Errorf("image %q appears again in provider argv", image)
		}
	}
	executable, err := providerToken(tail[0])
	if err != nil {
		return runCommand{}, err
	}
	if provider == "" {
		provider = executable
	} else if _, err := providerToken(provider); err != nil {
		return runCommand{}, err
	}
	if run.Workdir == "" || !filepath.IsAbs(run.Workdir) || filepath.Clean(run.Workdir) != run.Workdir {
		return runCommand{}, fmt.Errorf("run workdir %q is not absolute", run.Workdir)
	}
	hostWorkdir, err := translateWorkdir(root, run.Workdir, run.Mounts)
	if err != nil {
		return runCommand{}, err
	}
	run.Image, run.Provider, run.ProviderArgv, run.HostWorkdir = image, provider, tail, hostWorkdir
	if err := validateMountPolicy(root, run, providerHomes); err != nil {
		return runCommand{}, err
	}
	return run, nil
}

func parseMount(root, value string) (mount, error) {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return mount{}, fmt.Errorf("invalid mount %q", value)
	}
	if len(parts) == 3 && parts[2] != "ro" {
		return mount{}, fmt.Errorf("unsupported mount mode %q", parts[2])
	}
	m := mount{Source: parts[0], Target: parts[1], ReadOnly: len(parts) == 3}
	if !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target {
		return mount{}, fmt.Errorf("mount target %q is not a clean absolute path", m.Target)
	}
	if !filepath.IsAbs(m.Source) {
		if m.Source != "coop-cache" && m.Source != "coop-asdf" {
			return mount{}, fmt.Errorf("unknown named volume %q", m.Source)
		}
		m.Named = true
		return m, nil
	}
	canonical, err := procharness.CanonicalUnderRoot(root, m.Source)
	if err != nil {
		return mount{}, err
	}
	m.Source = canonical
	return m, nil
}

func translateWorkdir(root, workdir string, mounts []mount) (string, error) {
	best := -1
	host := ""
	for _, m := range mounts {
		if m.Named {
			continue
		}
		rel, err := filepath.Rel(m.Target, workdir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(m.Target) <= best {
			continue
		}
		host = filepath.Join(m.Source, rel)
		best = len(m.Target)
	}
	if best < 0 {
		return "", fmt.Errorf("workdir %q is not covered by a host bind", workdir)
	}
	return procharness.CanonicalUnderRoot(root, host)
}

func validateMountPolicy(root string, run runCommand, providerHomes []string) error {
	repoMounts, reviewQueueMounts := 0, 0
	repoReadOnly := false
	for _, m := range run.Mounts {
		if m.Named {
			if (m.Source != "coop-cache" || m.Target != "/home/node/.cache") &&
				(m.Source != "coop-asdf" || m.Target != "/home/node/.asdf") {
				return fmt.Errorf("named volume %q has unexpected target %q", m.Source, m.Target)
			}
			if m.ReadOnly {
				return fmt.Errorf("named volume %q:%q must be writable", m.Source, m.Target)
			}
			continue
		}
		if m.Target == run.Workdir {
			repoMounts++
			repoReadOnly = m.ReadOnly
			if m.Source != run.HostWorkdir {
				return fmt.Errorf("repo mount %q:%q must be the translated workdir", m.Source, m.Target)
			}
			info, err := os.Stat(m.Source)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("repo mount source %q is not a directory", m.Source)
			}
			continue
		}
		if !m.ReadOnly && m.Source == filepath.Join(run.HostWorkdir, ".agent", "tasks") && m.Target == filepath.Join(run.Workdir, ".agent", "tasks") {
			reviewQueueMounts++
			continue
		}
		if provider, ok := credentialMountProvider(m.Target); ok && credentialSourceMatches(root, provider, m.Source) {
			if !slices.Contains(providerHomes, provider) {
				return fmt.Errorf("credential mount for %q is outside scenario provider_homes", provider)
			}
			if m.ReadOnly {
				return fmt.Errorf("credential mount %q:%q must be writable", m.Source, m.Target)
			}
			continue
		}
		if !m.ReadOnly {
			return fmt.Errorf("unexpected writable mount %q:%q", m.Source, m.Target)
		}
		if m.Target == fusion.ConsultWrapperPath {
			if err := validateConsultWrapperMount(root, m); err != nil {
				return err
			}
			continue
		}
		if m.Target == preset.DelegateWrapperPath {
			if err := validateDelegateWrapperMount(root, m); err != nil {
				return err
			}
			continue
		}
		if !pathAtOrBelow(run.Workdir, m.Target) && !pathAtOrBelow("/home/node", m.Target) {
			return fmt.Errorf("read-only mount target %q is outside the repo and fixture home", m.Target)
		}
		if err := validateGeneratedReadOnlyMount(root, run, m, providerHomes); err != nil {
			return err
		}
	}
	if repoMounts != 1 {
		return fmt.Errorf("run has %d repo mounts, want one", repoMounts)
	}
	if (repoReadOnly && reviewQueueMounts != 1) || (!repoReadOnly && reviewQueueMounts != 0) {
		return fmt.Errorf("run has repo read-only=%v and %d review queue mounts", repoReadOnly, reviewQueueMounts)
	}
	return nil
}

func validateGeneratedReadOnlyMount(root string, run runCommand, m mount, providerHomes []string) error {
	rel, err := filepath.Rel(filepath.Join(root, "tmp"), m.Source)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("read-only mount source %q is not generated fixture temp state", m.Source)
	}
	name := strings.Split(rel, string(filepath.Separator))[0]
	providerHome := "/home/node/." + run.Provider
	switch {
	case strings.HasPrefix(name, "coop-decoy-"):
		if !pathAtOrBelow(run.Workdir, m.Target) {
			return fmt.Errorf("decoy mount target %q is outside the repo", m.Target)
		}
	case strings.HasPrefix(name, "coop-mcp-"):
		if !scenarioProviderHomeTarget(m.Target, providerHomes) && !scenarioProviderHomeTarget(m.Target, agents.Names()) && !peerContractTarget(m.Target) && m.Target != "/home/node/.gitconfig" && m.Target != "/home/node/.coop-gitignore" {
			return fmt.Errorf("generated config mount target %q is outside the provider and git homes", m.Target)
		}
	case strings.HasPrefix(name, "coop-githooks-"):
		if m.Target != "/home/node/.coop-git-hooks" {
			return fmt.Errorf("generated git hooks target %q is unexpected", m.Target)
		}
	case strings.HasPrefix(name, "coop-agents-"):
		if !pathAtOrBelow(providerHome, m.Target) {
			return fmt.Errorf("generated agent mount target %q is outside the provider home", m.Target)
		}
	default:
		return fmt.Errorf("read-only mount source %q is not an allowed generated fixture class", m.Source)
	}
	return nil
}

func scenarioProviderHomeTarget(target string, providerHomes []string) bool {
	rel, err := filepath.Rel("/home/node", target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	if !strings.HasPrefix(first, ".") {
		return false
	}
	provider := strings.TrimPrefix(first, ".")
	if _, err := providerToken(provider); err != nil {
		return false
	}
	for _, allowed := range providerHomes {
		if provider == allowed {
			return true
		}
	}
	return false
}

func credentialMountProvider(target string) (string, bool) {
	dir, base := filepath.Split(filepath.Clean(target))
	if strings.TrimRight(dir, string(filepath.Separator)) != "/home/node" || !strings.HasPrefix(base, ".") {
		return "", false
	}
	provider, err := providerToken(strings.TrimPrefix(base, "."))
	return provider, err == nil
}

func credentialSourceMatches(root, provider, source string) bool {
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return false
	}
	rel, err := filepath.Rel(filepath.Join(root, "config"), source)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) == 3 && parts[0] == provider && parts[1] == "profiles" && parts[2] != ""
}

func pathAtOrBelow(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func applyEnv(env map[string]string, value string) error {
	key, resolved, ok := strings.Cut(value, "=")
	if !ok {
		if value != "TERM" {
			return fmt.Errorf("bare environment key %q is unsupported", value)
		}
		key, resolved = value, os.Getenv(value)
	}
	if key == "" || strings.ContainsAny(key, " \t\r\n\x00") || strings.ContainsRune(resolved, '\x00') {
		return fmt.Errorf("invalid environment entry %q", value)
	}
	env[key] = resolved
	return nil
}

func readEnvFile(root string, env map[string]string, path string) error {
	f, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := applyEnv(env, line); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func providerToken(command string) (string, error) {
	if len(command) == 0 || len(command) > 64 || command[0] < 'a' || command[0] > 'z' {
		return "", fmt.Errorf("unsafe provider command %q", command)
	}
	for _, r := range command[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", fmt.Errorf("unsafe provider command %q", command)
		}
	}
	return command, nil
}

func parsePS(args []string) error {
	if len(args) == 0 || args[0] != "-q" {
		return fmt.Errorf("unsupported ps arguments %q", args)
	}
	for i := 1; i < len(args); i += 2 {
		if i+1 >= len(args) || args[i] != "--filter" || !strings.HasPrefix(args[i+1], "label=") {
			return fmt.Errorf("unsupported ps arguments %q", args)
		}
	}
	return nil
}

func parseRemoval(args []string) error {
	start := 1
	if args[0] == "rm" && len(args) > 1 && args[1] == "-f" {
		start = 2
	}
	if start >= len(args) {
		return fmt.Errorf("%s has no container id", args[0])
	}
	for _, id := range args[start:] {
		if id == "" || strings.HasPrefix(id, "-") || strings.ContainsAny(id, " \t\r\n") {
			return fmt.Errorf("unsafe container id %q", id)
		}
	}
	return nil
}

func serveProvider(root, trace, scenarioPath string, args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return errors.New("provider mode requires NAME -- ARGV")
	}
	provider := args[0]
	if _, err := providerToken(provider); err != nil {
		return err
	}
	providerArgv := append([]string(nil), args[2:]...)
	if len(providerArgv) == 0 {
		return fmt.Errorf("provider %s received no executable", provider)
	}
	if _, err := providerToken(providerArgv[0]); err != nil {
		return fmt.Errorf("provider %s executable: %w", provider, err)
	}
	s, err := readScenario(root, scenarioPath)
	if err != nil {
		return err
	}
	if s.Loop == nil && s.Provider != provider {
		return fmt.Errorf("scenario provider %q does not match %q", s.Provider, provider)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := procharness.CanonicalUnderRoot(root, cwd); err != nil {
		return fmt.Errorf("provider cwd: %w", err)
	}
	if err := record(root, trace, traceRecord{Source: "provider", Event: "start", PID: os.Getpid(), ParentPID: os.Getppid(), Argv: traceProviderArgv(providerArgv), Cwd: traceContainerPath(root, cwd), Environment: traceEnvironment(environmentMap(os.Environ()))}); err != nil {
		return err
	}
	if s.Loop != nil {
		exitCode, signal, runErr := serveLoopAttempt(root, trace, provider, providerArgv, *s.Loop)
		if err := record(root, trace, traceRecord{Source: "provider", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Signal: signal}); err != nil {
			return err
		}
		if runErr != nil {
			return runErr
		}
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return nil
	}
	if s.Output != nil {
		fmt.Fprint(os.Stdout, *s.Output)
	} else {
		marker := s.Marker
		if marker == "" {
			marker = "fixture-ok-" + provider
		}
		fmt.Println(marker)
	}
	if s.Behavior == "wait" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		if err := record(root, trace, traceRecord{Source: "provider", Event: "ready", PID: os.Getpid()}); err != nil {
			return err
		}
		got := <-signals
		sig, _ := got.(syscall.Signal)
		exitCode := 128 + int(sig)
		if err := record(root, trace, traceRecord{Source: "provider", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Signal: got.String()}); err != nil {
			return err
		}
		os.Exit(exitCode)
	}
	exitCode := s.ExitCode
	if err := record(root, trace, traceRecord{Source: "provider", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode}); err != nil {
		return err
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func readScenario(root, path string) (scenario, error) {
	const maxScenarioBytes = 64 << 10
	f, err := procharness.OpenRegularFile(root, path, os.O_RDONLY)
	if err != nil {
		return scenario{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxScenarioBytes+1))
	if err != nil {
		return scenario{}, err
	}
	if len(data) > maxScenarioBytes {
		return scenario{}, fmt.Errorf("scenario exceeds %d bytes", maxScenarioBytes)
	}
	var s scenario
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return scenario{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return scenario{}, errors.New("scenario contains more than one JSON value")
		}
		return scenario{}, err
	}
	if _, err := providerToken(s.Provider); err != nil {
		return scenario{}, err
	}
	seenHomes := map[string]bool{}
	for _, provider := range s.ProviderHomes {
		if _, err := providerToken(provider); err != nil {
			return scenario{}, fmt.Errorf("scenario provider home: %w", err)
		}
		if seenHomes[provider] {
			return scenario{}, fmt.Errorf("scenario provider home %q is duplicated", provider)
		}
		seenHomes[provider] = true
	}
	if s.Loop != nil {
		if s.Version != 6 {
			return scenario{}, fmt.Errorf("loop scenario version %d is unsupported", s.Version)
		}
		if s.Consult != nil || s.Delegate != nil || s.Marker != "" || s.Output != nil || s.Behavior != "" || s.ExitCode != 0 {
			return scenario{}, errors.New("loop scenario cannot set consult, delegate, or direct-provider result fields")
		}
		if err := validateLoopScenario(s.Provider, seenHomes, *s.Loop); err != nil {
			return scenario{}, err
		}
		return s, nil
	}
	if s.Delegate != nil {
		if s.Version != 3 {
			return scenario{}, fmt.Errorf("delegate scenario version %d is unsupported", s.Version)
		}
		if s.Consult != nil || s.Marker != "" || s.Output != nil || s.Behavior != "" || s.ExitCode != 0 {
			return scenario{}, errors.New("delegate scenario cannot set consult or direct-provider result fields")
		}
		if err := validateDelegateScenario(s.Provider, seenHomes, *s.Delegate); err != nil {
			return scenario{}, err
		}
		return s, nil
	}
	if s.Consult != nil {
		if s.Version != 2 {
			return scenario{}, fmt.Errorf("consult scenario version %d is unsupported", s.Version)
		}
		if s.Marker != "" || s.Output != nil || s.Behavior != "" || s.ExitCode != 0 {
			return scenario{}, errors.New("consult scenario cannot set direct-provider result fields")
		}
		if err := validateConsultScenario(s.Provider, seenHomes, *s.Consult); err != nil {
			return scenario{}, err
		}
		return s, nil
	}
	if s.Version != 1 {
		return scenario{}, fmt.Errorf("direct scenario version %d is unsupported", s.Version)
	}
	if s.ExitCode < 0 || s.ExitCode > 255 {
		return scenario{}, fmt.Errorf("scenario exit code %d is outside 0..255", s.ExitCode)
	}
	if s.Marker != "" && s.Output != nil {
		return scenario{}, errors.New("scenario cannot set both marker and output")
	}
	if err := validateScenarioText("marker", s.Marker); err != nil {
		return scenario{}, err
	}
	if s.Output != nil {
		if err := validateScenarioText("output", *s.Output); err != nil {
			return scenario{}, err
		}
	}
	switch s.Behavior {
	case "", "complete":
	case "wait":
		if s.ExitCode != 0 {
			return scenario{}, errors.New("wait scenario cannot set exit_code")
		}
	default:
		return scenario{}, fmt.Errorf("scenario behavior %q is unsupported", s.Behavior)
	}
	return s, nil
}

func validateScenarioText(label, value string) error {
	if len(value) > 4<<10 {
		return fmt.Errorf("scenario %s exceeds 4 KiB", label)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("scenario %s contains control characters", label)
		}
	}
	return nil
}

func providerEnvironment(root, image, trace, scenarioPath string, values map[string]string) []string {
	values = cloneMap(values)
	values["HOME"] = filepath.Join(root, "home")
	values["PATH"] = os.Getenv("PATH")
	values["TMPDIR"] = filepath.Join(root, "tmp")
	values["COOP_PROVIDER_FIXTURE_ROOT"] = root
	values["COOP_PROVIDER_FIXTURE_IMAGE"] = image
	values["COOP_PROVIDER_FIXTURE_TRACE"] = trace
	values["COOP_PROVIDER_FIXTURE_SCENARIO"] = scenarioPath
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func environmentMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		if key, value, ok := strings.Cut(item, "="); ok {
			out[key] = value
		}
	}
	return out
}

func traceEnvironment(env map[string]string) []envTrace {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]envTrace, 0, len(keys))
	for _, key := range keys {
		entry := envTrace{Name: key}
		if traceableEnvValue(key) {
			entry.Value = env[key]
		} else {
			entry.Redacted = true
		}
		out = append(out, entry)
	}
	return out
}

func traceableEnvValue(key string) bool {
	switch key {
	case "TZ", "TERM", "COOP_PRIMARY", "COOP_PEERS", "COOP_RUN_ID", "COOP_CONSULT_TIMEOUT", "COOP_DELEGATE_TIMEOUT", "COOP_DELEGATE_DEPTH", "FIXTURE_SAFE":
		return true
	}
	if strings.HasPrefix(key, "COOP_CONSULT_") && strings.HasSuffix(key, "_TARGETS") {
		return true
	}
	if strings.HasPrefix(key, "COOP_DELEGATE_") && strings.HasSuffix(key, "_TARGETS") {
		return true
	}
	return strings.HasPrefix(key, "COOP_PEER_MODEL_") || strings.HasPrefix(key, "COOP_PEER_EFFORT_") ||
		strings.HasSuffix(key, "_MODEL") || strings.HasSuffix(key, "_EFFORT") || strings.HasSuffix(key, "_EFFORT_LEVEL")
}

func traceRuntimeArgv(root, image string, args []string) []string {
	if len(args) == 0 {
		return []string{"<rejected>"}
	}
	if args[0] != "run" {
		switch args[0] {
		case "--version", "info":
			if len(args) == 1 {
				return append([]string(nil), args...)
			}
			return []string{"<rejected>"}
		case "image":
			if len(args) == 3 && args[1] == "inspect" && args[2] == image {
				return []string{"image", "inspect", image}
			}
			return []string{"<rejected>"}
		case "ps", "rm", "kill":
			return []string{args[0], "<validated>"}
		default:
			return []string{"<rejected>"}
		}
	}
	out := []string{"run"}
	for i := 1; i < len(args); {
		arg := args[i]
		if arg == image {
			out = append(out, image)
			out = append(out, traceProviderArgv(args[i+1:])...)
			break
		}
		out = append(out, arg)
		switch arg {
		case "--rm", "-i", "-t", "-it":
			i++
		case "-e":
			if i+1 >= len(args) {
				return append(out, "<missing>")
			}
			out = append(out, traceEnvArg(args[i+1]))
			i += 2
		case "-v":
			if i+1 >= len(args) {
				return append(out, "<missing>")
			}
			out = append(out, traceMountArg(root, args[i+1]))
			i += 2
		case "--env-file":
			if i+1 >= len(args) {
				return append(out, "<missing>")
			}
			out = append(out, traceRootPath(root, args[i+1]))
			i += 2
		case "-w":
			if i+1 >= len(args) {
				return append(out, "<missing>")
			}
			out = append(out, traceContainerPath(root, args[i+1]))
			i += 2
		case "--label":
			if i+1 >= len(args) {
				return append(out, "<missing>")
			}
			out = append(out, traceLabel(args[i+1]))
			i += 2
		case "--network":
			if i+1 >= len(args) {
				return append(out, "<missing>")
			}
			value := args[i+1]
			if value != "none" {
				value = traceValue(value)
			}
			out = append(out, value)
			i += 2
		case "--security-opt", "--cap-drop", "--pids-limit", "--memory", "--cpus":
			if i+1 >= len(args) {
				return append(out, "<missing>")
			}
			out = append(out, traceValue(args[i+1]))
			i += 2
		default:
			return []string{"<rejected>"}
		}
	}
	return out
}

func traceRunCommand(root string, run runCommand) runCommand {
	out := run
	out.Workdir = traceContainerPath(root, run.Workdir)
	out.HostWorkdir = traceRootPath(root, run.HostWorkdir)
	out.ProviderArgv = traceProviderArgv(run.ProviderArgv)
	out.Env = nil
	out.EnvFiles = make([]string, len(run.EnvFiles))
	for i, path := range run.EnvFiles {
		out.EnvFiles[i] = traceRootPath(root, path)
	}
	out.Mounts = make([]mount, len(run.Mounts))
	for i, m := range run.Mounts {
		out.Mounts[i] = m
		if !m.Named {
			out.Mounts[i].Source = traceRootPath(root, m.Source)
		}
		out.Mounts[i].Target = traceContainerPath(root, m.Target)
	}
	out.Labels = make([]string, len(run.Labels))
	for i, label := range run.Labels {
		out.Labels[i] = traceLabel(label)
	}
	if out.Network != "" && out.Network != "none" {
		out.Network = traceValue(out.Network)
	}
	return out
}

func traceProviderArgv(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		if i == 0 {
			if _, err := providerToken(arg); err == nil {
				out[i] = arg
			} else {
				out[i] = "<redacted-provider>"
			}
		} else if safeFlagToken(arg) {
			out[i] = arg
		} else {
			out[i] = traceValue(arg)
		}
	}
	return out
}

func safeFlagToken(value string) bool {
	if len(value) < 2 || len(value) > 64 || value[0] != '-' {
		return false
	}
	for _, r := range value[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func traceEnvArg(value string) string {
	key, resolved, ok := strings.Cut(value, "=")
	if !ok {
		if value == "TERM" {
			return value
		}
		return "<redacted-env>"
	}
	if traceableEnvValue(key) {
		return key + "=" + resolved
	}
	return key + "=<redacted>"
}

func traceLabel(value string) string {
	key, resolved, ok := strings.Cut(value, "=")
	if !ok {
		return "<redacted>"
	}
	if key == "coop" && resolved == "box" {
		return value
	}
	return key + "=" + traceValue(resolved)
}

func traceMountArg(root, value string) string {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "<redacted-mount>"
	}
	if filepath.IsAbs(parts[0]) {
		parts[0] = traceRootPath(root, parts[0])
	} else if parts[0] != "coop-cache" && parts[0] != "coop-asdf" {
		parts[0] = "<redacted-volume>"
	}
	parts[1] = traceContainerPath(root, parts[1])
	if len(parts) == 3 && parts[2] != "ro" {
		parts[2] = "<redacted-mode>"
	}
	return strings.Join(parts, ":")
}

func traceRootPath(root, path string) string {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "<outside-root>"
	}
	if rel == "." {
		return "<root>"
	}
	return "<root>/" + filepath.ToSlash(rel)
}

func traceContainerPath(root, path string) string {
	if traced := traceRootPath(root, path); traced != "<outside-root>" {
		return traced
	}
	return "<container>" + filepath.ToSlash(filepath.Clean(path))
}

func traceValue(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("<sha256:%x>", digest[:8])
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func commandExit(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, ""
	}
	signal := ""
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		signal = status.Signal().String()
	}
	return exitErr.ExitCode(), signal
}

type fixtureExitError struct {
	Code int
	Err  error
}

func (e *fixtureExitError) Error() string { return e.Err.Error() }
func (e *fixtureExitError) Unwrap() error { return e.Err }

func record(root, path string, entry traceRecord) error {
	entry.Version = traceVersion
	f, err := procharness.OpenRegularFile(root, path, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 8<<20 {
		return fmt.Errorf("unsafe trace file mode/size %s/%d", info.Mode(), info.Size())
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 128<<10)
	for scanner.Scan() {
		entry.Sequence++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	entry.Sequence++
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if len(encoded) > 64<<10 {
		return errors.New("trace record exceeds 64 KiB")
	}
	_, err = f.Write(append(encoded, '\n'))
	return err
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "providerfixture: "+format+"\n", args...)
	os.Exit(1)
}

func exitFixtureAlias(label string, err error) {
	var status *fixtureExitError
	if errors.As(err, &status) && status.Code >= 1 && status.Code <= 255 {
		fmt.Fprintf(os.Stderr, "providerfixture: %s: %v\n", label, err)
		os.Exit(status.Code)
	}
	fatalf("%s: %v", label, err)
}
