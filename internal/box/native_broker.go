package box

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

type nativeRunAccount struct {
	agent             agents.Agent
	account           string
	record            *accountAuthority
	seed              agents.NativeBrokerSeed
	routes            []agents.NativeBrokerRoute
	revision          uint64
	canonicalRevision uint64
	canonicalAccess   string
	canonicalExpiry   time.Time
	invalid           bool
	accessOverride    string                        // explicit default host-key selection, frozen for this run
	ephemeral         *agents.NativeCredentialState // env-only authority never published as an account
}

type nativeRun struct {
	cfg           *config.Config
	runID         string
	accounts      []*nativeRunAccount
	dir, publicCA string
	publicFiles   []string
	cancel        context.CancelFunc
	workers       sync.WaitGroup
}

func planNativeRun(ctx context.Context, cfg *config.Config, rt runtime.Runtime, spec RunSpec) (*nativeRun, error) {
	return planNativeAccounts(ctx, cfg, rt, spec, true)
}

func planNativeAccounts(ctx context.Context, cfg *config.Config, rt runtime.Runtime, spec RunSpec, persistent bool) (*nativeRun, error) {
	if !spec.Homes || spec.Login || cfg.Egress == "none" {
		return nil, nil
	}
	run := &nativeRun{cfg: cfg.NativeAuthorityConfig(), runID: randomHex(16)}
	for _, provider := range credentialScope(cfg, spec) {
		ag, _ := agents.Get(provider)
		if err := validateNativeOverrides(cfg, spec, ag); err != nil {
			return nil, err
		}
		account := cfg.ActiveProfile(provider)
		record, exists, err := ensureNativeAccount(ctx, cfg, rt, ag, account)
		if err != nil {
			return nil, err
		}
		if exists && record.Revoked {
			return nil, fmt.Errorf("%s account %q was removed; use coop login", provider, account)
		}
		if exists {
			record, err = renewNativeAccount(ctx, cfg, ag, account, time.Now().Add(2*time.Minute))
			if err != nil {
				return nil, fmt.Errorf("prepare %s account %q: %w", provider, account, err)
			}
		}
		var state agents.NativeCredentialState
		if exists {
			state, err = ag.NativeCredentials().Inspect(record.Artifacts, time.Now())
			if err != nil {
				return nil, err
			}
		}
		// Derive an environment overlay from this exact canonical epoch. A second
		// authority read could mistake a concurrent host sign-in for an env key.
		overlay, environment, err := nativeEnvironment(run.cfg, ag, account, state)
		if err != nil {
			return nil, err
		}
		if !exists && !environment {
			continue
		}
		if !exists {
			record = &accountAuthority{Epoch: 1, Revision: 1, Selection: overlay.Selection, Principal: overlay.Principal}
		}
		native := ag.NativeCredentials()
		if native.Broker.Seed == nil || native.Broker.Routes == nil {
			return nil, fmt.Errorf("%s has no native broker contract", provider)
		}
		seed, err := native.Broker.Seed(record.Selection)
		if err != nil {
			return nil, err
		}
		if native.Broker.Env != nil {
			env, err := native.Broker.Env(record.Selection, cfg.EffortFor(provider))
			if err != nil {
				return nil, err
			}
			for key, value := range env {
				seed.Env[key] = value
			}
		}
		routes, err := native.Broker.Routes(record.Selection)
		if err != nil {
			return nil, err
		}
		if persistent {
			home, err := cfg.NativeHome(provider)
			if err != nil {
				return nil, err
			}
			if err := prepareNativeAuth(ctx, home, native, &seed); err != nil {
				return nil, fmt.Errorf("%s repository account state in %s (recovery custody: %s): %w", provider, home, filepath.Dir(home), err)
			}
		} else {
			seed.StorageHostname, seed.StorageUsername = "coop-native-"+randomHex(12), "node"
		}
		selected := &nativeRunAccount{agent: ag, account: account, record: record, seed: seed, routes: routes}
		if !exists {
			selected.ephemeral = &overlay
		} else if environment {
			selected.accessOverride = overlay.AccessToken
		}
		run.accounts = append(run.accounts, selected)
	}
	if len(run.accounts) == 0 {
		return nil, nil
	}
	return run, nil
}

func (a *nativeRunAccount) inspect(record *accountAuthority) (agents.NativeCredentialState, error) {
	if a.ephemeral != nil {
		return *a.ephemeral, nil
	}
	return a.agent.NativeCredentials().Inspect(record.Artifacts, time.Now())
}

