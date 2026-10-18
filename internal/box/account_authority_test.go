package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// transactAccountAuthority exercises the production locking and publication primitives.
func transactAccountAuthority(ctx context.Context, cfg *config.Config, spec accountAuthoritySpec, account string,
	change func(*accountAuthority) (*accountAuthority, error)) (record *accountAuthority, published bool, err error) {
	root, lock, current, err := lockAccountAuthority(ctx, cfg, spec, account)
	if err != nil {
		return nil, false, err
	}
	defer root.close()
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return current, false, err
	}
	next, readinessErr := change(cloneAccountAuthority(current))
	return root.publish(spec, account, current, next, readinessErr)
}

func authorityTestSpec() accountAuthoritySpec {
	return accountAuthoritySpec{Provider: "codex", Files: map[string]int64{"auth.json": 1024},
		Inspect: func(files map[string][]byte) (string, string, error) {
			var doc map[string]string
			if err := json.Unmarshal(files["auth.json"], &doc); err != nil || doc["principal"] == "" {
				return "", "", errors.New("bad fixture credential")
			}
			return "oauth", doc["principal"], nil
		}}
}

func authorityTestRecord(epoch uint64) *accountAuthority {
	return &accountAuthority{Epoch: epoch, Selection: "oauth", Principal: "inert-account", Artifacts: map[string][]byte{
		"auth.json": []byte(`{"principal":"inert-account","grant":"inert-canary"}`),
	}}
}

func authorityTestPath(cfg *config.Config) string {
	return filepath.Join(cfg.ConfigDir, "codex", "credentials", "work", "authority.json")
}

func TestAccountAuthorityAtomicConcurrencyAndReadinessError(t *testing.T) {
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	first, effect, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
		if current != nil {
			t.Fatal("new authority exists")
		}
		return authorityTestRecord(1), nil
	})
	if err != nil || !effect || first.Revision != 1 {
		t.Fatalf("initial publication: effect=%v err=%v", effect, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, effect, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
				current.Artifacts["auth.json"] = []byte(fmt.Sprintf(`{"principal":"inert-account","grant":"inert-revision-%d"}`, current.Revision+1))
				return current, errors.New("insufficient turn deadline")
			})
			if !effect || err == nil || !strings.Contains(err.Error(), "insufficient") {
				t.Errorf("rotation: effect=%v err=%v", effect, err)
			}
		}()
	}
	wg.Wait()
	root, err := openAccountAuthorityRoot(cfg, spec, "work")
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	last, err := root.read(spec, "work")
	if err != nil {
		t.Fatal(err)
	}
	if last.Revision != 9 || last.Epoch != 1 || !strings.Contains(string(last.Artifacts["auth.json"]), "inert-revision-9") {
		t.Fatalf("lost concurrent authority: revision=%d epoch=%d", last.Revision, last.Epoch)
	}
}

func TestAccountAuthorityGenerationCannotResurrectOrSwitchInPlace(t *testing.T) {
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { return authorityTestRecord(1), nil }); err != nil {
		t.Fatal(err)
	}
	_, effect, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
		current.Principal = "different-account"
		current.Artifacts["auth.json"] = []byte(`{"principal":"different-account"}`)
		return current, nil
	})
	if err == nil || effect {
		t.Fatal("same-epoch identity replacement was admitted")
	}
	_, effect, err = transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
		return &accountAuthority{Epoch: current.Epoch + 1, Revoked: true}, nil
	})
	if err != nil || !effect {
		t.Fatalf("revoke: %v", err)
	}
	_, effect, err = transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
		return authorityTestRecord(current.Epoch), nil
	})
	if err == nil || effect {
		t.Fatal("revoked generation resurrected")
	}
	recreated, effect, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
		return authorityTestRecord(current.Epoch + 1), nil
	})
	if err != nil || !effect || recreated.Epoch != 3 || recreated.Revision != 3 {
		t.Fatalf("recreate: effect=%v err=%v", effect, err)
	}
}

func TestAccountAuthorityCancelledAdmission(t *testing.T) {
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, effect, err := transactAccountAuthority(ctx, cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { called = true; return authorityTestRecord(1), nil })
	if called || effect || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission: called=%v effect=%v err=%v", called, effect, err)
	}
}

func TestAccountAuthorityCancellationKeepsReceivedRotation(t *testing.T) {
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, effect, err := transactAccountAuthority(ctx, cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
		cancel()
		return authorityTestRecord(1), context.Canceled
	})
	if !effect || !errors.Is(err, context.Canceled) {
		t.Fatalf("issued authority lost: effect=%v err=%v", effect, err)
	}
}

func TestAccountAuthorityCancellationDuringInspectionPreventsAdmission(t *testing.T) {
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
		return authorityTestRecord(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inspect := spec.Inspect
	spec.Inspect = func(files map[string][]byte) (string, string, error) {
		cancel()
		return inspect(files)
	}
	called := false
	_, effect, err := transactAccountAuthority(ctx, cfg, spec, "work", func(record *accountAuthority) (*accountAuthority, error) {
		called = true
		return record, nil
	})
	if called || effect || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled inspection admitted callback=%v effect=%v err=%v", called, effect, err)
	}
}

func TestAccountAuthorityCancelledBusyTransactionDoesNotRun(t *testing.T) {
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	root, err := openAccountAuthorityRoot(cfg, spec, "work")
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	lock, err := root.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	called := false
	_, effect, err := transactAccountAuthority(ctx, cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { called = true; return authorityTestRecord(1), nil })
	if called || effect || err == nil {
		t.Fatalf("busy transaction admitted: called=%v effect=%v err=%v", called, effect, err)
	}
}

// `coop credentials rm` deletes an account's native homes, custody lock files and all, so it waits
// for a launch working in one and keeps new ones out until it is done.
func TestAccountHomesRemovalExcludesLaunches(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	folder := HistoryAccountRoot(cfg, "codex", "work")
	launch, err := lockAccountHomes(context.Background(), folder, false)
	if err != nil {
		t.Fatal(err)
	}
	busy, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if release, err := LockAccountHomesForRemoval(busy, cfg, "codex", "work"); err == nil {
		release()
		t.Fatal("removal went ahead while a launch was working in a home")
	}
	launch()
	removal, err := LockAccountHomesForRemoval(context.Background(), cfg, "codex", "work")
	if err != nil {
		t.Fatal(err)
	}
	defer removal()
	busy, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if release, err := lockAccountHomes(busy, folder, false); err == nil {
		release()
		t.Fatal("a launch entered the homes while removal held them")
	}
}

// ConfigDir/<provider> was a box mount before 2026-07, so custody refuses a folder of its path that
// is a link instead of following it to where a box could read the credential.
func TestPrivateAccountTreeRefusesALinkedFolder(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg.ConfigDir, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.ConfigDir, "codex", "credentials")); err != nil {
		t.Fatal(err)
	}
	if root, err := openPrivateAccountTree(cfg.ConfigDir, "codex", "credentials", "work"); err == nil {
		root.close()
		t.Fatal("custody followed a linked folder")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("custody wrote through the link: %v, %v", entries, err)
	}
}
