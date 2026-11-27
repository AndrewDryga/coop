package box

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestAccountMoveOffline(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "gemini", "grok"} {
		t.Run(provider, func(t *testing.T) {
			ctx := t.Context()
			from := &config.Config{ConfigDir: t.TempDir()}
			to := &config.Config{ConfigDir: t.TempDir()}
			seedCanonicalFixture(t, from, provider, "work")
			receiver, err := NewAccountReceiver(ctx, to, provider, "imported")
			if err != nil {
				t.Fatal(err)
			}
			other, err := NewAccountReceiver(ctx, to, provider, "other")
			if err != nil {
				t.Fatal(err)
			}
			ag, _ := agents.Get(provider)
			before, _, err := readNativeAccount(ctx, from, ag, "work")
			if err != nil {
				t.Fatal(err)
			}
			state, err := ag.NativeCredentials().Inspect(before.Artifacts, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if state.Refreshable {
				_, _, err = ExportAccount(ctx, from, provider, "work", receiver.Code, func(string) error { return errors.New("declined") })
				if err == nil {
					t.Fatal("transferred after declined confirmation")
				}
			}
			sealed, _, err := ExportAccount(ctx, from, provider, "work", receiver.Code, func(string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(sealed, "CANARY") {
				t.Fatal("export disclosed plaintext")
			}
			after, _, err := readNativeAccount(ctx, from, ag, "work")
			if err != nil || after.Revoked != state.Refreshable {
				t.Fatalf("incorrect source state after export, error %v", err)
			}
			if state.Refreshable {
				if len(after.Artifacts) != 0 {
					t.Fatal("source retained serving credentials")
				}
				retry, _, err := ExportAccount(ctx, from, provider, "work", receiver.Code, nil)
				if retry != sealed || err != nil {
					t.Fatalf("could not recover encrypted receipt: %v", err)
				}
				if _, _, err := ExportAccount(ctx, from, provider, "work", other.Code, nil); err == nil {
					t.Fatal("re-targeted completed move")
				}
			}
			if err := other.Import(ctx, to, provider, "other", sealed); err == nil {
				t.Fatal("imported on wrong receiver")
			}
			if err := receiver.Import(ctx, to, provider, "imported", sealed+"!"); err == nil {
				t.Fatal("accepted damaged bundle")
			}
			reopened, err := NewAccountReceiver(ctx, to, provider, "imported")
			if err != nil || reopened.Code != receiver.Code {
				t.Fatalf("lost receive key at restart: %v", err)
			}
			if err := reopened.Import(ctx, to, provider, "imported", sealed); err != nil {
				t.Fatal(err)
			}
			got, _, err := readNativeAccount(ctx, to, ag, "imported")
			if err != nil || got.Principal != before.Principal || got.Selection != before.Selection {
				t.Fatalf("wrong restored account: %v", err)
			}
			importedState, err := ag.NativeCredentials().Inspect(got.Artifacts, time.Now())
			if err != nil || importedState.Refreshable != state.Refreshable {
				t.Fatal("renewal ownership did not move")
			}
			if err := receiver.Import(ctx, to, provider, "imported", sealed); err != nil {
				t.Fatal("could not confirm a completed import", err)
			}
		})
	}
}

func TestAccountTransferCopiesAPIKey(t *testing.T) {
	ctx := t.Context()
	source := &config.Config{ConfigDir: t.TempDir()}
	dst := &config.Config{ConfigDir: t.TempDir()}
	importCanonicalFixture(t, source, "codex", "work", map[string][]byte{"auth.json": []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-account-key"}`)})
	r, err := NewAccountReceiver(ctx, dst, "codex", "ci")
	if err != nil {
		t.Fatal(err)
	}
	sealed, _, err := ExportAccount(ctx, source, "codex", "work", r.Code, func(string) error { t.Fatal("asked to move an API key"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Import(ctx, dst, "codex", "ci", sealed); err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get("codex")
	for _, pair := range []struct {
		c    *config.Config
		name string
	}{{source, "work"}, {dst, "ci"}} {
		if ready, _ := nativeAccountReady(pair.c, ag, pair.name, time.Now()); !ready {
			t.Fatal("missing usable API key")
		}
	}
}

func TestAccountTransferConfiguredClaudeKey(t *testing.T) {
	ctx := t.Context()
	source := &config.Config{ConfigDir: t.TempDir()}
	to := &config.Config{ConfigDir: t.TempDir()}
	const key = "configured-synthetic-claude-key"
	if err := os.WriteFile(source.EnvFile(), []byte("ANTHROPIC_API_KEY="+key+"\nUNRELATED_PRIVATE_KEY=unrelated-canary\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := NewAccountReceiver(ctx, to, "claude", "build")
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := ExportAccount(ctx, source, "claude", source.DefaultProfileOf("claude"), r.Code, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.open("claude", "build", bundle)
	if err != nil || strings.Contains(string(raw), "unrelated-canary") {
		t.Fatalf("export included unrelated config or failed: %v", err)
	}
	if err := r.Import(ctx, to, "claude", "build", bundle); err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get("claude")
	selected, ok, err := previewNativeAuthority(ctx, to, ag, "build")
	if err != nil || !ok || selected.state.AccessToken != key || !selected.state.APIKey {
		t.Fatalf("normal launch does not select imported key: %v", err)
	}
	busy, err := NewAccountReceiver(ctx, source, "claude", source.DefaultProfileOf("claude"))
	if err != nil {
		t.Fatal(err)
	}
	extra, _, err := ExportAccount(ctx, to, "claude", "build", busy.Code, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(busy.Import(ctx, source, "claude", source.DefaultProfileOf("claude"), extra), ErrImportOccupied) {
		t.Fatal("overwrote a configured key")
	}
}

func TestAccountMoveRevalidatesAfterConfirmation(t *testing.T) {
	src := &config.Config{ConfigDir: t.TempDir()}
	dst := &config.Config{ConfigDir: t.TempDir()}
	seedCanonicalFixture(t, src, "codex", "work")
	r, err := NewAccountReceiver(t.Context(), dst, "codex", "work")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ExportAccount(t.Context(), src, "codex", "work", r.Code, func(string) error {
		importCanonicalFixture(t, src, "codex", "work", canonicalFixtureFiles(t, "codex", "changed"))
		return nil
	})
	if err == nil {
		t.Fatal("moved a different login after confirmation")
	}
	ag, _ := agents.Get("codex")
	current, _, err := readNativeAccount(t.Context(), src, ag, "work")
	if err != nil || current.Revoked {
		t.Fatal("invalidated replacement")
	}
}

func TestTwoOfflineMovesCanUseSameReceiveCode(t *testing.T) {
	ctx := t.Context()
	dst := &config.Config{ConfigDir: t.TempDir()}
	r, err := NewAccountReceiver(ctx, dst, "codex", "work")
	if err != nil {
		t.Fatal(err)
	}
	var moves []string
	for _, user := range []string{"one", "two"} {
		src := &config.Config{ConfigDir: t.TempDir()}
		seedCanonicalFixture(t, src, "codex", user)
		sealed, _, err := ExportAccount(ctx, src, "codex", user, r.Code, func(string) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		moves = append(moves, sealed)
	}
	if err = r.Import(ctx, dst, "codex", "work", moves[0]); err != nil {
		t.Fatal(err)
	}
	if err = r.Import(ctx, dst, "codex", "work", moves[1]); !errors.Is(err, ErrImportOccupied) {
		t.Fatal("failed to protect existing sign-in")
	}
	again, err := NewAccountReceiver(ctx, dst, "codex", "work")
	if err != nil || again.Code != r.Code {
		t.Fatal("lost offline recipient")
	}
	if err = again.ImportAs(ctx, dst, "codex", "work", "spare", moves[1]); err != nil {
		t.Fatal(err)
	}
	if err = again.ImportAs(ctx, dst, "codex", "work", "duplicate", moves[1]); err == nil {
		t.Fatal("installed same move twice")
	}
	if err = RemoveNativeAccount(ctx, dst, "codex", "spare"); err != nil {
		t.Fatal(err)
	}
	if err = again.ImportAs(ctx, dst, "codex", "work", "spare", moves[1]); err == nil {
		t.Fatal("replayed revoked account")
	}
}

func TestInterruptedReservationCanRecoverUnderAnotherName(t *testing.T) {
	ctx := t.Context()
	src := &config.Config{ConfigDir: t.TempDir()}
	dst := &config.Config{ConfigDir: t.TempDir()}
	seedCanonicalFixture(t, src, "codex", "work")
	r, err := NewAccountReceiver(ctx, dst, "codex", "work")
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := ExportAccount(ctx, src, "codex", "work", r.Code, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	plain, err := r.open("codex", "work", bundle)
	if err != nil {
		t.Fatal(err)
	}
	var payload transferAccount
	if err = json.Unmarshal(plain, &payload); err != nil {
		t.Fatal(err)
	}
	// Simulate power loss between the durable reservation and the first
	// credential write, followed by a different sign-in at the intended name.
	if err = os.WriteFile(filepath.Join(r.mailbox, "delivery-"+payload.ID), []byte(`{"account":"work"}`), 0600); err != nil {
		t.Fatal(err)
	}
	seedCanonicalFixture(t, dst, "codex", "work")
	if err = r.Import(ctx, dst, "codex", "work", bundle); !errors.Is(err, ErrImportOccupied) {
		t.Fatal("overwrote another login")
	}
	if err = r.ImportAs(ctx, dst, "codex", "work", "spare", bundle); err != nil {
		t.Fatal(err)
	}
	importCanonicalFixture(t, dst, "codex", "spare", canonicalFixtureFiles(t, "codex", "new-owner"))
	if err = r.ImportAs(ctx, dst, "codex", "work", "again", bundle); err == nil {
		t.Fatal("a replaced account lost its spent receipt")
	}
}