func (r *nativeRun) refresh(ctx context.Context, account *nativeRunAccount) (*accountAuthority, error) {
	if account.ephemeral != nil {
		_, exists, err := readNativeAccount(ctx, r.cfg, account.agent, account.account)
		if err != nil || exists {
			return nil, errors.New("environment account authority changed; restart with the selected host account")
		}
		return account.record, nil
	}
	return renewNativeAccount(ctx, r.cfg, account.agent, account.account, time.Now().Add(2*time.Minute))
}

// The receipt is outside the writable home. Native sign-in/out is not allowed
// to publish account-wide credentials; unexpected local grants are preserved
// and refused on reuse, never overwritten by a later launch.
func prepareNativeAuth(ctx context.Context, home string, native agents.NativeCredentialSpec, seed *agents.NativeBrokerSeed) error {
	custody, err := openPrivateAccountTree(filepath.Dir(home), "auth")
	if err != nil {
		return err
	}
	defer custody.close()
	lock, err := custody.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	root, err := safefile.OpenRoot(home)
	if err != nil {
		return err
	}
	defer root.Close()
	type authReceipt struct {
		Version            int
		Family             string
		Hostname, Username string
	}
	want := authReceipt{Version: 1, Family: seed.Family, Hostname: "coop-native-" + randomHex(12), Username: "node"}
	receipt, err := safefile.ReadRegular(custody.dir(), "seed.json", 4096)
	if err == nil {
		want = authReceipt{}
		if json.Unmarshal(receipt, &want) != nil || want.Version != 1 || want.Family == "" || len(want.Hostname) != 36 || !strings.HasPrefix(want.Hostname, "coop-native-") || want.Username != "node" {
			return errors.New("invalid native auth seed receipt")
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(want.Hostname, "coop-native-")); err != nil {
			return errors.New("invalid native auth storage identity")
		}
		seed.StorageHostname, seed.StorageUsername = want.Hostname, want.Username
		if native.Broker.Check == nil {
			return errors.New("native credential divergence checker unavailable")
		}
		readFiles := func() (map[string][]byte, error) {
			files := map[string][]byte{}
			for _, artifact := range append(slices.Clone(native.Artifacts), native.Broker.CheckFiles...) {
				data, err := safefile.ReadRegular(root, artifact.Name, artifact.Limit)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return nil, err
				}
				files[artifact.Name] = data
			}
			return files, nil
		}
		files, err := readFiles()
		if err != nil {
			return err
		}
		if want.Family == seed.Family {
			return native.Broker.Check(files, *seed)
		}
		previous, err := native.Broker.Seed(want.Family)
		if err != nil {
			return err
		}
		previous.StorageHostname, previous.StorageUsername = want.Hostname, want.Username
		if err := native.Broker.Check(files, previous); err != nil {
			return err
		}
		// Only add missing public selectors. Native settings and unexpected grants
		// are preserved; a host account change is not permission to replace them.
		var missing []string
		for name, data := range seed.Files {
			if _, exists := files[name]; exists {
				continue
			}
			if !accountNameValid(name) {
				return errors.New("invalid native seed filename")
			}
			files[name] = data
			missing = append(missing, name)
		}
		if err := native.Broker.Check(files, *seed); err != nil {
			return err
		}
		slices.Sort(missing)
		for _, name := range missing {
			if err := publishMissingNativeAuth(custody, lock, root, name, seed.Files[name]); err != nil {
				return err
			}
		}
		files, err = readFiles()
		if err != nil {
			return err
		}
		if err := native.Broker.Check(files, previous); err != nil {
			return err
		}
		if err := native.Broker.Check(files, *seed); err != nil {
			return err
		}
		want.Family = seed.Family
		data, err := json.Marshal(want)
		if err != nil {
			return err
		}
		return writeAccountPrivate(custody, lock, "seed.json", data)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, artifact := range append(slices.Clone(native.Artifacts), native.Broker.CheckFiles...) {
		if slices.Contains(native.Broker.MergeJSON, artifact.Name) {
			continue
		}
		if _, err := safefile.ReadRegular(root, artifact.Name, artifact.Limit); !errors.Is(err, os.ErrNotExist) {
			return errors.New("unmanaged native credentials already exist; preserve this home and recover with host sign-in")
		}
	}
	names := make([]string, 0, len(seed.Files))
	for name := range seed.Files {
		names = append(names, name)
	}
	slices.Sort(names)
	prepared := map[string][]byte{}
	// Public settings may already exist even when this auth family seeds no
	// files (Claude API keys). Inspect them as well, never silently bless grants.
	for _, artifact := range native.Artifacts {
		if !slices.Contains(native.Broker.MergeJSON, artifact.Name) {
			continue
		}
		data, err := safefile.ReadRegular(root, artifact.Name, artifact.Limit)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			prepared[artifact.Name] = data
		}
	}
	for _, name := range names {
		data := seed.Files[name]
		if slices.Contains(native.Broker.MergeJSON, name) {
			prior, err := safefile.ReadRegular(root, name, 1<<20)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if len(prior) > 0 {
				var old, new map[string]any
				if json.Unmarshal(prior, &old) != nil || json.Unmarshal(data, &new) != nil {
					return errors.New("invalid native settings seed")
				}
				mergeNativeSeed(old, new)
				data, err = json.Marshal(old)
				if err != nil {
					return err
				}
			}
		}
		prepared[name] = data
	}
	seed.StorageHostname, seed.StorageUsername = want.Hostname, want.Username
	if native.Broker.Check == nil {
		return errors.New("native credential divergence checker unavailable")
	}
	// Inspect the merged settings before publishing anything: an unmanaged
	// credential inside a settings document must not survive a first seed.
	if err := native.Broker.Check(prepared, *seed); err != nil {
		return err
	}
	for _, name := range names {
		if err := config.WriteFileAtomicMode(filepath.Join(home, name), prepared[name], 0600); err != nil {
			return err
		}
	}
	if err := custody.checkLock(lock); err != nil {
		return err
	}
	seed.StorageHostname, seed.StorageUsername = want.Hostname, want.Username
	data, err := json.Marshal(want)
	if err != nil {
		return err
	}
	return writeAccountPrivate(custody, lock, "seed.json", data)
}

