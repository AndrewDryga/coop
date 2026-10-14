package acpctl

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// Host policy belongs to cli. These fixtures state its replies, not a second implementation.
func testHost() Host {
	return Host{
		AccountsFor: func(*config.Config, string) []string { return nil },
		ExpandLadder: func(*config.Config, string, []agents.Target) ([]agents.Target, error) {
			return nil, fmt.Errorf("no ladder fixture configured")
		},
		WriteModelsCache: func(*config.Config, string, []agents.Model) error { return nil },
	}
}

func withTestAccounts(h Host, accounts map[string][]string) Host {
	replies := make(map[string][]string, len(accounts))
	for provider, names := range accounts {
		replies[provider] = slices.Clone(names)
	}
	previous := h.AccountsFor
	h.AccountsFor = func(cfg *config.Config, provider string) []string {
		if names, ok := replies[provider]; ok {
			return slices.Clone(names)
		}
		return previous(cfg, provider)
	}
	return h
}

type testLadderReply struct {
	provider string
	rungs    []agents.Target
	targets  []agents.Target
	err      error
}

func withTestLadderReplies(t *testing.T, h Host, replies ...testLadderReply) Host {
	t.Helper()
	clone := func(targets []agents.Target) []agents.Target {
		out := slices.Clone(targets)
		for i := range out {
			out[i].Accounts = slices.Clone(out[i].Accounts)
		}
		return out
	}
	expected := slices.Clone(replies)
	for i := range expected {
		expected[i].rungs = clone(expected[i].rungs)
		expected[i].targets = clone(expected[i].targets)
	}
	h.ExpandLadder = func(_ *config.Config, provider string, rungs []agents.Target) ([]agents.Target, error) {
		for _, reply := range expected {
			if provider == reply.provider && reflect.DeepEqual(rungs, reply.rungs) {
				return clone(reply.targets), reply.err
			}
		}
		t.Errorf("unexpected ladder request: provider %q, rungs %+v", provider, rungs)
		return nil, fmt.Errorf("unexpected ladder request")
	}
	return h
}

func testHostCapturingModels(captured map[string][]agents.Model) Host {
	h := testHost()
	h.WriteModelsCache = func(_ *config.Config, provider string, models []agents.Model) error {
		captured[provider] = slices.Clone(models)
		return nil
	}
	return h
}
