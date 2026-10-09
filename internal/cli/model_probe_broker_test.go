package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

const grokCatalogInitialize = `{"jsonrpc":"2.0","id":1,"result":{"authMethods":[{"id":"cached_token"},{"id":"grok.com","_meta":{"external_provider":true}}]}}`

func TestACPModelBrokerBootstrapSequence(t *testing.T) {
	ag, _ := agents.Get("grok")
	child, methods, replies, done := controlledModelChild(t)
	finished := make(chan error, 1)
	go func() {
		result, err := acpModelHandshake(t.Context(), child, "/repo", ag.ModelCatalog())
		if err == nil && len(ag.ModelCatalog().ParseACP(result)) != 1 {
			err = errors.New("broker catalog was not parsed")
		}
		finished <- err
	}()
	if req := requireModelPhase(t, methods, "initialize"); req.ID != 1 {
		t.Fatal("initialize ID", req.ID)
	}
	replies <- json.RawMessage(grokCatalogInitialize)
	auth := requireModelPhase(t, methods, "authenticate")
	if auth.ID != 2 || !reflect.DeepEqual(auth.Params, map[string]any{"methodId": "grok.com"}) {
		t.Fatal("bootstrap copied unqualified parameters", auth)
	}
	replies <- json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{}}`)
	if req := requireModelPhase(t, methods, "session/new"); req.ID != 3 || req.Params["cwd"] != "/repo" {
		t.Fatal("session request", req)
	}
	replies <- json.RawMessage(`{"jsonrpc":"2.0","id":3,"result":{"sessionId":"native","models":{"availableModels":[{"modelId":"grok-live"}]}}}`)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	child.Stop()
	<-done
	if len(methods) != 0 {
		t.Fatal("catalog sent a prompt or another authentication request")
	}
}

func TestACPModelBrokerBootstrapRefusalPreservesCache(t *testing.T) {
	ag, _ := agents.Get("grok")
	for _, missing := range []bool{false, true} {
		child, methods, replies, done := controlledModelChild(t)
		finished := make(chan error, 1)
		go func() {
			_, err := acpModelHandshake(t.Context(), child, "/repo", ag.ModelCatalog())
			finished <- err
		}()
		requireModelPhase(t, methods, "initialize")
		if missing {
			replies <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"authMethods":[{"id":"grok.com","name":"SECRET-CANARY"}]}}`)
		} else {
			replies <- json.RawMessage(grokCatalogInitialize)
			requireModelPhase(t, methods, "authenticate")
			replies <- json.RawMessage(`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"SECRET-CANARY"}}`)
		}
		err := <-finished
		child.Stop()
		<-done
		if err == nil || strings.Contains(err.Error(), "SECRET-CANARY") || len(methods) != 0 {
			t.Fatal("bootstrap failure continued or exposed provider data", err)
		}
		a := modelsApp(t)
		if err := writeModelsCache(a.cfg, "grok", []agents.Model{{ID: "last-good"}}); err != nil {
			t.Fatal(err)
		}
		a.acpModels = func(string) ([]agents.Model, error) { return nil, err }
		_, failed := a.refreshCatalog("grok", nil)
		cached, _ := loadModelsCache(a.cfg, "grok")
		if !failed || cached.Models[0].ID != "last-good" || strings.Contains(cached.AttemptError, "SECRET-CANARY") {
			t.Fatal("bootstrap failure lost safe fallback")
		}
	}
}

func TestACPModelBrokerAuthenticationSharesStartupDeadline(t *testing.T) {
	ag, _ := agents.Get("grok")
	child, methods, replies, done := controlledModelChild(t)
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := acpModelHandshakeWithin(ctx, child, "/repo", ag.ModelCatalog(), time.Second, time.Second)
		finished <- err
	}()
	requireModelPhase(t, methods, "initialize")
	timer := time.NewTimer(700 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	replies <- json.RawMessage(grokCatalogInitialize)
	requireModelPhase(t, methods, "authenticate")
	if err := <-finished; !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatal("authentication reset the startup budget", err, ctx.Err())
	}
	child.Stop()
	<-done
	if len(methods) != 0 {
		t.Fatal("timed-out authentication continued")
	}
}