func publishMissingNativeAuth(custody *accountAuthorityRoot, lock, home *os.File, name string, data []byte) (result error) {
	if !accountNameValid(name) {
		return errors.New("invalid native seed filename")
	}
	stage := "public-seed-" + randomHex(12)
	if err := writeAccountPrivate(custody, lock, stage, data); err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, unix.Unlinkat(int(custody.dir().Fd()), stage, 0), custody.dir().Sync())
	}()
	if err := custody.checkLock(lock); err != nil {
		return err
	}
	err := unix.Linkat(int(custody.dir().Fd()), stage, int(home.Fd()), name, 0)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	return home.Sync()
}

func (r *nativeRun) hostname() string {
	if r != nil {
		for _, account := range r.accounts {
			if account.agent.NativeCredentials().Broker.BindStorageIdentity {
				return account.seed.StorageHostname
			}
		}
	}
	return ""
}

func nativeCACommand(cmd []string) []string {
	return append([]string{"sh", "-c", "umask 077; cat /etc/ssl/certs/ca-certificates.crt /run/coop-native-ca.pem > /tmp/coop-native-ca.pem || exit 1; export SSL_CERT_FILE=/tmp/coop-native-ca.pem GIT_SSL_CAINFO=/tmp/coop-native-ca.pem; exec \"$@\"", "coop-native"}, cmd...)
}

func mergeNativeSeed(destination, seed map[string]any) {
	for key, value := range seed {
		object, isObject := value.(map[string]any)
		existing, isExisting := destination[key].(map[string]any)
		if isObject && isExisting {
			mergeNativeSeed(existing, object)
		} else {
			destination[key] = value
		}
	}
}

func (r *nativeRun) agentArgs(artifacts compositionArtifactOps) ([]string, error) {
	if r == nil {
		return nil, nil
	}
	args := []string{"--mount", networkMount("bind", r.publicCA, networkgateway.NativePublicCA, true)}
	for _, value := range []string{"HTTPS_PROXY=http://" + networkgateway.NativeProxyAddress, "HTTP_PROXY=http://" + networkgateway.NativeProxyAddress,
		"https_proxy=http://" + networkgateway.NativeProxyAddress, "http_proxy=http://" + networkgateway.NativeProxyAddress,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1", "NODE_EXTRA_CA_CERTS=" + networkgateway.NativePublicCA,
		"SSL_CERT_FILE=" + networkgateway.NativePublicCA} {
		args = append(args, "-e", value)
	}
	for _, account := range r.accounts {
		keys := make([]string, 0, len(account.seed.Env))
		for key := range account.seed.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			args = append(args, "-e", key+"="+account.seed.Env[key])
		}
		if len(account.seed.Helper) > 0 {
			// The helper contains ONLY the provider's public selector; it cannot
			// renew or reveal a real grant, even if another process runs it.
			if bytes.ContainsAny(account.seed.Helper, "'\x00") {
				return nil, errors.New("invalid public native helper")
			}
			body := "#!/bin/sh\nprintf '%s\\n' '" + string(bytes.TrimSpace(account.seed.Helper)) + "'\n"
			helper, err := artifacts.writeFile(artifacts.parent, body)
			if err != nil {
				return nil, err
			}
			if err := artifacts.chmod(helper, 0555); err != nil {
				return nil, err
			}
			r.publicFiles = append(r.publicFiles, helper)
			target := "/run/coop-native-" + account.agent.Name()
			args = append(args, "--mount", networkMount("bind", helper, target, true), "-e", "GROK_AUTH_PROVIDER_COMMAND="+target)
		}
	}
	return args, nil
}

