package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// An exchange that kept no answer leaves nothing to protect: the grant it sent is either unspent or
// lost either way. Its journal is cleared at once, and the next renewal simply tries again.
func TestAccountRenewalRetriesAfterAnExchangeThatKeptNothing(t *testing.T) {
	cfg, spec := newRenewalAuthority(t)
	lost := errors.New("inert connection lost")
	_, effect, err := renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(*accountAuthority, func([]byte) error) (*accountAuthority, error) {
		return nil, lost
	})
	if effect || !errors.Is(err, lost) || errors.Is(err, errAccountRenewalUncertain) {
		t.Fatalf("failed exchange = effect %v, %v; want the exchange's own error and no wedge", effect, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(authorityTestPath(cfg)), accountRenewalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a journal that kept nothing was left behind")
	}
	calls := 0
	record, effect, err := renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(current *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
		calls++
		if current.Revision != 1 || !strings.Contains(string(current.Artifacts["auth.json"]), "inert-canary") {
			t.Fatalf("retry started from %+v, want the unchanged account", current)
		}
		if err := retain([]byte(`{"grant":"inert-new-grant"}`)); err != nil {
			t.Fatal(err)
		}
		current.Artifacts["auth.json"] = []byte(`{"principal":"inert-account","grant":"inert-new-grant"}`)
		return current, nil
	})
	if err != nil || !effect || calls != 1 || record.Revision != 2 {
		t.Fatalf("retry = %+v, effect %v, %v after %d calls", record, effect, err, calls)
	}
}

// writeKeptAnswer leaves the journal a renewal leaves when it dies after keeping the provider's
// answer: the intent at the account's current revision, with the answer and when it arrived.
func writeKeptAnswer(t *testing.T, cfg *config.Config, spec accountAuthoritySpec, received string, at time.Time) accountRenewal {
	t.Helper()
	root, lock, current, err := lockAccountAuthority(context.Background(), cfg, spec, "work")
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	defer lock.Close()
	pending := accountRenewal{Version: 1, ID: strings.Repeat("c", 32), Provider: current.Provider, Account: current.Account,
		Epoch: current.Epoch, Revision: current.Revision, Received: []byte(received)}
	if !at.IsZero() {
		pending.ReceivedAt = at.UnixMilli()
	}
	if err := root.beginRenewal(pending); err != nil {
		t.Fatal(err)
	}
	return pending
}

// completingSpec completes a kept answer of the form {"grant":...} into the account's credential,
// recording when Coop says the answer arrived.
func completingSpec(t *testing.T, at *time.Time) accountAuthoritySpec {
	spec := authorityTestSpec()
	spec.Complete = func(files map[string][]byte, received []byte, arrived time.Time) (map[string][]byte, error) {
		var answer map[string]string
		if json.Unmarshal(received, &answer) != nil || answer["grant"] == "" {
			return nil, errors.New("inert unusable answer")
		}
		*at = arrived
		next := map[string][]byte{"auth.json": []byte(`{"principal":"inert-account","grant":"` + answer["grant"] + `"}`)}
		return next, nil
	}
	return spec
}

