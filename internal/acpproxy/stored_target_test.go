package acpproxy_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestStoredTargetReadsOnlyTheSelectedACPRepositoryHome(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo, foreign := t.TempDir(), t.TempDir()
	const session, marker = "selected-session", "UNIQUE_MARKER"
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home := func(provider, selected string) string {
		t.Helper()
		canonical, err := filepath.EvalSymlinks(selected)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(canonical))
		return filepath.Join(cfg.ConfigDir, provider, "acp-homes", fmt.Sprintf("%s-%x", filepath.Base(canonical), digest[:8]), "home")
	}
	codexBody := func(model string) string {
		return fmt.Sprintf("{\"type\":\"turn_context\",\"payload\":{\"model\":%q,\"effort\":\"xhigh\"}}\n{\"text\":%q}\n", model, marker+" "+marker)
	}
	claudeBody := func(model string) string {
		return fmt.Sprintf("{\"message\":{\"model\":%q,\"text\":%q}}\n", model, marker+" "+marker)
	}
	for _, provider := range []string{"claude", "codex"} {
		subdir, body, file := "sessions", codexBody("wrong-model"), "rollout-"+session+".jsonl"
		if provider == "claude" {
			subdir, body, file = "projects", claudeBody("wrong-model"), session+".jsonl"
		}
		for _, decoy := range []string{
			filepath.Join(cfg.ConfigDir, provider, "acp-sessions"),
			home(provider, foreign),
			filepath.Join(cfg.ConfigDir, provider, "native-homes", "personal", "other", "home"),
		} {
			write(filepath.Join(decoy, subdir, file), body)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if provider, model, _, err := waitForFrontierStoredTarget(ctx, cfg, repo, session, marker); !errors.Is(err, context.DeadlineExceeded) || provider != "" || model != "" {
		t.Fatalf("retired/foreign/account history leaked into selected ACP home: %q %q %v", provider, model, err)
	}
	codexRoot := filepath.Join(home("codex", repo), "sessions")
	write(filepath.Join(home("claude", repo), "projects", session+".jsonl"), fmt.Sprintf("{\"message\":{\"model\":\"failed-claude\",\"text\":%q}}\n", marker))
	write(filepath.Join(codexRoot, "rollout-"+session+".jsonl"), codexBody("selected-codex"))
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	model, effort, err := waitForCodexRolloutTarget(ctx, codexRoot, session)
	if err != nil || model != "selected-codex" || effort != "xhigh" {
		t.Fatalf("selected Codex rollout = %q %q %v", model, effort, err)
	}
	provider, model, effort, err := waitForFrontierStoredTarget(ctx, cfg, repo, session, marker)
	if err != nil || provider != "codex" || model != "selected-codex" || effort != "xhigh" {
		t.Fatalf("selected preset Codex target = %q %q %q %v", provider, model, effort, err)
	}
	write(filepath.Join(home("claude", repo), "projects", session+".jsonl"), claudeBody("<synthetic>")+claudeBody("selected-claude"))
	provider, model, effort, err = waitForFrontierStoredTarget(ctx, cfg, repo, session, marker)
	if err != nil || provider != "claude" || model != "selected-claude" || effort != "" {
		t.Fatalf("selected preset Claude target = %q %q %q %v", provider, model, effort, err)
	}
}

func waitForCodexRolloutTarget(ctx context.Context, root, sessionID string) (string, string, error) {
	var lastErr error
	for {
		model, effort, walkErr := codexStoredTarget(root, sessionID)
		if model != "" || effort != "" {
			return model, effort, nil
		}
		if walkErr != nil {
			lastErr = walkErr
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", "", fmt.Errorf("%w (last rollout read: %v)", ctx.Err(), lastErr)
			}
			return "", "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func waitForFrontierStoredTarget(ctx context.Context, cfg *config.Config, repo, editorSessionID, marker string) (string, string, string, error) {
	claudeHome, err := box.NativeHomePath(cfg, "claude", "", repo, true)
	if err != nil {
		return "", "", "", err
	}
	codexHome, err := box.NativeHomePath(cfg, "codex", "", repo, true)
	if err != nil {
		return "", "", "", err
	}
	var lastErr error
	for {
		model, claudeErr := claudeStoredModel(filepath.Join(claudeHome, "projects"), editorSessionID, marker)
		if model != "" {
			return "claude", model, "", nil
		}
		model, effort, codexErr := codexStoredTargetByMarker(filepath.Join(codexHome, "sessions"), marker)
		if model != "" {
			return "codex", model, effort, nil
		}
		if claudeErr != nil {
			lastErr = claudeErr
		}
		if codexErr != nil {
			lastErr = codexErr
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", "", "", fmt.Errorf("%w (last provider storage read: %v)", ctx.Err(), lastErr)
			}
			return "", "", "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func claudeStoredModel(root, sessionID, marker string) (string, error) {
	want := sessionID + ".jsonl"
	model := ""
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || info.Name() != want {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The prompt text is present on every attempted rung; the exact reply is present only in the
		// provider that completed the turn. Requiring both occurrences avoids reporting a failed rung.
		if strings.Count(string(data), marker) < 2 {
			return nil
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		for {
			var record struct {
				Message struct {
					Model string `json:"model"`
				} `json:"message"`
			}
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
			if record.Message.Model != "" && record.Message.Model != "<synthetic>" {
				model = record.Message.Model
				return nil
			}
		}
		return nil
	})
	return model, err
}

func codexStoredTarget(root, sessionID string) (string, string, error) {
	return codexStoredTargetMatching(root, func(info os.FileInfo, _ []byte) bool {
		return strings.Contains(info.Name(), sessionID)
	})
}

func codexStoredTargetByMarker(root, marker string) (string, string, error) {
	return codexStoredTargetMatching(root, func(_ os.FileInfo, data []byte) bool {
		return strings.Count(string(data), marker) >= 2
	})
}

func codexStoredTargetMatching(root string, matches func(os.FileInfo, []byte) bool) (string, string, error) {
	model, effort := "", ""
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".jsonl") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !matches(info, data) {
			return nil
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		for {
			var record struct {
				Type    string `json:"type"`
				Payload struct {
					Model  string `json:"model"`
					Effort string `json:"effort"`
				} `json:"payload"`
			}
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
			if record.Type == "turn_context" {
				model, effort = record.Payload.Model, record.Payload.Effort
			}
		}
		return nil
	})
	return model, effort, err
}