func (r *nativeRun) origins() []string {
	if r == nil {
		return nil
	}
	var hosts []string
	for _, account := range r.accounts {
		for _, route := range account.routes {
			if !slices.Contains(hosts, route.Host) {
				hosts = append(hosts, route.Host)
			}
		}
	}
	for _, download := range r.downloads() {
		if !slices.Contains(hosts, download.Upstream) {
			hosts = append(hosts, download.Upstream)
		}
	}
	slices.Sort(hosts)
	return hosts
}

func (r *nativeRun) downloads() []networkgateway.CredentialBrokerRoute {
	var routes []networkgateway.CredentialBrokerRoute
	if r == nil {
		return routes
	}
	for _, account := range r.accounts {
		for _, download := range account.agent.CredentialBroker().Downloads {
			if download.GitRepository == "" {
				continue
			}
			route := networkgateway.CredentialBrokerRoute{Name: download.Name, Kind: networkgateway.CredentialBrokerDownload, Upstream: download.Upstream, Port: 443}
			for _, line := range download.Allow {
				route.Allow = append(route.Allow, networkgateway.BrokerRequestLine{Method: line.Method, Path: line.Path, Query: line.Query})
			}
			routes = append(routes, route)
		}
	}
	return routes
}

func (r *nativeRun) gitRewrites() map[string]string {
	out := map[string]string{}
	if r == nil {
		return out
	}
	for _, account := range r.accounts {
		for _, download := range account.agent.CredentialBroker().Downloads {
			if download.GitRepository != "" {
				out[download.GitRepository] = "http://" + networkgateway.NativeProxyAddress + strings.TrimPrefix(download.GitRepository, "https://"+download.Upstream)
			}
		}
	}
	return out
}

func (r *nativeRun) selected(provider string) bool {
	if r != nil {
		for _, account := range r.accounts {
			if account.agent.Name() == provider {
				return true
			}
		}
	}
	return false
}