// A renewal that died after keeping the provider's answer completes at the next read, from the
// kept answer alone, dated from when it arrived; the next renewal starts from the recovered grant.
// A stale temporary file from the interrupted publication does not get in the way.
func TestAccountRenewalCompletesAKeptAnswerWithoutTheProvider(t *testing.T) {
	cfg, _ := newRenewalAuthority(t)
	var arrived time.Time
	spec := completingSpec(t, &arrived)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	pending := writeKeptAnswer(t, cfg, spec, `{"grant":"inert-kept-grant"}`, at)
	stale := filepath.Join(filepath.Dir(authorityTestPath(cfg)), ".authority-"+strings.Repeat("d", 32))
	if err := os.WriteFile(stale, []byte(`{"partial":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	record, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(current *accountAuthority) (*accountAuthority, error) {
		called = true
		if current.Revision != 2 || current.Renewal != pending.ID || !strings.Contains(string(current.Artifacts["auth.json"]), "inert-kept-grant") {
			t.Fatalf("reader saw %+v, want the completed renewal", current)
		}
		return nil, nil
	})
	if err != nil || !called || record.Revision != 2 || !arrived.Equal(at) {
		t.Fatalf("read after a kept answer = %+v, %v (called %v, dated %v, want %v)", record, err, called, arrived, at)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(authorityTestPath(cfg)), accountRenewalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed journal was left behind")
	}
	_, _, err = renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(current *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
		if !strings.Contains(string(current.Artifacts["auth.json"]), "inert-kept-grant") {
			t.Fatalf("next renewal started from %s, a grant the provider already spent", current.Artifacts["auth.json"])
		}
		return nil, errors.New("inert stop")
	})
	if errors.Is(err, errAccountRenewalUncertain) {
		t.Fatal(err)
	}
}

// An answer kept before arrival times were recorded dates its grant as long expired, so the next
// renewal spends the grant it carries rather than trusting its access token.
func TestAccountRenewalDatesAnUndatedKeptAnswerAsExpired(t *testing.T) {
	cfg, _ := newRenewalAuthority(t)
	var arrived time.Time
	spec := completingSpec(t, &arrived)
	writeKeptAnswer(t, cfg, spec, `{"grant":"inert-kept-grant"}`, time.Time{})
	if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if !arrived.Equal(time.Unix(0, 0)) {
		t.Fatalf("an undated answer was dated %v", arrived)
	}
}

// Two readers that both find the kept answer complete it once.
func TestAccountRenewalCompletesAKeptAnswerOnce(t *testing.T) {
	cfg, _ := newRenewalAuthority(t)
	var arrived time.Time
	spec := completingSpec(t, &arrived)
	writeKeptAnswer(t, cfg, spec, `{"grant":"inert-kept-grant"}`, time.Now())
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { return nil, nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	root, lock, current, err := lockAccountAuthority(context.Background(), cfg, spec, "work")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	root.close()
	if current.Revision != 2 {
		t.Fatalf("revision = %d after two readers, want one completion", current.Revision)
	}
}

// An earlier version kept every answer, error answers included, with no arrival time. Such an
// answer issued nothing, so it is cleared and the next renewal tries again instead of asking for a
// sign-in.
func TestAccountRenewalClearsAnErrorAnswerAnEarlierVersionKept(t *testing.T) {
	cfg, _ := newRenewalAuthority(t)
	var arrived time.Time
	spec := completingSpec(t, &arrived)
	writeKeptAnswer(t, cfg, spec, `{"error":"temporarily_unavailable"}`, time.Time{})
	called := false
	_, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
		called = true
		return nil, nil
	})
	if err != nil || !called || !arrived.IsZero() {
		t.Fatalf("read past an old kept error answer = %v (called %v)", err, called)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(authorityTestPath(cfg)), accountRenewalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an old error answer was kept")
	}
}

// A kept answer that cannot be used still refuses every read and renewal until a fresh host
// sign-in archives it: the grant it would replace is spent, and nothing else can recover it.
func TestAccountRenewalUnusableKeptAnswerRefuses(t *testing.T) {
	cfg, _ := newRenewalAuthority(t)
	var arrived time.Time
	spec := completingSpec(t, &arrived)
	writeKeptAnswer(t, cfg, spec, "inert unusable rotated response", time.Now())
	called := false
	_, effect, err := renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(*accountAuthority, func([]byte) error) (*accountAuthority, error) {
		called = true
		return nil, nil
	})
	if called || effect || !errors.Is(err, errAccountRenewalUncertain) {
		t.Fatalf("renewal past an unusable kept answer = called %v, effect %v, %v", called, effect, err)
	}
	_, _, err = transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) {
		called = true
		return nil, nil
	})
	if called || !errors.Is(err, errAccountRenewalUncertain) {
		t.Fatal("an unusable kept answer was discarded")
	}
}

// A journal a host sign-in superseded, but could not archive at the time, is archived at the next
// read instead of blocking the fresh account.
func TestAccountRenewalArchivesASupersededJournal(t *testing.T) {
	cfg, spec := newRenewalAuthority(t)
	writeKeptAnswer(t, cfg, spec, "inert superseded answer", time.Now())
	root, lock, current, err := lockAccountAuthorityState(context.Background(), cfg, spec, "work")
	if err != nil {
		t.Fatal(err)
	}
	next := authorityTestRecord(current.Epoch + 1)
	if _, _, err := root.publish(spec, "work", current, next, nil); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	root.close()
	if _, _, err := transactAccountAuthority(context.Background(), cfg, spec, "work", func(*accountAuthority) (*accountAuthority, error) { return nil, nil }); err != nil {
		t.Fatalf("a superseded journal blocked the fresh account: %v", err)
	}
	if archives, _ := filepath.Glob(filepath.Join(filepath.Dir(authorityTestPath(cfg)), "recovery-*.json")); len(archives) != 1 {
		t.Fatalf("superseded journal archives = %v", archives)
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
			if err := root.beginRenewal(pending); err != nil {
				t.Fatal(err)
			}
			next := cloneAccountAuthority(current)
			next.Renewal = strings.Repeat("b", 32)
			if matches {
				next.Renewal = pending.ID
			}
			if _, effect, err := root.publish(spec, "work", current, next, nil); err != nil || !effect {
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

// A grant that could not be kept is still published: it exists only in memory then, and dropping
// it would leave the account on a grant the provider already spent.
func TestAccountRenewalPublishesAGrantItCouldNotKeep(t *testing.T) {
	cfg, spec := newRenewalAuthority(t)
	record, effect, err := renewAccountAuthority(context.Background(), cfg, spec, "work", nil, func(current *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
		if retain(nil) == nil || retain(make([]byte, (1<<20)+1)) == nil {
			t.Fatal("invalid custody payload accepted")
		}
		current.Artifacts["auth.json"] = []byte(`{"principal":"inert-account","grant":"inert-unkept-grant"}`)
		return current, nil
	})
	if err != nil || !effect || record.Revision != 2 || !strings.Contains(string(record.Artifacts["auth.json"]), "inert-unkept-grant") {
		t.Fatalf("unkept grant = %+v, effect %v, %v", record, effect, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(authorityTestPath(cfg)), accountRenewalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published renewal left its journal")
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
