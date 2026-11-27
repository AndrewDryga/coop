package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/safefile"
	"os"
	"slices"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// Files holds only the selected adapter's portable artifacts. APIKey is used
// solely for Ryker's provider-key interface. Neither representation leaves the
// source before encryption.
type transferAccount struct {
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
	Account  string            `json:"account"`
	Files    map[string][]byte `json:"files,omitempty"`
	APIKey   string            `json:"api_key,omitempty"`
}

type sealedAccountMove struct {
	Receiver string `json:"receiver"`
	Bundle   string `json:"bundle"`
}

func (m *sealedAccountMove) valid() bool {
	return len(m.Receiver) == 43 && len(m.Bundle) > 0 && len(m.Bundle) <= AccountBundleLimit
}

func ExportAccount(ctx context.Context, cfg *config.Config, provider, account, invitation string, confirm func(string) error) (string, string, error) {
	inv, public, err := parseTransferInvitation(invitation, provider)
	if err != nil {
		return "", "", err
	}
	ag, ok := agents.Get(provider)
	if !ok || !accountNameValid(account) {
		return "", "", errTransfer
	}
	if inv.Destination == "ryker" && ag.NativeCredentials().ControllerImport == nil {
		return "", "", errors.New("choose a Codex or Claude account for Ryker")
	}
	cfg = cfg.NativeAuthorityConfig()
	before, exists, err := readNativeAccount(ctx, cfg, ag, account)
	if err != nil {
		return "", "", err
	}
	if !exists {
		if err := refuseLegacyLogin(cfg, ag, account); err != nil {
			return "", "", err
		}
		state, found, err := nativeEnvironment(cfg, ag, account, agents.NativeCredentialState{})
		if err != nil {
			return "", "", err
		}
		if !found || !state.APIKey {
			return "", "", errors.New("this account has no saved sign-in")
		}
	}
	target := fmt.Sprintf("%s %s@%q", inv.Destination, provider, inv.Account)
	var confirmedEpoch uint64
	native := ag.NativeCredentials()
	if exists && !before.Revoked {
		state, err := native.Inspect(before.Artifacts, time.Now())
		if err != nil {
			return "", "", err
		}
		if state.Refreshable {
			if err := transferAllowed(inv.Destination, native, state, before.Artifacts); err != nil {
				return "", "", err
			}
			if confirm == nil {
				return "", "", errors.New("account move requires confirmation")
			}
			if err := confirm(target); err != nil {
				return "", "", err
			}
			confirmedEpoch = before.Epoch
		}
	}
	root, lock, current, err := lockAccountAuthority(ctx, cfg, nativeAccountSpec(ag), account)
	if err != nil {
		return "", "", err
	}
	defer root.close()
	defer lock.Close()
	if current != nil && current.Revoked {
		if current.Transfer != nil && current.Transfer.Receiver == accountTransferHeader(invitation).KeyID {
			if err := root.dir().Sync(); err != nil {
				return "", "", err
			}
			return current.Transfer.Bundle, "Previously exported to the same receiver", nil
		}
		return "", "", errors.New("this account has already been removed or moved")
	}
	var state agents.NativeCredentialState
	var files map[string][]byte
	if current != nil {
		if refusedIn(root.path, current) {
			return "", "", errors.New("the provider declined this sign-in")
		}
		files = current.Artifacts
		state, err = native.Inspect(files, time.Now())
		if err != nil {
			return "", "", err
		}
	}
	selected, configured, err := nativeEnvironment(cfg, ag, account, state)
	if err != nil {
		return "", "", err
	}
	if configured {
		state = selected
		if inv.Destination == "coop" {
			if native.APIKeyFiles == nil {
				return "", "", errors.New("provider has no portable API key format")
			}
			files, err = native.APIKeyFiles(state.AccessToken)
			if err != nil {
				return "", "", err
			}
		}
	}
	if !state.Ready || (state.Refreshable && current.Epoch != confirmedEpoch) {
		return "", "", errors.New("the source account changed; start export again")
	}
	if err := transferAllowed(inv.Destination, native, state, files); err != nil {
		return "", "", err
	}
	payload := transferAccount{ID: randid.Hex(16), Provider: provider, Account: inv.Account}
	summary := "API-key access"
	if inv.Destination == "ryker" && state.APIKey {
		payload.APIKey = state.AccessToken
	} else {
		payload.Files = files
		if !state.APIKey {
			if state.Refreshable {
				summary = "Subscription login moved"
			} else {
				if !state.ExpiresAt.After(time.Now()) {
					return "", "", errors.New("this sign-in has expired")
				}
				summary = "Access ending " + state.ExpiresAt.UTC().Format(time.RFC3339)
			}
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	sealed, err := sealTransfer(invitation, public, raw)
	if err != nil {
		return "", "", err
	}
	if state.Refreshable {
		moved := &accountAuthority{Epoch: current.Epoch + 1, Revoked: true, Transfer: &sealedAccountMove{accountTransferHeader(invitation).KeyID, sealed}}
		if _, _, err := root.publish(nativeAccountSpec(ag), account, current, moved); err != nil {
			return "", "", err
		}
		if err := root.dir().Sync(); err != nil {
			return "", "", err
		}
	}
	return sealed, fmt.Sprintf("%s for %s", summary, target), nil
}

// A renewable account leaves the source only if the destination accepts
// its provider, identity and token sizes. Coop's native adapters accept a
// wider range than Ryker's encrypted model-account document.
func transferAllowed(destination string, native agents.NativeCredentialSpec, state agents.NativeCredentialState, files map[string][]byte) error {
	if destination == "ryker" {
		return native.ControllerImport(state, files)
	}
	return nil
}

// Record the selected name before publication. The account write then records
// the delivery ID atomically with the credential. On retry, an unfulfilled
// reservation can move to a free name while an applied delivery stays spent.
type accountDelivery struct {
	Account string `json:"account"`
}

func (r *AccountReceiver) Import(ctx context.Context, cfg *config.Config, provider, account, bundle string) error {
	return r.ImportAs(ctx, cfg, provider, account, account, bundle)
}

func (r *AccountReceiver) ImportAs(ctx context.Context, cfg *config.Config, provider, account, alias, bundle string) error {
	if !accountNameValid(alias) {
		return errTransfer
	}
	raw, err := r.open(provider, account, bundle)
	if err != nil {
		return err
	}
	var payload transferAccount
	ag, ok := agents.Get(provider)
	if !ok || json.Unmarshal(raw, &payload) != nil || !transferIDValid(payload.ID) || payload.Provider != provider || payload.Account != account || len(payload.Files) == 0 || payload.APIKey != "" {
		return errTransfer
	}
	native := ag.NativeCredentials()
	for name := range payload.Files {
		declared := false
		for _, artifact := range native.Artifacts {
			if artifact.Name == name && int64(len(payload.Files[name])) <= artifact.Limit {
				declared = true
				break
			}
		}
		if !declared {
			return errTransfer
		}
	}
	record, err := normalizeNativeAccount(ag, payload.Files)
	if err != nil {
		return errors.New("invalid account data")
	}
	state, err := native.Inspect(record.Artifacts, time.Now())
	if err != nil || (!state.Refreshable && !state.APIKey && !state.ExpiresAt.After(time.Now())) {
		return errors.New("this bundle has expired")
	}
	inbox, err := openPrivateAccountTree(cfg.NativeAuthorityConfig().ConfigDir, "account-imports", provider, account)
	if err != nil {
		return err
	}
	defer inbox.close()
	if inbox.path != r.mailbox {
		return errTransfer
	}
	inboxLock, err := inbox.lock(ctx)
	if err != nil {
		return err
	}
	defer inboxLock.Close()
	spec := nativeAccountSpec(ag)
	name := "delivery-" + payload.ID
	var reservation accountDelivery
	data, err := safefile.ReadRegular(inbox.dir(), name, 4096)
	if err == nil {
		if json.Unmarshal(data, &reservation) != nil || !accountNameValid(reservation.Account) {
			return errTransfer
		}
		if reservation.Account != alias {
			prior, _, err := readNativeAccount(ctx, cfg, ag, reservation.Account)
			if err != nil {
				return err
			}
			if accountReceived(prior, payload.ID) {
				return errors.New("this delivery was already imported")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, lock, current, err := lockAccountAuthority(ctx, cfg.NativeAuthorityConfig(), spec, alias)
	if err != nil {
		return err
	}
	defer root.close()
	defer lock.Close()
	var epoch uint64
	if current != nil {
		epoch = current.Epoch
	}
	if accountReceived(current, payload.ID) {
		if current.Imported == payload.ID && !current.Revoked {
			return root.dir().Sync()
		}
		return errors.New("this delivery was already imported")
	}
	if current != nil && !current.Revoked {
		return ErrImportOccupied
	}
	if current == nil {
		if _, found, err := nativeEnvironment(cfg.NativeAuthorityConfig(), ag, alias, agents.NativeCredentialState{}); err != nil || found {
			return ErrImportOccupied
		}
	}
	if reservation.Account != alias {
		encoded, err := json.Marshal(accountDelivery{Account: alias})
		if err != nil {
			return err
		}
		if err = writeAccountPrivate(inbox, name, encoded); err != nil {
			return err
		}
	}
	if err = inbox.dir().Sync(); err != nil {
		return err
	}
	record.Imported = payload.ID
	record.Epoch = epoch + 1
	if _, _, err = root.publish(spec, alias, current, record); err != nil {
		return err
	}
	if err = root.dir().Sync(); err != nil {
		return fmt.Errorf("account was installed; could not confirm durable storage: %w", err)
	}
	return nil
}

var ErrImportOccupied = errors.New("this name is in use; choose another local name for the incoming account")

func transferIDValid(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func accountReceived(current *accountAuthority, id string) bool {
	return current != nil && (current.Imported == id || slices.Contains(current.PriorImports, id))
}