func (r *nativeRun) prepare(ctx context.Context, parent string) error {
	if r == nil {
		return nil
	}
	dir, err := os.MkdirTemp(parent, "native-")
	if err != nil {
		return err
	}
	// The outer owner-only directory keeps host custody. Only the guard can
	// mount this child; its own UID must traverse it and read atomic snapshots.
	r.dir = filepath.Join(dir, "guard")
	if err := os.Mkdir(r.dir, 0755); err != nil {
		return err
	}
	ca, key, public, err := nativeCertificateAuthority()
	if err != nil {
		return err
	}
	r.publicCA = filepath.Join(dir, "ca.pem")
	r.publicFiles = []string{r.publicCA}
	if err := os.WriteFile(r.publicCA, public, 0444); err != nil {
		return err
	}
	plan := networkgateway.NativeRunConfig{Version: 1, RunID: r.runID, Downloads: r.downloads()}
	for _, account := range r.accounts {
		state, err := account.inspect(account.record)
		if err != nil {
			return err
		}
		selected := networkgateway.NativeAccountConfig{Binding: networkgateway.NativeBrokerBinding{
			RunID: r.runID, Provider: account.agent.Name(), Account: account.account, Epoch: account.record.Epoch}}
		for _, host := range r.origins() {
			var lines []networkgateway.NativeBrokerRequestLine
			realHeaders, publicHeaders := map[string]string{}, map[string]string{}
			for _, route := range account.routes {
				if route.Host != host {
					continue
				}
				line := networkgateway.NativeBrokerRequestLine{Method: route.Method, Path: route.Path, Query: route.Query, Segment: route.Segment, Suffix: route.Suffix,
					Header: route.Header, HeaderPrefix: route.HeaderPrefix, ClientHeader: route.Header, ClientPrefix: route.HeaderPrefix, ClientMarker: account.seed.Marker}
				if route.CredentialFree {
					line.CredentialFree = true
					line.ClientMarker = ""
				}
				for _, query := range route.Queries {
					line.Queries = append(line.Queries, networkgateway.NativeBrokerQuery{Name: query.Name, Values: query.Values, Optional: query.Optional, MaxBytes: query.MaxBytes})
				}
				lines = append(lines, line)
				if route.AccountHeader != "" {
					realHeaders[route.AccountHeader] = state.AccountID
					publicHeaders[route.AccountHeader] = agents.NativeBrokerAccount
				}
			}
			if len(lines) == 0 {
				continue
			}
			certificate, private, err := nativeLeafCertificate(ca, key, host)
			if err != nil {
				return err
			}
			selected.Origins = append(selected.Origins, networkgateway.NativeOriginConfig{Host: host, Certificate: certificate, Key: private, Requests: lines,
				AccountHeaders: realHeaders, ClientAccountHeaders: publicHeaders})
		}
		plan.Accounts = append(plan.Accounts, selected)
		current, err := r.refresh(ctx, account)
		if err != nil {
			return err
		}
		if err := r.publish(account, current, nil); err != nil {
			return err
		}
		if account.invalid {
			return errors.New("native account changed during launch; retry with the selected host account")
		}
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.dir, "config.json"), data, 0444); err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	for _, account := range r.accounts {
		r.workers.Go(func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-lifetime.Done():
					return
				case <-ticker.C:
				}
				renewal, done := context.WithTimeout(lifetime, 30*time.Second)
				record, err := r.refresh(renewal, account)
				done()
				publishErr := r.publish(account, record, err)
				if err != nil || publishErr != nil {
					return
				}
			}
		})
	}
	return nil
}

func (r *nativeRun) publish(account *nativeRunAccount, record *accountAuthority, cause error) error {
	snapshot := networkgateway.NativeAccessSnapshot{Binding: networkgateway.NativeBrokerBinding{
		RunID: r.runID, Provider: account.agent.Name(), Account: account.account, Epoch: account.record.Epoch}, Revoked: true}
	account.revision++
	snapshot.Revision = account.revision
	var canonicalState agents.NativeCredentialState
	if !account.invalid && cause == nil && record != nil && !record.Revoked && record.Epoch == account.record.Epoch &&
		record.Principal == account.record.Principal && record.Selection == account.record.Selection {
		state, err := account.inspect(record)
		if err == nil && state.Ready && record.Revision >= account.canonicalRevision &&
			(record.Revision != account.canonicalRevision || state.AccessToken == account.canonicalAccess && state.ExpiresAt.Equal(account.canonicalExpiry)) {
			canonicalState = state
			snapshot.Expires = time.Now().Add(time.Minute)
			if !state.ExpiresAt.IsZero() && state.ExpiresAt.Before(snapshot.Expires) {
				snapshot.Expires = state.ExpiresAt
			}
			snapshot.Credential = state.AccessToken
			if account.accessOverride != "" {
				snapshot.Credential = account.accessOverride
			}
			snapshot.Revoked = false
		}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	defer clear(data)
	// The guard mounts the containing directory, so it observes rename, not a
	// stale file-bind inode. Loss of the host publisher expires the last lease.
	err = config.WriteFileAtomicMode(filepath.Join(r.dir, account.agent.Name()+".json"), data, 0444)
	if err == nil {
		account.invalid = snapshot.Revoked
		if !snapshot.Revoked {
			account.canonicalRevision = record.Revision
			account.canonicalAccess = canonicalState.AccessToken
			account.canonicalExpiry = canonicalState.ExpiresAt
		}
	}
	return err
}

func (r *nativeRun) close() error {
	if r == nil {
		return nil
	}
	if r.cancel != nil {
		r.cancel()
		r.workers.Wait()
	}
	var result error
	if r.dir != "" {
		for _, account := range r.accounts {
			result = errors.Join(result, r.publish(account, nil, context.Canceled))
		}
	}
	return result
}

func nativeCertificateAuthority() (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Coop private run broker"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	ca, err := x509.ParseCertificate(der)
	return ca, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), err
}

func nativeLeafCertificate(ca *x509.Certificate, authority *ecdsa.PrivateKey, host string) ([]byte, []byte, error) {
	if ca == nil || authority == nil || host == "" {
		return nil, nil, errors.New("invalid native certificate authority")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, authority)
	if err != nil {
		return nil, nil, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), err
}
