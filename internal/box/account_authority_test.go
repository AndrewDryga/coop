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
	"golang.org/x/sys/unix"
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
	return root.publish(lock, spec, account, current, next, readinessErr)
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

func TestAccountAuthorityLockReplacementAndCancelledAdmission(t *testing.T) {
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, effect, err := transactAccountAuthority(ctx, cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { called = true; return authorityTestRecord(1), nil })
	if called || effect || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission: called=%v effect=%v err=%v", called, effect, err)
	}
	_, effect, err = transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
		path := filepath.Join(filepath.Dir(authorityTestPath(cfg)), ".authority.lock")
		if err := os.Rename(path, path+".retained"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return authorityTestRecord(1), nil
	})
	if err == nil || effect || !strings.Contains(err.Error(), "lock changed") {
		t.Fatalf("lock substitution: effect=%v err=%v", effect, err)
	}
	if _, err := os.Stat(authorityTestPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock substitution published authority")
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

func TestAccountAuthorityRefusesChangedPublicationInput(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
			if existing {
				if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { return authorityTestRecord(1), nil }); err != nil {
					t.Fatal(err)
				}
			}
			_, effect, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
				external := authorityTestRecord(1)
				external.Version, external.Provider, external.Account, external.Revision = 1, "codex", "work", 1
				external.Artifacts["auth.json"] = []byte(`{"principal":"inert-account","grant":"inert-external-change"}`)
				data, err := json.Marshal(external)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(authorityTestPath(cfg), data, 0o600); err != nil {
					t.Fatal(err)
				}
				return authorityTestRecord(1), nil
			})
			if err == nil || effect {
				t.Fatalf("external authority overwritten: effect=%v err=%v", effect, err)
			}
			data, err := os.ReadFile(authorityTestPath(cfg))
			if err != nil {
				t.Fatal(err)
			}
			var saved accountAuthority
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(saved.Artifacts["auth.json"]), "inert-external-change") {
				t.Fatal("external credential change lost")
			}
		})
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

func TestAccountAuthorityRefusesLinkedAndSpecialInputs(t *testing.T) {
	for _, kind := range []string{"provider-symlink", "lock-symlink", "lock-hardlink", "authority-hardlink", "authority-fifo", "extra-field", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
			if kind == "provider-symlink" {
				if err := os.Symlink(t.TempDir(), filepath.Join(cfg.ConfigDir, "codex")); err != nil {
					t.Fatal(err)
				}
			} else {
				root, err := openAccountAuthorityRoot(cfg, spec, "work")
				if err != nil {
					t.Fatal(err)
				}
				root.close()
				dir := filepath.Dir(authorityTestPath(cfg))
				lock := filepath.Join(dir, ".authority.lock")
				switch kind {
				case "lock-symlink":
					if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), lock); err != nil {
						t.Fatal(err)
					}
				case "lock-hardlink":
					if err := os.WriteFile(lock, nil, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(lock, filepath.Join(t.TempDir(), "alias")); err != nil {
						t.Fatal(err)
					}
				case "authority-hardlink":
					if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { return authorityTestRecord(1), nil }); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(authorityTestPath(cfg), filepath.Join(t.TempDir(), "alias")); err != nil {
						t.Fatal(err)
					}
				case "authority-fifo":
					if err := unix.Mkfifo(authorityTestPath(cfg), 0o600); err != nil {
						t.Fatal(err)
					}
				case "extra-field":
					if err := os.WriteFile(authorityTestPath(cfg), []byte(`{"version":1,"unexpected":true}`), 0o600); err != nil {
						t.Fatal(err)
					}
				case "oversized":
					if err := os.WriteFile(authorityTestPath(cfg), make([]byte, accountAuthorityLimit+1), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			called := false
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, effect, err := transactAccountAuthority(ctx, cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { called = true; return authorityTestRecord(1), nil })
			if called || effect || err == nil {
				t.Fatalf("unsafe input admitted: called=%v effect=%v err=%v", called, effect, err)
			}
		})
	}
}
