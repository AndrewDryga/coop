package networkgateway

import (
	"fmt"
	"strings"
	"testing"
)

func TestNativeRunConfigValidatesAccountAndOriginInventory(t *testing.T) {
	fresh := func() NativeRunConfig {
		run := strings.Repeat("a", 32)
		return NativeRunConfig{Version: 1, RunID: run, Accounts: []NativeAccountConfig{{Binding: NativeBrokerBinding{RunID: run, Provider: "codex", Account: "selected", Epoch: 1}, Origins: []NativeOriginConfig{{Host: "api.example.com", Requests: []NativeBrokerRequestLine{{Method: "GET", Path: "/models", CredentialFree: true}}}}}}}
	}
	for name, mutate := range map[string]func(*NativeRunConfig){
		"version": func(c *NativeRunConfig) { c.Version = 2 }, "run syntax": func(c *NativeRunConfig) { c.RunID = "bad" }, "binding run": func(c *NativeRunConfig) { c.Accounts[0].Binding.RunID = strings.Repeat("b", 32) },
		"epoch": func(c *NativeRunConfig) { c.Accounts[0].Binding.Epoch = 0 }, "account": func(c *NativeRunConfig) { c.Accounts[0].Binding.Account = "" }, "provider": func(c *NativeRunConfig) { c.Accounts[0].Binding.Provider = "" }, "provider syntax": func(c *NativeRunConfig) { c.Accounts[0].Binding.Provider = "Codex" },
		"duplicate provider": func(c *NativeRunConfig) { c.Accounts = append(c.Accounts, c.Accounts[0]) }, "no accounts": func(c *NativeRunConfig) { c.Accounts = nil }, "no origins": func(c *NativeRunConfig) { c.Accounts[0].Origins = nil }, "uncanonical origin": func(c *NativeRunConfig) { c.Accounts[0].Origins[0].Host = "API.example.com" },
		"duplicate origin": func(c *NativeRunConfig) {
			c.Accounts[0].Origins = append(c.Accounts[0].Origins, c.Accounts[0].Origins[0])
		}, "no requests": func(c *NativeRunConfig) { c.Accounts[0].Origins[0].Requests = nil }, "request limit": func(c *NativeRunConfig) { c.Accounts[0].Origins[0].Requests = make([]NativeBrokerRequestLine, 129) },
	} {
		t.Run(name, func(t *testing.T) {
			c := fresh()
			mutate(&c)
			if err := validateNativeRunConfig(c, "", nil); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	c := fresh()
	if err := validateNativeRunConfig(c, c.RunID, []string{"api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := validateNativeRunConfig(c, strings.Repeat("b", 32), nil); err == nil {
		t.Fatal("wrong expected run accepted")
	}
	for _, count := range []int{4, 5} {
		c := fresh()
		for i := 1; i < count; i++ {
			account := fresh().Accounts[0]
			account.Binding.Provider = string(rune('a' + i))
			account.Origins[0].Host = fmt.Sprintf("api%d.example.com", i)
			c.Accounts = append(c.Accounts, account)
		}
		err := validateNativeRunConfig(c, "", nil)
		if (err == nil) != (count == 4) {
			t.Fatalf("account count %d: %v", count, err)
		}
	}
	for _, count := range []int{4, 5} {
		c := fresh()
		for i := 0; i < count; i++ {
			c.Downloads = append(c.Downloads, CredentialBrokerRoute{Name: fmt.Sprintf("download%d", i), Kind: CredentialBrokerDownload, Upstream: "downloads.example.com", Port: 443, Allow: []BrokerRequestLine{{Method: "GET", Path: fmt.Sprintf("/public/%d", i)}}})
		}
		err := validateNativeRunConfig(c, "", nil)
		if (err == nil) != (count == 4) {
			t.Fatalf("download count %d: %v", count, err)
		}
	}
	c = fresh()
	download := CredentialBrokerRoute{Name: "download", Kind: CredentialBrokerDownload, Upstream: "downloads.example.com", Port: 443, Allow: []BrokerRequestLine{{Method: "GET", Path: "/public"}}}
	c.Downloads = []CredentialBrokerRoute{download}
	for _, origins := range [][]string{{"downloads.example.com", "api.example.com"}, {"api.example.com", "downloads.example.com"}, nil} {
		if err := validateNativeRunConfig(c, "", origins); err != nil {
			t.Fatalf("valid origin set: %v", err)
		}
	}
	for _, origins := range [][]string{{}, {"api.example.com"}, {"api.example.com", "downloads.example.com", "extra.example.com"}, {"api.example.com", "downloads.example.com", "downloads.example.com"}} {
		if err := validateNativeRunConfig(c, "", origins); err == nil {
			t.Fatalf("invalid origin set accepted: %v", origins)
		}
	}
	c.Downloads[0].Upstream = "api.example.com"
	if err := validateNativeRunConfig(c, "", []string{"api.example.com"}); err != nil {
		t.Fatalf("shared download origin: %v", err)
	}
	for _, mutate := range []func(*CredentialBrokerRoute){func(d *CredentialBrokerRoute) { d.Kind = "wrong" }, func(d *CredentialBrokerRoute) { d.Allow[0].Path = "/a/../b" }, func(d *CredentialBrokerRoute) { d.Allow = append(d.Allow, d.Allow[0]) }} {
		c := fresh()
		d := download
		d.Allow = append([]BrokerRequestLine(nil), download.Allow...)
		mutate(&d)
		c.Downloads = []CredentialBrokerRoute{d}
		if err := validateNativeRunConfig(c, "", nil); err == nil {
			t.Fatal("invalid download accepted")
		}
	}
}
