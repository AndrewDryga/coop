package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func newRenewalAuthority(t *testing.T) (*config.Config, accountAuthoritySpec) {
	t.Helper()
	cfg, spec := &config.Config{ConfigDir: t.TempDir()}, authorityTestSpec()
	if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
		return authorityTestRecord(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	return cfg, spec
}

func TestAccountRenewalPersistsIntentResponseAndShortGrant(t *testing.T) {
	cfg, spec := newRenewalAuthority(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	short := errors.New("inert short grant")
	record, effect, err := renewAccountAuthority(ctx, cfg, spec, "work", nil, func(current *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
		path := filepath.Join(filepath.Dir(authorityTestPath(cfg)), accountRenewalName)
		var intent accountRenewal
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &intent) != nil || intent.Revision != current.Revision || intent.Epoch != current.Epoch {
			t.Fatal("provider request admitted without durable intent")
		}
		if err := retain([]byte("inert issued response")); err != nil {
			t.Fatal(err)
		}
		data, err = os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &intent) != nil || string(intent.Received) != "inert issued response" {
			t.Fatal("received response was not durably retained")
		}
		cancel()
		current.Artifacts["auth.json"] = []byte(`{"principal":"inert-account","grant":"inert-new-grant"}`)
		return current, short
	})
	if !effect || !errors.Is(err, short) || record.Revision != 2 || record.Renewal == "" || !strings.Contains(string(record.Artifacts["auth.json"]), "inert-new-grant") {
		t.Fatalf("issued authority lost after cancellation/deadline error: effect=%v err=%v", effect, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(authorityTestPath(cfg)), accountRenewalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed rotation still has a pending intent")
	}
}

func TestAccountRenewalUncertainResponseCannotRetryOrMutate(t *testing.T) {
	for _, received := range []bool{false, true} {
		t.Run(map[bool]string{false: "connection-lost", true: "unusable-response"}[received], func(t *testing.T) {
			cfg, spec := newRenewalAuthority(t)
			_, effect, err := renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(*accountAuthority, func([]byte) error) (*accountAuthority, error) {
				return nil, errors.New("inert connection lost")
			})
			if received {
				// Model a response that was retained immediately before process death.
				root, err := openAccountAuthorityRoot(cfg, spec, "work")
				if err != nil {
					t.Fatal(err)
				}
				lock, err := root.lock(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				pending, err := root.readRenewal()
				if err != nil || pending == nil {
					t.Fatal("missing intent")
				}
				next := *pending
				next.Received = []byte("inert unusable rotated response")
				if err := root.writeRenewal(lock, pending, next); err != nil {
					t.Fatal(err)
				}
				_ = lock.Close()
				root.close()
			}
			if effect || !errors.Is(err, errAccountRenewalUncertain) {
				t.Fatalf("uncertainty lost: %v", err)
			}
			called := false
			_, effect, err = renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(*accountAuthority, func([]byte) error) (*accountAuthority, error) {
				called = true
				return authorityTestRecord(1), nil
			})
			if called || effect || !errors.Is(err, errAccountRenewalUncertain) {
				t.Fatal("uncertain rotation retried")
			}
			_, effect, err = transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
				called = true
				return authorityTestRecord(1), nil
			})
			if called || effect || !errors.Is(err, errAccountRenewalUncertain) {
				t.Fatal("pending recovery silently discarded")
			}
		})
	}
}

func TestAccountRenewalRecoversOnlyExactPublishedReceipt(t *testing.T) {
	for _, matches := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-receipt", true: "committed-before-crash"}[matches], func(t *testing.T) {
			cfg, spec := newRenewalAuthority(t)
			root, lock, current, err := lockAccountAuthority(context.Background(), cfg, spec, "work")
			if err != nil {
				t.Fatal(err)
			}
			pending := accountRenewal{Version: 1, ID: strings.Repeat("a", 32), Provider: current.Provider, Account: current.Account,
				Epoch: current.Epoch, Revision: current.Revision, Received: []byte("inert issued grant")}
			if err := root.writeRenewal(lock, nil, pending); err != nil {
				t.Fatal(err)
			}
			next := cloneAccountAuthority(current)
			next.Renewal = strings.Repeat("b", 32)
			if matches {
				next.Renewal = pending.ID
			}
			if _, effect, err := root.publish(lock, spec, "work", current, next, nil); err != nil || !effect {
				t.Fatal(err)
			}
			_ = lock.Close()
			root.close()
			called := false
			_, _, err = transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
				called = true
				return nil, nil
			})
			if matches && (err != nil || !called) {
				t.Fatalf("exact committed rotation did not recover: %v", err)
			}
			if !matches && (!errors.Is(err, errAccountRenewalUncertain) || called) {
				t.Fatal("foreign receipt discarded recovery")
			}
		})
	}
}

func TestAccountRenewalRequiresReceivedResponseCustody(t *testing.T) {
	cfg, spec := newRenewalAuthority(t)
	_, effect, err := renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(current *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
		if retain(nil) == nil || retain(make([]byte, (1<<20)+1)) == nil {
			t.Fatal("invalid custody payload accepted")
		}
		return current, nil
	})
	if effect || !errors.Is(err, errAccountRenewalUncertain) {
		t.Fatal("rotation published without custody")
	}
}

func TestAccountRenewalFreshSignInAndRemovalPreserveUncertainResponse(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "sign-in", true: "remove"}[revoked], func(t *testing.T) {
			cfg, spec := newRenewalAuthority(t)
			_, _, err := renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(_ *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
				if err := retain([]byte("inert uncertain issued response")); err != nil {
					t.Fatal(err)
				}
				return nil, errors.New("inert invalid response")
			})
			if !errors.Is(err, errAccountRenewalUncertain) {
				t.Fatal(err)
			}
			replacement := authorityTestRecord(1)
			if revoked {
				replacement = &accountAuthority{Revoked: true}
			}
			current, effect, err := replaceAccountAuthority(context.Background(), cfg, spec, "work", replacement)
			if err != nil || !effect || current.Epoch != 2 || current.Revision != 2 || current.Revoked != revoked {
				t.Fatalf("explicit lifecycle operation did not supersede pending grant: %v", err)
			}
			paths, err := filepath.Glob(filepath.Join(filepath.Dir(authorityTestPath(cfg)), "recovery-*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatal("uncertain response was not archived")
			}
			data, err := os.ReadFile(paths[0])
			var pending accountRenewal
			if err != nil || json.Unmarshal(data, &pending) != nil || string(pending.Received) != "inert uncertain issued response" {
				t.Fatal("archived response changed")
			}
			called := false
			_, _, err = transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
				called = true
				return nil, nil
			})
			if err != nil || !called {
				t.Fatalf("superseded recovery kept account blocked: %v", err)
			}
		})
	}
}
