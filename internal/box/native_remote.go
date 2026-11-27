package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

type remoteNativeAccount struct {
	mu        sync.Mutex
	grant     workerproto.ModelGrant
	selection string
	accountID string
	epoch     uint64
}

func planRemoteNativeAccounts(ctx context.Context, cfg *config.Config, spec RunSpec, persistent bool, source *ControllerModels) (*nativeRun, error) {
	run := &nativeRun{cfg: cfg, source: source, runID: randid.Hex(16), filtered: cfg.Egress == "filtered"}
	for _, provider := range credentialScope(cfg, spec) {
		ag, ok := agents.Get(provider)
		if !ok {
			return nil, errors.New("unknown controller model provider")
		}
		account := cfg.ActiveProfile(provider)
		grant, err := source.grant(ctx, run.runID, provider, account)
		if err != nil {
			return nil, err
		}
		native := ag.NativeCredentials()
		if native.Broker.Seed == nil || native.Broker.Routes == nil {
			return nil, errors.New("provider has no native proxy contract")
		}
		seed, err := native.Broker.Seed(grant.Selection)
		if err != nil {
			return nil, err
		}
		if native.Broker.Env != nil {
			env, err := native.Broker.Env(grant.Selection, cfg.EffortFor(provider))
			if err != nil {
				return nil, err
			}
			for k, v := range env {
				seed.Env[k] = v
			}
		}
		routes, err := native.Broker.Routes(grant.Selection)
		if err != nil {
			return nil, err
		}
		if persistent {
			home, err := cfg.NativeHome(provider)
			if err != nil {
				return nil, err
			}
			if err = prepareNativeAuth(ctx, home, native, &seed); err != nil {
				return nil, err
			}
		} else {
			seed.StorageHostname, seed.StorageUsername = "coop-native-"+randid.Hex(12), "node"
		}
		run.accounts = append(run.accounts, &nativeRunAccount{agent: ag, account: account, seed: seed, routes: routes,
			remote: &remoteNativeAccount{grant: grant, epoch: grant.Epoch, selection: grant.Selection, accountID: grant.AccountID}})
	}
	if len(run.accounts) == 0 {
		return nil, errors.New("controller job selected no model account")
	}
	return run, nil
}

func (r *nativeRun) publishRemote(account *nativeRunAccount, revoked bool) error {
	account.remote.mu.Lock()
	if revoked {
		account.remote.grant.ExpiresAt = time.Now()
	}
	grant := account.remote.grant
	account.remote.mu.Unlock()
	account.revision++
	snapshot := networkgateway.NativeAccessSnapshot{Binding: networkgateway.NativeBrokerBinding{
		RunID: r.runID, Provider: account.agent.Name(), Account: account.account, Epoch: account.epoch()},
		Revision: account.revision, Expires: grant.ExpiresAt, Credential: grant.Token, Revoked: revoked}
	if revoked {
		snapshot.Credential = ""
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return config.WriteFileAtomicMode(filepath.Join(r.dir, account.agent.Name()+".json"), raw, 0444)
}

func (r *nativeRun) serveRemote(ctx context.Context, account *nativeRunAccount) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		read, cancel := context.WithTimeout(ctx, 5*time.Second)
		grant, err := r.source.grant(read, r.runID, account.agent.Name(), account.account)
		cancel()
		if err != nil {
			if errors.Is(err, workerproto.ErrModelGrantRefused) {
				_ = r.publishRemote(account, true)
				return
			}
			continue
		}
		remote := account.remote
		if grant.Epoch != remote.epoch || grant.Selection != remote.selection || grant.AccountID != remote.accountID {
			_ = r.publishRemote(account, true)
			return
		}
		remote.mu.Lock()
		remote.grant = grant
		remote.mu.Unlock()
		if err = r.publishRemote(account, false); err != nil {
			return
		}
	}
}

func (r *nativeRun) startModelRelay(ctx context.Context) error {
	socket := filepath.Join(r.dir, "relay.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err = os.Chmod(socket, 0666); err != nil {
		listener.Close()
		return err
	}
	client := r.source.client()
	context.AfterFunc(ctx, client.CloseIdleConnections)
	r.relay = &http.Server{ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 64 << 10, Handler: http.HandlerFunc(func(w http.ResponseWriter, in *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(in.URL.Path, "/"), "/", 3)
		if len(parts) != 3 {
			http.Error(w, "model route refused", http.StatusForbidden)
			return
		}
		var account *nativeRunAccount
		for _, a := range r.accounts {
			if a.agent.Name() == parts[0] {
				account = a
				break
			}
		}
		if account == nil {
			http.Error(w, "model provider refused", http.StatusForbidden)
			return
		}
		found := false
		for _, route := range account.routes {
			if route.Host == parts[1] {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "model origin refused", http.StatusForbidden)
			return
		}
		account.remote.mu.Lock()
		grant := account.remote.grant
		account.remote.mu.Unlock()
		if !time.Now().Before(grant.ExpiresAt) {
			http.Error(w, "model grant expired", http.StatusForbidden)
			return
		}
		stream, cancel := context.WithCancel(in.Context())
		defer cancel()
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stream.Done():
					return
				case <-ticker.C:
				}
				account.remote.mu.Lock()
				current := account.remote.grant
				account.remote.mu.Unlock()
				if !time.Now().Before(current.ExpiresAt) {
					cancel()
					return
				}
			}
		}()
		proxy := &httputil.ReverseProxy{Transport: client.Transport, FlushInterval: -1,
			Rewrite: func(p *httputil.ProxyRequest) {
				p.Out.URL.Scheme, p.Out.URL.Host, p.Out.Host = "http", "localhost", "localhost"
				p.Out.URL.Path = fmt.Sprintf("/v1/model-access/%s/forward/%s/%s/%s", r.source.Session, parts[0], parts[1], parts[2])
				p.Out.URL.RawPath = ""
				p.Out.Header.Set("X-Coop-Model-Grant", grant.Token)
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "model relay unavailable", http.StatusBadGateway)
			}}
		in.Body = http.MaxBytesReader(w, in.Body, 32<<20)
		proxy.ServeHTTP(w, in.WithContext(stream))
	})}
	go func() { _ = r.relay.Serve(listener) }()
	context.AfterFunc(ctx, func() { _ = r.relay.Close() })
	return nil
}
