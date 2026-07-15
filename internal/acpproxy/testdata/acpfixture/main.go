package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type plan struct {
	Providers map[string][][]step `json:"providers"`
}

type step struct {
	Method string            `json:"method"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  json.RawMessage   `json:"error,omitempty"`
	Events []json.RawMessage `json:"events,omitempty"`
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: acpfixture runtime-args | provider NAME")
	}
	if os.Args[1] == "provider" {
		if len(os.Args) != 3 {
			fatalf("provider name required")
		}
		if err := serveProvider(os.Args[2]); err != nil {
			fatalf("provider %s: %v", os.Args[2], err)
		}
		return
	}
	if err := serveRuntime(os.Args[1:]); err != nil {
		fatalf("runtime: %v", err)
	}
}

func serveRuntime(args []string) error {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("coop-acp-fixture 1")
		return nil
	}
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return nil
	}
	if len(args) >= 1 && args[0] == "ps" {
		return nil
	}
	if len(args) >= 1 && (args[0] == "kill" || args[0] == "rm") {
		return nil
	}
	if len(args) == 0 || args[0] != "run" {
		return fmt.Errorf("unsupported command %q", strings.Join(args, " "))
	}

	image := os.Getenv("COOP_IMAGE")
	imageAt := -1
	for i, arg := range args {
		if arg == image {
			imageAt = i
			break
		}
	}
	if imageAt < 0 || imageAt+1 >= len(args) {
		return fmt.Errorf("run command has no %q image/adapter tail: %q", image, strings.Join(args, " "))
	}
	env := append([]string(nil), os.Environ()...)
	for i := 1; i < imageAt; i++ {
		switch args[i] {
		case "-e":
			if i+1 < imageAt {
				i++
				if strings.Contains(args[i], "=") {
					env = setEnv(env, args[i])
				}
			}
		case "--env-file":
			if i+1 < imageAt {
				i++
				data, err := os.ReadFile(args[i])
				if err != nil {
					return err
				}
				for _, line := range strings.Split(string(data), "\n") {
					line = strings.TrimSpace(line)
					if line != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
						env = setEnv(env, line)
					}
				}
			}
		}
	}
	provider, err := adapterProvider(args[imageAt+1:])
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "provider", provider)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func adapterProvider(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("adapter command is empty")
	}
	switch filepath.Base(args[0]) {
	case "claude-agent-acp":
		return "claude", nil
	case "codex-acp":
		return "codex", nil
	case "gemini":
		return "gemini", nil
	case "grok":
		return "grok", nil
	default:
		return "", fmt.Errorf("unknown adapter command %q", strings.Join(args, " "))
	}
}

func serveProvider(provider string) error {
	data, err := os.ReadFile(os.Getenv("COOP_ACP_FIXTURE_PLAN"))
	if err != nil {
		return err
	}
	var p plan
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	gen, transcript, err := claimGeneration(provider)
	if err != nil {
		return err
	}
	defer transcript.Close()
	generations := p.Providers[provider]
	if gen >= len(generations) {
		return fmt.Errorf("generation %d has no script", gen)
	}
	steps := generations[gen]
	scanner := bufio.NewScanner(os.Stdin)
	buf := make([]byte, 64<<10)
	scanner.Buffer(buf, 1<<20)
	for i, expected := range steps {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			return fmt.Errorf("step %d expected %s, got EOF", i+1, expected.Method)
		}
		line := append([]byte(nil), scanner.Bytes()...)
		record(transcript, "in", line)
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			return fmt.Errorf("step %d invalid JSON: %w", i+1, err)
		}
		if req.Method != expected.Method {
			return fmt.Errorf("step %d method = %q, want %q", i+1, req.Method, expected.Method)
		}
		for _, event := range expected.Events {
			if err := emit(transcript, event); err != nil {
				return err
			}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if len(expected.Error) > 0 {
			response["error"] = expected.Error
		} else if len(expected.Result) > 0 {
			response["result"] = expected.Result
		} else {
			response["result"] = map[string]any{}
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		if err := emit(transcript, encoded); err != nil {
			return err
		}
	}
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		record(transcript, "in", line)
		return fmt.Errorf("unexpected frame after script: %s", line)
	}
	return scanner.Err()
}

func claimGeneration(provider string) (int, *os.File, error) {
	state := os.Getenv("COOP_ACP_FIXTURE_STATE")
	if state == "" {
		return 0, nil, errors.New("COOP_ACP_FIXTURE_STATE is empty")
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return 0, nil, err
	}
	for gen := 0; ; gen++ {
		dir := filepath.Join(state, fmt.Sprintf("%s-%d", provider, gen))
		if err := os.Mkdir(dir, 0o755); err == nil {
			f, err := os.Create(filepath.Join(dir, "wire.jsonl"))
			return gen, f, err
		} else if !os.IsExist(err) {
			return 0, nil, err
		}
	}
}

func emit(transcript io.Writer, raw []byte) error {
	record(transcript, "out", raw)
	_, err := os.Stdout.Write(append(append([]byte(nil), raw...), '\n'))
	return err
}

func record(w io.Writer, direction string, raw []byte) {
	entry, _ := json.Marshal(map[string]any{"direction": direction, "frame": json.RawMessage(raw)})
	fmt.Fprintf(w, "%s\n", entry)
}

func setEnv(env []string, item string) []string {
	key, _, _ := strings.Cut(item, "=")
	prefix := key + "="
	for i := range env {
		if strings.HasPrefix(env[i], prefix) {
			env[i] = item
			return env
		}
	}
	return append(env, item)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "acpfixture: "+format+"\n", args...)
	os.Exit(1)
}
