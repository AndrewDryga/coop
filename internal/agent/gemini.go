package agent

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
	"golang.org/x/crypto/scrypt"
)

type geminiAgent struct{}

func checkGeminiBroker(files map[string][]byte, seed NativeBrokerSeed) error {
	if data, exists := files["gemini-credentials.json"]; exists {
		if err := checkGeminiEncryptedProviderServices(data, seed); err != nil {
			return err
		}
	}
	if _, exists := files["oauth_creds.json"]; exists {
		return errNativeBrokerDiverged
	}
	if key, exists := files["api-key"]; exists && string(key) != seed.Marker {
		return errNativeBrokerDiverged
	}
	if _, err := nativePublicObject(files["settings.json"]); err != nil {
		return err
	}
	if data, exists := files["google_accounts.json"]; exists {
		if _, err := nativePublicObject(data); err != nil {
			return err
		}
	}
	return nil
}

type geminiNativeSelector struct {
	Security struct {
		Auth struct {
			Selected string `json:"selectedType"`
		} `json:"auth"`
	} `json:"security"`
}
type geminiNativeIdentity struct {
	Active string   `json:"active"`
	Old    []string `json:"old"`
}

func (geminiAgent) NativeCredentials() NativeCredentialSpec {
	return NativeCredentialSpec{Environment: func(selection, key, value string) (NativeCredentialState, error) {
		if key == "GOOGLE_API_KEY" {
			return NativeCredentialState{}, fmt.Errorf("native Gemini vertex-ai authentication is not supported by the native broker")
		}
		return nativeAPIEnvironment("gemini-api-key", "GEMINI_API_KEY")(selection, key, value)
	}, Defaults: func(source string) (map[string][]byte, error) {
		return nativeDefaultSettings(source, "settings.json", "GEMINI.md", map[string]string{"theme": "string", "ui.theme": "string", "security.folderTrust.enabled": "bool"})
	}, LegacyGrants: []string{"oauth_creds.json", "api-key"}, Artifacts: []NativeCredentialArtifact{
		{Name: "settings.json", Limit: 1 << 20, Required: true, Import: importGeminiSelector, AccessOnly: importGeminiSelector},
		{Name: "oauth_creds.json", Limit: 1 << 20, Import: importGeminiOAuth, AccessOnly: projectGeminiOAuth},
		{Name: "google_accounts.json", Limit: 1 << 20, Import: importGeminiIdentity, AccessOnly: importGeminiIdentity},
		{Name: "api-key", Limit: 4 << 10, Import: importGeminiKey, AccessOnly: importGeminiKey},
	}, LegacyCheck: checkGeminiLegacyCutover, Select: selectGeminiNative, Inspect: inspectGeminiNative, Renew: renewGeminiNative, Broker: NativeBrokerSpec{BindStorageIdentity: true, MergeJSON: []string{"settings.json"}, Seed: seedGeminiBroker, Env: geminiBrokerEnv, Routes: geminiBrokerRoutes, Check: checkGeminiBroker, CheckFiles: []NativeCredentialArtifact{{Name: "gemini-credentials.json", Limit: 1 << 20}}}}
}

func checkGeminiLegacyCutover(home string) error {
	_, err := os.Lstat(filepath.Join(home, "gemini-credentials.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("legacy Gemini encrypted cache has no recorded storage identity; account and cache retained unchanged for explicit host recovery")
}

func seedGeminiBroker(selection string) (NativeBrokerSeed, error) {
	out := publicNativeSeed("coop-native-gemini-v1")
	out.Family = selection
	if selection != "oauth-personal" && selection != "gemini-api-key" {
		return out, fmt.Errorf("unsupported Gemini broker selection")
	}
	selector := geminiNativeSelector{}
	selector.Security.Auth.Selected = "oauth-personal"
	settings, err := nativeJSON(selector)
	if err != nil {
		return out, err
	}
	out.Files["settings.json"] = settings
	if selection == "gemini-api-key" {
		out.Env["GEMINI_API_KEY"] = out.Marker
	} else {
		out.Env["GOOGLE_GENAI_USE_GCA"] = "true"
		out.Env["GOOGLE_CLOUD_ACCESS_TOKEN"] = out.Marker
	}
	identity, err := nativeJSON(geminiNativeIdentity{Active: nativeBrokerEmail, Old: []string{}})
	out.Files["google_accounts.json"] = identity
	return out, err
}

// The native encrypted cache may also hold MCP grants. Authenticate it with its
// original public home identity, inspect provider services, and never rewrite it.
func checkGeminiEncryptedProviderServices(data []byte, seed NativeBrokerSeed) error {
	if len(data) == 0 || len(data) > 1<<20 || !validNativeIdentity(seed.StorageHostname) || !validNativeIdentity(seed.StorageUsername) {
		return errNativeBrokerDiverged
	}
	parts := strings.Split(string(data), ":")
	if len(parts) != 3 {
		return errNativeBrokerDiverged
	}
	iv, e1 := hex.DecodeString(parts[0])
	tag, e2 := hex.DecodeString(parts[1])
	body, e3 := hex.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || (len(iv) != 12 && len(iv) != 16) || len(tag) != 16 {
		return errNativeBrokerDiverged
	}
	key, err := scrypt.Key([]byte("gemini-cli-oauth"), []byte(seed.StorageHostname+"-"+seed.StorageUsername+"-gemini-cli"), 16384, 8, 1, 32)
	if err != nil {
		return errNativeBrokerDiverged
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return errNativeBrokerDiverged
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return errNativeBrokerDiverged
	}
	plain, err := gcm.Open(nil, iv, append(body, tag...), nil)
	if err != nil {
		return errNativeBrokerDiverged
	}
	defer clear(plain)
	doc, err := nativePublicObject(plain)
	if err != nil {
		return err
	}
	for _, rule := range []struct{ service, account, kind string }{{"gemini-cli-oauth", "main-account", "Bearer"}, {"gemini-cli-api-key", "default-api-key", "ApiKey"}} {
		raw, exists := doc[rule.service]
		if !exists {
			continue
		}
		var entries map[string]string
		if json.Unmarshal(raw, &entries) != nil || entries == nil {
			return errNativeBrokerDiverged
		}
		for account, password := range entries {
			if account != rule.account {
				return errNativeBrokerDiverged
			}
			credential, err := nativePublicObject([]byte(password))
			if err != nil {
				return err
			}
			server, err := nativePublicString(credential, "serverName")
			if err != nil || server != account {
				return errNativeBrokerDiverged
			}
			token, err := nativePublicObject(credential["token"])
			if err != nil {
				return err
			}
			access, err := nativePublicString(token, "accessToken")
			if err != nil || access != seed.Marker {
				return errNativeBrokerDiverged
			}
			for _, field := range []string{"refreshToken", "idToken"} {
				value, err := nativePublicString(token, field)
				if err != nil || value != "" {
					return errNativeBrokerDiverged
				}
			}
			kind, err := nativePublicString(token, "tokenType")
			if err != nil || kind != "" && kind != rule.kind {
				return errNativeBrokerDiverged
			}
		}
	}
	return nil
}

const geminiBrokerSettingsDir = "/etc/gemini-cli/native-broker"

func geminiBrokerSettings(selection, effort string) string {
	settings := map[string]any{"general": geminiNoUpdates}
	if effort != "" {
		_ = json.Unmarshal([]byte(geminiThinkingSettings(effort)), &settings)
	}
	settings["security"] = map[string]any{"auth": map[string]any{"selectedType": selection}}
	return geminiSystemSettings(settings)
}

func geminiBrokerEnv(selection, effort string) (map[string]string, error) {
	if selection != "oauth-personal" && selection != "gemini-api-key" {
		return nil, fmt.Errorf("unsupported Gemini broker selection")
	}
	level := effort
	if level == "" {
		level = "default"
	} else if _, ok := geminiThinking[effort]; !ok {
		return nil, fmt.Errorf("gemini effort %q has no thinking setting; use low or high", effort)
	}
	dir := geminiBrokerSettingsDir + "/" + selection
	return map[string]string{geminiThinkingEnv: dir, "GEMINI_CLI_SYSTEM_SETTINGS_PATH": dir + "/" + level + ".json"}, nil
}

func geminiBrokerRoutes(selection string) ([]NativeBrokerRoute, error) {
	if selection == "gemini-api-key" {
		var out []NativeBrokerRoute
		for _, operation := range []string{"generateContent", "countTokens", "streamGenerateContent"} {
			route := NativeBrokerRoute{Host: "generativelanguage.googleapis.com", Method: "POST", Path: "/v1beta/models/", Segment: "token", Suffix: ":" + operation, Header: "X-Goog-Api-Key"}
			if operation == "streamGenerateContent" {
				route.Query = "alt=sse"
			}
			out = append(out, route)
		}
		return out, nil
	}
	if selection != "oauth-personal" {
		return nil, fmt.Errorf("unsupported Gemini broker selection")
	}
	var out []NativeBrokerRoute
	add := func(method, path, query string) {
		out = append(out, NativeBrokerRoute{Host: "cloudcode-pa.googleapis.com", Method: method, Path: path, Query: query, Header: "Authorization", HeaderPrefix: "Bearer "})
	}
	for _, operation := range []string{"loadCodeAssist", "onboardUser", "generateContent", "countTokens", "listExperiments", "fetchAdminControls", "retrieveUserQuota", "recordCodeAssistMetrics", "setCodeAssistGlobalUserSetting"} {
		add("POST", "/v1internal:"+operation, "")
	}
	add("POST", "/v1internal:streamGenerateContent", "alt=sse")
	add("GET", "/v1internal:getCodeAssistGlobalUserSetting", "")
	out = append(out, NativeBrokerRoute{Host: "cloudcode-pa.googleapis.com", Method: "GET", Path: "/v1internal/operations/", Segment: "segments", Header: "Authorization", HeaderPrefix: "Bearer "})
	out = append(out, NativeBrokerRoute{Host: "www.googleapis.com", Method: "GET", Path: "/oauth2/v2/userinfo", Header: "Authorization", HeaderPrefix: "Bearer "})
	return out, nil
}

func selectGeminiNative(files map[string][]byte) (map[string][]byte, error) {
	data, err := importGeminiSelector(files["settings.json"])
	if err != nil {
		return nil, err
	}
	var selector geminiNativeSelector
	_ = json.Unmarshal(data, &selector)
	out := map[string][]byte{"settings.json": data}
	if selector.Security.Auth.Selected == "oauth-personal" {
		out["oauth_creds.json"], out["google_accounts.json"] = files["oauth_creds.json"], files["google_accounts.json"]
	} else {
		out["api-key"] = files["api-key"]
	}
	return out, nil
}

func importGeminiSelector(data []byte) ([]byte, error) {
	var selector geminiNativeSelector
	if json.Unmarshal(data, &selector) != nil || selector.Security.Auth.Selected != "oauth-personal" && selector.Security.Auth.Selected != "gemini-api-key" {
		return nil, fmt.Errorf("unsupported Gemini selected credential family")
	}
	return nativeJSON(selector)
}

func importGeminiIdentity(data []byte) ([]byte, error) {
	var identity geminiNativeIdentity
	if json.Unmarshal(data, &identity) != nil || !validNativeIdentity(identity.Active) {
		return nil, fmt.Errorf("native Gemini selected native identity missing")
	}
	identity.Old = []string{}
	return nativeJSON(identity)
}

func importGeminiKey(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > 4<<10 || !validNativeGrant(string(data)) {
		return nil, fmt.Errorf("invalid Gemini API key")
	}
	return bytes.Clone(data), nil
}

func importGeminiOAuth(data []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if len(data) == 0 || len(data) > 1<<20 || json.Unmarshal(data, &doc) != nil || doc == nil {
		return nil, fmt.Errorf("invalid Gemini OAuth grant")
	}
	return nativeJSON(doc)
}

func projectGeminiOAuth(data []byte) ([]byte, error) {
	var access struct {
		Token  string `json:"access_token"`
		Expiry int64  `json:"expiry_date"`
		Type   string `json:"token_type,omitempty"`
		Scope  string `json:"scope,omitempty"`
	}
	if err := json.Unmarshal(data, &access); err != nil {
		return nil, errors.New("invalid Gemini access grant")
	}
	return nativeJSON(access)
}

func inspectGeminiNative(files map[string][]byte, now time.Time) (NativeCredentialState, error) {
	var selector geminiNativeSelector
	if json.Unmarshal(files["settings.json"], &selector) != nil {
		return NativeCredentialState{}, fmt.Errorf("invalid Gemini selector")
	}
	switch selector.Security.Auth.Selected {
	case "gemini-api-key":
		key := files["api-key"]
		if len(files["oauth_creds.json"]) != 0 || len(files["google_accounts.json"]) != 0 || len(key) == 0 || len(key) > 4<<10 || !validNativeGrant(string(key)) {
			return NativeCredentialState{}, fmt.Errorf("invalid Gemini API-key inventory")
		}
		return NativeCredentialState{Selection: "gemini-api-key", Principal: "opaque-api-key", AccessToken: string(key), Ready: true, APIKey: true}, nil
	case "oauth-personal":
		if len(files["api-key"]) != 0 {
			return NativeCredentialState{}, fmt.Errorf("conflicting Gemini authority")
		}
		var identity geminiNativeIdentity
		if json.Unmarshal(files["google_accounts.json"], &identity) != nil || !validNativeIdentity(identity.Active) || len(identity.Old) != 0 {
			return NativeCredentialState{}, fmt.Errorf("invalid Gemini selected identity")
		}
		var grant struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Expiry  int64  `json:"expiry_date"`
			IDToken string `json:"id_token"`
			Kind    string `json:"token_type"`
		}
		if json.Unmarshal(files["oauth_creds.json"], &grant) != nil || grant.Expiry <= 0 || grant.Kind != "" && !strings.EqualFold(grant.Kind, "Bearer") {
			return NativeCredentialState{}, fmt.Errorf("invalid Gemini native OAuth shape")
		}
		access, refreshable := validGeminiGrant(grant.Access), validGeminiGrant(grant.Refresh)
		if !access && !refreshable || grant.Access != "" && !access || grant.Refresh != "" && !refreshable {
			return NativeCredentialState{}, fmt.Errorf("invalid Gemini native grant")
		}
		if grant.IDToken != "" {
			claims, err := nativeJWT(grant.IDToken)
			if err != nil || claims.Email != "" && !strings.EqualFold(claims.Email, identity.Active) {
				return NativeCredentialState{}, fmt.Errorf("native Gemini token/cache identity mismatch")
			}
		}
		expiry := time.UnixMilli(grant.Expiry)
		return NativeCredentialState{Selection: "oauth-personal", Principal: nativeTuple(identity.Active), AccountID: identity.Active,
			AccessToken: grant.Access, ExpiresAt: expiry, Refreshable: refreshable, Ready: refreshable || access && expiry.After(now)}, nil
	default:
		return NativeCredentialState{}, fmt.Errorf("unsupported Gemini selected authority")
	}
}

func renewGeminiNative(ctx context.Context, files map[string][]byte, deadline time.Time, retain func([]byte) error) (map[string][]byte, error) {
	before, err := inspectGeminiNative(files, time.Now())
	if err != nil {
		return nil, err
	}
	if before.Selection != "oauth-personal" || !before.Refreshable {
		return nil, fmt.Errorf("native Gemini authority needs host sign-in")
	}
	result, renewErr := renewGeminiAuthority(ctx, files["oauth_creds.json"], deadline, retain)
	if len(result.Credential) == 0 {
		return nil, renewErr
	}
	next := cloneNativeFiles(files)
	next["oauth_creds.json"] = result.Credential
	after, err := inspectGeminiNative(next, time.Now())
	if err != nil || before.Principal != after.Principal || before.Selection != after.Selection {
		return nil, fmt.Errorf("native Gemini issued identity invalid; retained for recovery")
	}
	return next, renewErr
}

func (geminiAgent) NativeHistory(source string, ownsCWD func(string) bool) (NativeHistoryPlan, error) {
	s, err := openNativeHistory(source, ownsCWD)
	if err != nil {
		return NativeHistoryPlan{}, err
	}
	defer s.close()
	for _, base := range []string{"tmp", "history"} {
		s.walk(base, func(path string) {
			if filepath.Base(path) != ".project_root" || filepath.Dir(filepath.Dir(path)) != base {
				return
			}
			data, ok := s.small(path, geminiProjectRootLimit)
			cwd := strings.TrimSpace(string(data))
			if !ok || !s.owns(cwd) {
				return
			}
			bucket := filepath.Dir(path)
			s.add(path, cwd, "")
			s.walk(bucket, func(child string) {
				if child == path {
					return
				}
				rel, _ := filepath.Rel(bucket, child)
				if strings.HasPrefix(rel, "chats"+string(filepath.Separator)) {
					if !strings.HasSuffix(child, ".jsonl") && !strings.HasSuffix(child, ".json") {
						return
					}
					var id, project string
					ok := s.inspect(child, func(reader io.Reader) error {
						if err := nativeHistoryObjects(reader, []string{"sessionId", "projectHash"}, strings.HasSuffix(child, ".json"), func(fields map[string]string, _, _ int64) error {
							for _, pair := range []struct {
								value       string
								destination *string
							}{{fields["sessionId"], &id}, {fields["projectHash"], &project}} {
								if pair.value == "" {
									continue
								}
								if *pair.destination != "" && *pair.destination != pair.value {
									return fmt.Errorf("native Gemini chat has conflicting ownership")
								}
								*pair.destination = pair.value
							}
							return nil
						}); err != nil {
							return err
						}
						if !ValidSessionID(id) || project != fmt.Sprintf("%x", sha256.Sum256([]byte(cwd))) {
							return fmt.Errorf("native Gemini chat ownership does not match its project marker")
						}
						return nil
					})
					if ok {
						s.add(child, cwd, id)
						s.depend(child, path)
					}
					return
				}
				s.add(child, cwd, "")
				s.depend(child, path)
			})
		})
	}
	return s.finish(), nil
}

func (geminiAgent) Usage() UsageSpec {
	return UsageSpec{Quota: geminiUsageQuota, HistoryDirs: []string{"tmp"},
		HistoryFile: func(path string) bool {
			return strings.Contains(path, "/chats/") && (strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".json"))
		}, ParseHistory: geminiUsageHistory, Price: geminiUsagePrice,
		NativeCredentialLease: true, NativeCredentialCheck: geminiUsageCredentialCheck,
		NativeEnv: []string{"NO_BROWSER=true", "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE=false"}}
}

func geminiUsageCredentialCheck(profile string) error {
	data, err := ReadCredentialArtifact(filepath.Join(profile, "oauth_creds.json"), 1<<20)
	if os.IsNotExist(err) {
		return ErrUsageSignIn
	}
	var credential struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if err != nil || json.Unmarshal(data, &credential) != nil || credential.Access == "" && credential.Refresh == "" {
		return fmt.Errorf("limits require the original plain-file OAuth store; encrypted/keychain storage is not portable")
	}
	return nil
}

// Standard text-token rates, https://ai.google.dev/gemini-api/docs/pricing — 2026-10-01.
// Native history does not split audio input; exclude media/tool/storage surcharges from this value.
// Flash 3.6–3.8 use the published promotion through 2026-12-31, not their future tariffs.
func geminiUsagePrice(event UsageEvent) (float64, bool) {
	rates := map[string]usageTariff{
		"gemini-3.8-flash":                   {Input: .75, Read: .075, Output: 3.75},
		"gemini-3.7-flash":                   {Input: .75, Read: .075, Output: 3.75},
		"gemini-3.6-flash":                   {Input: .75, Read: .075, Output: 3.75},
		"gemini-3.5-flash":                   {Input: 1.5, Read: .15, Output: 9},
		"gemini-3.5-flash-lite":              {Input: .3, Read: .03, Output: 2.5},
		"gemini-3.1-pro-preview":             {Input: 2, Read: .2, Output: 12, LongAt: 200001, LongInput: 2, LongOutput: 1.5},
		"gemini-3.1-pro-preview-customtools": {Input: 2, Read: .2, Output: 12, LongAt: 200001, LongInput: 2, LongOutput: 1.5},
		"gemini-2.5-pro":                     {Input: 1.25, Read: .125, Output: 10, LongAt: 200001, LongInput: 2, LongOutput: 1.5},
		"gemini-2.5-flash":                   {Input: .3, Read: .03, Output: 2.5},
	}
	rate, ok := rates[event.Model]
	if !ok {
		return 0, false
	}
	return rate.value(event)
}

type geminiUsageMessage struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Model     string `json:"model"`
	Tokens    *struct {
		Input    *int64 `json:"input"`
		Read     *int64 `json:"cached"`
		Output   *int64 `json:"output"`
		Thoughts *int64 `json:"thoughts"`
		Tool     *int64 `json:"tool"`
	} `json:"tokens"`
}

func geminiUsageHistory(reader io.Reader) (UsageHistory, error) {
	// Failed/auxiliary calls are not all retained by the native recorder.
	out := UsageHistory{Partial: true}
	events := make(map[string]UsageEvent)
	session := ""
	add := func(row geminiUsageMessage) {
		if row.Type != "gemini" || row.Tokens == nil {
			return
		}
		u := row.Tokens
		event := UsageEvent{ID: session + ":" + row.ID, Model: row.Model, Time: usageReset(row.Timestamp), WriteKnown: true,
			Input: usageTokens(u.Input) - usageTokens(u.Read), Read: usageTokens(u.Read), Output: usageTokens(u.Output) + usageTokens(u.Thoughts),
			Partial: u.Input == nil || u.Output == nil || usageTokens(u.Tool) != 0, Approximate: true, ContextKnown: true}
		if session == "" || row.ID == "" || !event.valid() {
			return
		}
		prior, exists := events[event.ID]
		if !exists || event.Time.After(prior.Time) || event.Time.Equal(prior.Time) {
			events[event.ID] = event
		}
	}
	decoder := json.NewDecoder(reader)
	for {
		var row struct {
			geminiUsageMessage
			Session  string               `json:"sessionId"`
			Messages []geminiUsageMessage `json:"messages"`
			Set      struct {
				Messages []geminiUsageMessage `json:"messages"`
			} `json:"$set"`
		}
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if row.Session != "" {
			session = row.Session
		}
		add(row.geminiUsageMessage)
		for _, message := range row.Messages {
			add(message)
		}
		for _, message := range row.Set.Messages {
			add(message)
		}
		// $rewindTo changes visible conversation history, not already consumed tokens.
	}
	for _, event := range events {
		out.Events = append(out.Events, event)
	}
	return out, nil
}

func geminiUsageQuota(ctx context.Context, input UsageQuotaInput) (UsageQuota, error) {
	if input.APIKey && input.ProfileDir == "" {
		return UsageQuota{Auth: "API key"}, nil
	}
	if input.Current != nil {
		deadline, ok := ctx.Deadline()
		if !ok {
			return UsageQuota{}, fmt.Errorf("quota lookup requires a deadline")
		}
		state, _, err := input.Current(ctx, deadline)
		if err != nil || state.Selection != "oauth-personal" || state.AccessToken == "" {
			return UsageQuota{}, ErrUsageSignIn
		}
		return geminiCanonicalQuota(ctx, state.AccessToken)
	}
	auth, _, err := geminiSelectedAuthType(input.ProfileDir)
	if err != nil {
		return UsageQuota{}, fmt.Errorf("cannot read selected authentication mode")
	}
	if input.APIKey || auth == "gemini-api-key" || auth == "vertex-ai" {
		if auth == "vertex-ai" {
			return UsageQuota{Auth: "Vertex"}, nil
		}
		return UsageQuota{Auth: "API key"}, nil
	}
	if auth != "oauth-personal" {
		return UsageQuota{}, ErrUsageSignIn
	}
	if input.Native == nil {
		return UsageQuota{}, fmt.Errorf("native quota helper unavailable")
	}
	data, err := input.Native(ctx, []string{"/usr/local/bin/node", "--input-type=module", "-e", geminiQuotaHelper})
	if err != nil {
		return UsageQuota{}, err
	}
	var raw geminiQuotaResponse
	if len(data) > 1<<20 || json.Unmarshal(data, &raw) != nil || raw.Error != "" {
		return UsageQuota{}, fmt.Errorf("native quota lookup unavailable")
	}
	return raw.quota(), nil
}

// The pinned native helper uses these two read-only Code Assist calls. Canonical
// renewal stays on the host; no helper mounts a second refresh-token authority.
func geminiCanonicalQuota(ctx context.Context, access string) (UsageQuota, error) {
	header := http.Header{"Authorization": {"Bearer " + access}, "Content-Type": {"application/json"}}
	var info struct {
		Project string `json:"cloudaicompanionProject"`
		Current *struct {
			Name string `json:"name"`
		} `json:"currentTier"`
		Paid *struct {
			Name string `json:"name"`
		} `json:"paidTier"`
	}
	const endpoint = "https://cloudcode-pa.googleapis.com/v1internal:"
	if err := readUsageQuota(ctx, http.MethodPost, endpoint+"loadCodeAssist", header,
		strings.NewReader(`{"metadata":{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}}`), &info); err != nil {
		return UsageQuota{}, err
	}
	if info.Current == nil || info.Project == "" {
		return UsageQuota{}, fmt.Errorf("native Code Assist account has no quota project")
	}
	body, err := json.Marshal(map[string]string{"project": info.Project})
	if err != nil {
		return UsageQuota{}, err
	}
	var raw geminiQuotaResponse
	if err := readUsageQuota(ctx, http.MethodPost, endpoint+"retrieveUserQuota", header, bytes.NewReader(body), &raw); err != nil {
		return UsageQuota{}, err
	}
	raw.Tier = info.Current.Name
	if info.Paid != nil {
		raw.Tier = info.Paid.Name
	}
	return raw.quota(), nil
}

// Import only the native auth/server exports: CLI/model initialization can load hooks, MCP
// servers or perform onboarding. Let the process drain asynchronous native token persistence.
const geminiQuotaHelper = `import {getOauthClient,CodeAssistServer} from "/opt/coop/clients/node_modules/@google/gemini-cli/bundle/core-BXNHRQQE.js";
process.on("unhandledRejection",()=>{process.exitCode=1});
try {
  const config={getProxy:()=>undefined,isBrowserLaunchSuppressed:()=>true,isInteractive:()=>false};
  const client=await getOauthClient("oauth-personal",config);
  const server=new CodeAssistServer(client,undefined,{});
  const info=await server.loadCodeAssist({metadata:{ideType:"IDE_UNSPECIFIED",platform:"PLATFORM_UNSPECIFIED",pluginType:"GEMINI"}});
  const project=info.cloudaicompanionProject;
  if(!info.currentTier||typeof project!=="string"||!project) throw new Error();
  const quota=await server.retrieveUserQuota({project});
  console.log(JSON.stringify({tier:info.paidTier?.name??info.currentTier.name,buckets:quota.buckets?.map(b=>({modelId:b.modelId,tokenType:b.tokenType,remainingAmount:b.remainingAmount,remainingFraction:b.remainingFraction,resetTime:b.resetTime}))}));
} catch {process.exitCode=1;console.log(JSON.stringify({error:"quota lookup unavailable"}));}`

type geminiQuotaResponse struct {
	Tier    string `json:"tier"`
	Error   string `json:"error"`
	Buckets []struct {
		Model     string   `json:"modelId"`
		TokenType string   `json:"tokenType"`
		Remaining *string  `json:"remainingAmount"`
		Fraction  *float64 `json:"remainingFraction"`
		Reset     string   `json:"resetTime"`
	} `json:"buckets"`
}

func (raw geminiQuotaResponse) quota() UsageQuota {
	out := UsageQuota{Plan: raw.Tier}
	for _, entry := range raw.Buckets {
		name := entry.Model
		if name == "" {
			name = "Code Assist"
		}
		if entry.TokenType != "" {
			name += " · " + entry.TokenType
		}
		bucket := UsageBucket{Name: name, Reset: usageReset(entry.Reset)}
		if entry.Fraction != nil && *entry.Fraction >= 0 && *entry.Fraction <= 1 {
			used := (1 - *entry.Fraction) * 100
			bucket.Used = &used
		}
		if entry.Remaining != nil {
			bucket.Remaining = *entry.Remaining
		}
		out.Buckets = append(out.Buckets, bucket)
	}
	return out
}

func (geminiAgent) Scaffold() ScaffoldSpec {
	return ScaffoldSpec{
		Project: ScaffoldLayout{Dir: ".gemini"},
		SelectedIgnore: []string{
			"# .gemini may be globally ignored (local Gemini state); keep just the skills symlink",
			"!.gemini/", ".gemini/*", "!.gemini/skills",
		},
	}
}

func (geminiAgent) ModelCatalog() ModelCatalogSpec {
	return ModelCatalogSpec{ParseACP: func(raw json.RawMessage) []Model {
		var doc acpModelCatalog
		if json.Unmarshal(raw, &doc) != nil {
			return nil
		}
		return ParseACPAvailableModels(doc.Models)
	}}
}

func (geminiAgent) ReviewOutput(raw string, _ ReviewOutputContract) (string, bool) { return raw, true }
func (geminiAgent) ReviewFooterLine(string) bool                                   { return false }
func (geminiAgent) PlainOutputProbe() PlainOutputProbe                             { return nil }

func init() { register(geminiAgent{}) }

func (geminiAgent) Name() string        { return "gemini" }
func (geminiAgent) SkillsCapable() bool { return true }
func (geminiAgent) DisplayName() string { return "Gemini CLI" }
func (geminiAgent) Vendor() string      { return "Google" }

// Stream: gemini pairs tool_use with tool_result under `tool_id`, so its foreground tools are
// supervisable.
func (geminiAgent) Stream() StreamSpec {
	return StreamSpec{
		Format: StreamGeminiJSON, Flags: []string{"-o", "stream-json"}, TrailingArgs: 2,
		ToolLifecycle: ToolLifecycleIDs,
	}
}

func (geminiAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_GEMINI_CMD", "gemini --yolo")
	if len(b) == 0 { // match codex's guard: an empty override must still yield a runnable command
		b = []string{"gemini"}
	}
	return withModel(b, cfg.ModelFor("gemini"))
}

func (a geminiAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a geminiAgent) Headless(cfg *config.Config, prompt string) []string {
	return append(a.base(cfg), "-p", prompt)
}

func (a geminiAgent) HeadlessSession(cfg *config.Config, prompt, id string, resume bool) ([]string, bool) {
	if !ValidSessionID(id) {
		return nil, false
	}
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	return append(a.base(cfg), flag, id, "-p", prompt), true
}

// ACP is gemini's own binary, so the resolved model rides along as its normal --model flag.
func (geminiAgent) ACP(cfg *config.Config) []string {
	return withModel([]string{"gemini", "--acp"}, cfg.ModelFor("gemini"))
}

// ACPFinalChunk: every assistant chunk is answer text — gemini's adapter streams no separate commentary phase.
func (geminiAgent) ACPFinalChunk(json.RawMessage) bool    { return true }
func (geminiAgent) ACPProgressChunk(json.RawMessage) bool { return false }

func (geminiAgent) PresetSessionID() bool { return true }

func (a geminiAgent) StartSession(cfg *config.Config, id string) []string {
	if id == "" {
		return a.Interactive(cfg)
	}
	return append(a.base(cfg), "--session-id", id)
}

// Resume pins the coop-owned session id rather than "latest" — a loop or consult in the same
// cwd could be the latest, but resuming an explicit uuid is immune. Metadata is matched across
// every project bucket because Gemini's bucket naming has changed between releases.
func (a geminiAgent) Resume(cfg *config.Config, home, ws, id string) ([]string, bool) {
	if ValidSessionID(id) && geminiHasSession(home, ws, id) {
		return append(a.base(cfg), "--resume", id), true
	}
	return a.Interactive(cfg), false
}

const geminiProjectRootLimit = 4 << 10

// geminiHasSession matches both the Coop-owned id and Gemini's native sha256(cwd) projectHash.
// Bucket names vary between Gemini releases, so scan every bucket whose .project_root owns ws.
func geminiHasSession(home, ws, id string) bool {
	wantProject := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
	root, err := openSessionRoot(filepath.Join(home, "tmp"))
	if err != nil {
		return false
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return false
	}
	found := scanSessionDir(dir, func(bucket os.DirEntry) bool {
		if !bucket.IsDir() || bucket.Type()&os.ModeSymlink != 0 {
			return false
		}
		if geminiBucketCWD(root, bucket.Name()) != ws {
			return false
		}
		chats := filepath.Join(bucket.Name(), "chats")
		info, err := root.Lstat(chats)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		chatDir, err := root.Open(chats)
		if err != nil {
			return false
		}
		matched := scanSessionDir(chatDir, func(entry os.DirEntry) bool {
			if !strings.HasSuffix(entry.Name(), ".jsonl") && !strings.HasSuffix(entry.Name(), ".json") {
				return false
			}
			path := filepath.Join(chats, entry.Name())
			info, err := root.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return false
			}
			f, err := root.Open(path)
			if err != nil {
				return false
			}
			sessionID, projectHash := geminiSessionMetadata(f, strings.HasSuffix(entry.Name(), ".json"))
			_ = f.Close()
			return sessionID == id && projectHash == wantProject
		})
		_ = chatDir.Close()
		return matched
	})
	_ = dir.Close()
	return found
}

// geminiBucketCWD reads Gemini's bucket ownership marker. An absent or malformed marker is not
// authoritative and returns empty.
func geminiBucketCWD(root *os.Root, bucket string) string {
	path := filepath.Join(bucket, ".project_root")
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > geminiProjectRootLimit {
		return ""
	}
	f, err := root.Open(path)
	if err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, geminiProjectRootLimit+1))
	_ = f.Close()
	if err != nil || len(data) > geminiProjectRootLimit {
		return ""
	}
	cwd := strings.TrimSpace(string(data))
	if !filepath.IsAbs(cwd) {
		return ""
	}
	return cwd
}

// Ownership strings are bounded; native messages stream without a payload-size limit.
func geminiSessionMetadata(r io.Reader, multiline bool) (sessionID, projectHash string) {
	done := errors.New("native Gemini session metadata read")
	err := nativeHistoryObjects(r, []string{"sessionId", "projectHash"}, multiline,
		func(fields map[string]string, _, _ int64) error {
			sessionID, projectHash = fields["sessionId"], fields["projectHash"]
			return done
		})
	if !errors.Is(err, done) {
		return "", ""
	}
	return sessionID, projectHash
}

func (geminiAgent) Login(*config.Config) []string { return []string{"gemini"} }

func (geminiAgent) LoginConfig(*config.Config) (MCPConfig, error) {
	return MCPConfig{
		CommandArgs: []string{"--extensions", "none"},
		Env:         []string{"GEMINI_CLI_SYSTEM_SETTINGS_PATH=/etc/gemini-cli/login.json", "NO_BROWSER=true"},
	}, nil
}

func (geminiAgent) ConsultCmd(question string) []string {
	// -p takes the prompt as its value, so it must come last (right before the
	// question); otherwise -p swallows --approval-mode and gemini prints help.
	return []string{"gemini", "--approval-mode", "plan", "-p", question}
}

// RestrictedCommand: no restricted mode is qualified on this CLI yet (see unqualifiedRestrictedCommand).
func (a geminiAgent) RestrictedCommand(mode ExecutionMode, cmd []string) ([]string, error) {
	return unqualifiedRestrictedCommand(a, mode, cmd)
}

// ACPRestrictedSessionMeta: nor on its ACP adapter (see unqualifiedRestrictedACPSession).
func (a geminiAgent) ACPRestrictedSessionMeta(mode ExecutionMode) (map[string]any, error) {
	return unqualifiedRestrictedACPSession(a, mode)
}

// geminiNoUpdates is Gemini's update switch, a system-settings block. /etc/gemini-cli/settings.json
// is the pinned 0.62.0's system layer, which overrides both the user's and a workspace's settings
// (it reports a bad file there by path). Pointing GEMINI_CLI_SYSTEM_SETTINGS_PATH at another file
// replaces that layer instead of merging with it, so every system file Coop writes repeats the block.
var geminiNoUpdates = map[string]any{"enableAutoUpdate": false, "enableAutoUpdateNotification": false}

func (geminiAgent) UpdateControls() UpdateControls {
	// Gemini 0.62 requires every system file and ancestor to be root-owned and non-writable
	// by others. Image-owned files satisfy that; host-generated mounts below the user home do not.
	files := []SystemFile{
		{Path: "/etc/gemini-cli/settings.json", Content: geminiSystemSettings(map[string]any{"general": geminiNoUpdates})},
		// An empty system allowlist disables MCP, rather than merging with native servers.
		{Path: "/etc/gemini-cli/login.json", Content: geminiSystemSettings(map[string]any{
			"general": geminiNoUpdates, "mcp": map[string]any{"allowed": []string{}},
			"context":  map[string]any{"fileFiltering": map[string]any{"respectGitIgnore": false}},
			"privacy":  map[string]any{"usageStatisticsEnabled": false},
			"security": map[string]any{"folderTrust": map[string]any{"enabled": false}},
		})},
	}
	for _, effort := range []string{"low", "high"} {
		files = append(files, SystemFile{Path: geminiThinkingDir + "/" + effort + ".json", Content: geminiThinkingSettings(effort)})
	}
	for _, selection := range []string{"oauth-personal", "gemini-api-key"} {
		for _, level := range []string{"default", "low", "high"} {
			effort := level
			if level == "default" {
				effort = ""
			}
			files = append(files, SystemFile{Path: geminiBrokerSettingsDir + "/" + selection + "/" + level + ".json", Content: geminiBrokerSettings(selection, effort)})
		}
	}
	return UpdateControls{Files: files}
}

// Models are common Gemini model ids. Illustrative — any id the CLI accepts works.
func (geminiAgent) Models() []string {
	return []string{"gemini-3.8-flash", "gemini-3.1-pro-preview", "gemini-3.5-flash-lite"}
}

// ExampleModel: the current default tier.
func (geminiAgent) ExampleModel() string { return "gemini-3.8-flash" }

// ModelEnv: the Gemini CLI reads its default model from GEMINI_MODEL; the flag in base()
// covers coop-driven runs, this covers anything that takes no flags.
func (geminiAgent) ModelEnv() string { return "GEMINI_MODEL" }

// Effort/EffortEnv: the Gemini CLI has no reasoning-effort flag or environment variable; it takes
// thinking from image-owned settings, which MCP selects (see geminiThinkingWiring).
func (geminiAgent) Effort() EffortSpec {
	return EffortSpec{Settings: true, Validate: validateGeminiEffort}
}
func (geminiAgent) EffortEnv() string { return "" }

func (geminiAgent) InstructionFile() string { return "GEMINI.md" }

// NativeSubagents: Gemini CLI 0.59 loads local subagents from ~/.gemini/agents/*.md. The format has no
// reasoning effort, so a native Gemini role cannot set one.
func (geminiAgent) NativeSubagents() NativeSubagentSupport {
	return NativeSubagentSupport{HomeDir: ".gemini/agents", Render: renderGeminiSubagent}
}

func renderGeminiSubagent(role NativeSubagent) (filename, content string) {
	return role.Name + ".md", nativeMarkdown(role.Prompt, [2]string{"name", role.Name},
		[2]string{"description", role.Description}, [2]string{"kind", "local"}, [2]string{"model", role.Model})
}

func (geminiAgent) AuthMarker() (file, envKey string) {
	return "gemini-credentials.json", "GEMINI_API_KEY"
}

func (geminiAgent) HostCredential() HostCredentialSpec {
	return HostCredentialSpec{
		Prompt:       "Gemini API key (input hidden): ",
		Instructions: "Create or copy a key at https://aistudio.google.com/apikey",
		File:         "api-key",
		EnvKey:       "GEMINI_API_KEY",
		Activate:     selectGeminiAPIKey,
	}
}

// selectGeminiAPIKey changes only Gemini's selected auth family. The user's other settings and a
// native encrypted credential file are left alone; malformed or linked settings fail closed.
func selectGeminiAPIKey(profileDir string) error {
	path := filepath.Join(profileDir, "settings.json")
	settings, _, err := readJSONDefaults(path)
	if err != nil {
		return fmt.Errorf("read Gemini settings before saving API key: %w", err)
	}
	security, _ := settings["security"].(map[string]any)
	if security == nil {
		security = map[string]any{}
		settings["security"] = security
	}
	auth, _ := security["auth"].(map[string]any)
	if auth == nil {
		auth = map[string]any{}
		security["auth"] = auth
	}
	auth["selectedType"] = "gemini-api-key"
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Gemini settings after selecting API key: %w", err)
	}
	return config.WriteFileAtomic(path, append(encoded, '\n'))
}

// CredentialEnvKeys lists every env var the Gemini CLI reads a key from: GEMINI_API_KEY
// and the GOOGLE_API_KEY it also honors.
func (geminiAgent) CredentialEnvKeys() []string {
	return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
}

func (geminiAgent) CredentialBroker() CredentialBrokerSpec {
	return CredentialBrokerSpec{
		CredentialEnv: "GEMINI_API_KEY",
		BaseURLEnv:    "GOOGLE_GEMINI_BASE_URL",
		Upstream:      "generativelanguage.googleapis.com",
		Header:        "x-goog-api-key",
		Method:        "POST",
		Path:          "/v1beta/models/",
		PathPrefix:    true,
		AllowQuery:    true,
		Port:          443,
	}
}

func (geminiAgent) StoredAPIKey(profileDir string) (bool, error) {
	selected, ok, err := geminiSelectedAuthType(profileDir)
	return ok && selected == "gemini-api-key", err
}

func (geminiAgent) LiveCredentials() LiveCredentialSpec {
	return LiveCredentialSpec{
		Artifacts: []CredentialArtifact{
			// Gemini's keychain is encrypted from host identity and cannot be made portable. Retain
			// it in the integrity allowlist, but return nil so it is never mounted in a live box.
			{Name: "gemini-credentials.json", Primary: true, Project: func([]byte) ([]byte, error) { return nil, nil }},
			{Name: "google_accounts.json", Project: func([]byte) ([]byte, error) { return nil, nil }},
			{Name: "settings.json", Project: func(data []byte) ([]byte, error) {
				return projectJSONLeaf(data, "security", "auth", "selectedType")
			}},
		},
		Portability: func(string, time.Time) CredentialPortability { return CredentialNotPortable },
		// A rejected key comes back as the service's own error inside the pinned CLI's failed result:
		// "[API Error: {… API key not valid …}]".
		AuthSignals: []string{"manual authorization is required", "authentication required", "must specify the gemini_api_key",
			"api key not valid"},
	}
}

// ActiveCredentialEnvKeys grants exactly the key family selected in settings.json. Without a
// marker or selector, Gemini may auto-detect either supported key; a marker without a selector is
// file-backed and receives no env authority.
func (a geminiAgent) ActiveCredentialEnvKeys(profileDir string, markerPresent bool) []string {
	selectedType, ok, err := geminiSelectedAuthType(profileDir)
	if err != nil {
		return nil
	}
	if !ok {
		if markerPresent {
			return nil
		}
		return a.CredentialEnvKeys()
	}
	switch selectedType {
	case "gemini-api-key":
		return []string{"GEMINI_API_KEY"}
	case "vertex-ai":
		return []string{"GOOGLE_API_KEY"}
	}
	return nil
}

// MarkerProvidesSelectedCredential recognizes only Gemini's native OAuth store. Its encrypted
// API-key store is bound to one container hostname and is therefore not runnable in a fresh box;
// Coop's host credential is the portable API-key authority.
func (geminiAgent) MarkerProvidesSelectedCredential(profileDir string) bool {
	selectedType, ok, err := geminiSelectedAuthType(profileDir)
	return err == nil && ok && selectedType == "oauth-personal"
}

func geminiSelectedAuthType(profileDir string) (string, bool, error) {
	data, present, err := readDefaultsFile(filepath.Join(profileDir, "settings.json"))
	if err != nil {
		return "", false, err
	}
	if !present {
		return "", false, nil
	}
	var settings struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", false, err
	}
	return settings.Security.Auth.SelectedType, true, nil
}

func (geminiAgent) StoredCredentialStatus(profileDir string, _ time.Time) StoredCredentialStatus {
	selectedType, ok, err := geminiSelectedAuthType(profileDir)
	if err != nil || !ok {
		return StoredCredentialReauthRequired
	}
	if selectedType == "gemini-api-key" {
		return StoredCredentialReauthRequired
	}
	return StoredCredentialUnknown
}

// MCP builds the settings mounted inside a gemini box: the host settings plus the box-only
// file-filtering override and the managed-client defaults (no auto-update, no update prompt,
// no usage statistics), and shared servers only when MCP is active. The per-effort system files
// come from the image. The host file is never written here; EnsureDefaults owns the one
// host-side change (folder trust).
func (geminiAgent) MCP(cfg *config.Config, _ string) (MCPConfig, error) {
	dir, err := cfg.AgentSettingsDir("gemini")
	if err != nil {
		return MCPConfig{}, err
	}
	gm, requiredEnv, err := mcp.GenerateGemini(cfg.MCPFile, filepath.Join(dir, "settings.json"))
	if err != nil {
		return MCPConfig{}, err
	}
	gm, err = ensureGeminiBoxDefaults(gm, cfg.EvalDisableWebTools)
	if err != nil {
		return MCPConfig{}, err
	}
	env, err := geminiThinkingWiring(cfg)
	if err != nil {
		return MCPConfig{}, err
	}
	mounts := []MCPMount{{Content: gm, BoxPath: cfg.HomeInBox + "/.gemini/settings.json"}}
	return MCPConfig{Mounts: mounts, Env: env, RequiredEnv: requiredEnv}, nil
}

func ensureGeminiBoxDefaults(settingsJSON string, disableWebTools bool) (string, error) {
	settings := map[string]any{}
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		return "", fmt.Errorf("assemble Gemini box defaults: %w", err)
	}
	changed := disableGeminiFolderTrust(settings)
	if disableWebTools {
		tools, ok := settings["tools"].(map[string]any)
		if settings["tools"] != nil && !ok {
			return "", fmt.Errorf("gemini tools settings must be an object")
		}
		if tools == nil {
			tools = map[string]any{}
			settings["tools"] = tools
		}
		exclude, ok := tools["exclude"].([]any)
		if tools["exclude"] != nil && !ok {
			return "", fmt.Errorf("gemini tools.exclude must be an array")
		}
		for _, tool := range []string{"google_web_search", "web_fetch"} {
			if !slices.Contains(exclude, any(tool)) {
				exclude = append(exclude, tool)
			}
		}
		tools["exclude"] = exclude
		changed = true
	}
	if !changed {
		return settingsJSON, nil
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("assemble Gemini box defaults: %w", err)
	}
	return string(append(data, '\n')), nil
}

// ACPMCPServers is nil: this agent's ACP adapter reads the settings.json MCP mounts,
// so passing the servers again would register every one of them twice.
func (geminiAgent) ACPMCPServers(string, func(string) (string, bool)) ([]map[string]any, error) {
	return nil, nil
}

func (geminiAgent) DefaultsPublication(*config.Config) ([]ConfigPublication, error) { return nil, nil }

// EnsureDefaults guarantees a valid settings.json (an empty/missing one makes gemini
// fail at launch) and turns off its folder-trust prompt — the box is the sandbox. An
// existing choice is kept; a non-blank but unparseable file stops launch without being changed.
func (a geminiAgent) EnsureDefaults(cfg *config.Config, _ string) error {
	dir, err := cfg.AgentSettingsDir(a.Name())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create Gemini defaults directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, "settings.json")
	m, blank, err := readJSONDefaults(path)
	if err != nil {
		return err
	}
	if disableGeminiFolderTrust(m) || blank {
		if err := writeJSONFile(path, m, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// disableGeminiFolderTrust sets security.folderTrust.enabled=false unless the user
// already chose a value, reporting whether it changed m.
func disableGeminiFolderTrust(m map[string]any) bool {
	security, _ := m["security"].(map[string]any)
	if security == nil {
		security = map[string]any{}
		m["security"] = security
	}
	ft, _ := security["folderTrust"].(map[string]any)
	if ft == nil {
		ft = map[string]any{}
		security["folderTrust"] = ft
	}
	if _, ok := ft["enabled"]; ok {
		return false // user already chose — respect it
	}
	ft["enabled"] = false
	return true
}

// ACPRateLimitSignals: gemini surfaces a quota hit as the Google API status
// RESOURCE_EXHAUSTED; the value alone is the proof, whatever key carries it.
func (geminiAgent) ACPRateLimitSignals() []ACPSignal {
	return []ACPSignal{{Value: "RESOURCE_EXHAUSTED"}}
}

// ACPSessionSettings: Gemini changes models through session/set_model rather than
// session/set_config_option. Its ACP command also carries the model at launch.
func (geminiAgent) ACPSessionSettings(target Target) []ACPSessionSetting {
	if target.Model == "" {
		return nil
	}
	return []ACPSessionSetting{{Method: ACPSetModel, Value: target.Model}}
}

// BoxEnv: gemini stores everything under its mounted ~/.gemini; the one variable is the
// managed-client telemetry switch (docs/cli/telemetry.md: `GEMINI_TELEMETRY_ENABLED` overrides
// `telemetry.enabled`, default false), pinned explicitly so a box is deterministic whatever the
// host settings say. The update and usage-statistics switches ride the generated settings
// (mcp.GenerateGemini). None of this qualifies gemini for filtered networking.
func (geminiAgent) BoxEnv(string) []string { return []string{"GEMINI_TELEMETRY_ENABLED=false"} }

func (geminiAgent) HomeFallbacks() []HomeFallback { return nil }

// Native 0.59.0 fresh/resume captures (2026-09-13): assistant message deltas are
// reply text, and result.stats.input_tokens already includes cached input.
const geminiConsultText = `gemini_text() {
	jq -ers 'select(all(.[]; type=="object"))
		| select(.[-1].type=="result" and .[-1].status=="success")
		| select([.[] | select(.type=="result")]|length==1)
		| select(all(.[]; .type!="error"))
		| [.[] | select(.type=="message" and .role=="assistant") | .content | select(type=="string")]
		| join("") | select(test("[^[:space:]]"))'
}
gemini_delegate_text() {
	jq --unbuffered -jr '
		if .type=="message" and .role=="assistant" and (.content|type)=="string" then .content
		elif .type=="error" and (.message|type)=="string" then .message, "\n"
		elif .type=="result" then "\n"
		else empty end'
}
gemini_errors() {
	jq -r 'def text: strings | select(test("[^[:space:]]"));
		def message: (.message|text) // (.error|text) // (.error|objects|.message|text);
		select(type=="object")
		| if .type=="error" then (message // "error")
		  elif .type=="result" and .status!="success" then (message // (.status|text))
		  else empty end'
}
`

const geminiConsultUsage = `select(.[-1].type=="result" and .[-1].status=="success")
		| select([.[] | select(.type=="result")]|length==1)
		| .[-1].stats | select(type=="object")
		| {input:.input_tokens, fresh:.input, read:.cached, output:.output_tokens, duration:.duration_ms}
		| select(.input|token) | select(.output|token)
		| if (.fresh|token) then . else del(.fresh) end
		| if (.read|token) then . else del(.read) end
		| if (.duration|token) then . else del(.duration) end`

// geminiEffortEnv hands a consult or delegate call the thinking settings for its own $effort (see
// geminiThinkingWiring). Without one the call keeps the box's.
const geminiEffortEnv = `env ${effort:+GEMINI_CLI_SYSTEM_SETTINGS_PATH="$` + geminiThinkingEnv + `/$effort.json"} `

func (geminiAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$candidate_idfile\"\n" +
		`gemini_run ` + geminiEffortEnv + `gemini --approval-mode plan --session-id "$id" -o stream-json ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) ConsultResume() string {
	return `gemini_run ` + geminiEffortEnv + `gemini --approval-mode plan --resume "$id" -o stream-json ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) DelegateExec() string {
	return geminiEffortEnv + `gemini --yolo ${model:+--model "$model"} -o stream-json -p "$prompt"`
}

func (geminiAgent) UsagePrelude() string {
	return geminiConsultText + consultPeerRowShell("gemini", geminiConsultUsage)
}
func (a geminiAgent) ShellPrelude() string {
	return a.UsagePrelude() + consultCaptureShell("gemini", "Gemini")
}

// LockedClients pins Gemini's bundled CLI once while recording both ways Coop
// launches it. The npm tarball is integrity-locked by package-lock.json; the
// bundle is the executable entry point for ordinary and ACP sessions alike.
func (geminiAgent) LockedClients(platform ClientPlatform) []LockedClient {
	if !platform.valid() {
		return nil
	}
	client := LockedClient{Package: "@google/gemini-cli", Version: "0.62.0", Binary: "gemini",
		Exec:                []string{"/usr/local/bin/node", lockedClientRoot + "/node_modules/@google/gemini-cli/bundle/gemini.js"},
		RequiredExecutables: []LockedExecutable{{Path: lockedClientRoot + "/node_modules/@google/gemini-cli/bundle/gemini.js", Version: "0.62.0"}}}
	cli, acp := client, client
	cli.Client, acp.Client = egress.ClientCLI, egress.ClientACP
	return []LockedClient{cli, acp}
}

func (a geminiAgent) NetworkBundle(input NetworkBundleInput) (egress.Bundle, error) {
	return directNetworkBundle(a.Name(), "api-key", input,
		[]string{"generativelanguage.googleapis.com"},
		[]string{"https://github.com/google-gemini/gemini-cli/blob/v0.62.0/packages/core/src/core/contentGenerator.ts"})
}

// NetworkAuthSelection refuses native OAuth and Vertex until each has its own
// portable authority and endpoint trace. A saved or env-backed AI Studio key is
// the one account family qualified by this release.
func (geminiAgent) NetworkAuthSelection(profileDir string, markerPresent bool) (NetworkAuthSelection, error) {
	selected, ok, err := geminiSelectedAuthType(profileDir)
	if err != nil {
		return NetworkAuthSelection{}, fmt.Errorf("read Gemini authentication selection: %w", err)
	}
	if ok && selected != "gemini-api-key" {
		return NetworkAuthSelection{}, fmt.Errorf("gemini authentication %q is unsupported for restricted networking", selected)
	}
	if markerPresent && !ok {
		return NetworkAuthSelection{}, fmt.Errorf("gemini stored authentication is unsupported for restricted networking")
	}
	return NetworkAuthSelection{AuthMode: "api-key", EnvKey: "GEMINI_API_KEY"}, nil
}

// Gemini has no reasoning-effort flag or variable: the pinned client (0.62.0) takes thinking only
// from its settings. What it does with them was captured at the request level, against a local
// listener (.agent/kb/gemini-effort-thinking-settings.md), and two facts decide the shape here.
//
// Every chat model's settings chain runs through one family base — chat-base-3 for Gemini 3 and
// Gemma, whose thinkingLevel has only LOW and HIGH; chat-base-2.5 for Gemini 2.5, whose
// thinkingBudget is a token count. The model named at launch does not decide which one applies:
// the client remaps names (gemini-3-pro-preview is sent as gemini-3.1-pro-preview, and a 2.5 flash
// request goes to the current GA flash on an API key), routes auto, and falls back on quota. A setting
// keyed on the typed name is silently a no-op; a setting on both bases reaches whichever model is
// actually called.
//
// And since any Gemini target can end up on a Gemini 3 model, an effort has to mean something in
// both families. Low and high do; medium and every other level have no Gemini 3 level and are
// refused before launch rather than rounded.
//
// Each effort is one small system-settings file, chosen per invocation by
// GEMINI_CLI_SYSTEM_SETTINGS_PATH, so a lead and a preset role in one box can think at different
// levels. The client merges system settings over the user's and the project's key by key, so
// nothing of theirs is replaced — though a thinkingConfig they pin on one model is more specific
// than a family base, and still wins.

// geminiThinking is what each Coop effort becomes in each family. The budgets sit inside every 2.5
// model's accepted range (Pro 128-32768, Flash 0-24576, Flash-Lite 512-24576).
var geminiThinking = map[string]struct {
	level  string
	budget int
}{
	"low":  {"LOW", 1024},
	"high": {"HIGH", 24576},
}

// geminiThinkingModels are the names the pinned client runs through a family base: its request
// aliases and every chat model it defines (packages/core/src/config/models.ts,
// defaultModelConfigs.ts). An effort on any other name would reach no request, so it is refused.
var geminiThinkingModels = []string{
	"auto", "pro", "flash", "flash-lite", "auto-gemini-3", "auto-gemini-2.5",
	"gemini-3-pro-preview", "gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools",
	"gemini-3-flash-preview", "gemini-3.5-flash", "gemini-3.8-flash", "gemini-3-flash", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite",
	"gemini-3.1-flash-lite-preview", "gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite",
	"gemma-4-31b-it", "gemma-4-26b-a4b-it",
}

// validateGeminiEffort refuses an effort the pinned client cannot carry for model, naming what
// works. An empty model is the client's default, which resolves to one of the known models.
func validateGeminiEffort(model, effort string) error {
	if _, ok := geminiThinking[effort]; !ok {
		return fmt.Errorf("gemini takes effort low or high, not %q: Gemini 3 has no other thinking level, and any Gemini target can end up on a Gemini 3 model", effort)
	}
	if model != "" && !slices.Contains(geminiThinkingModels, model) {
		return fmt.Errorf("gemini applies effort only to the models Coop maps to a Gemini thinking level (%s); run %s without an effort", strings.Join(geminiThinkingModels, ", "), model)
	}
	return nil
}

// geminiThinkingEnv names the in-box directory holding one settings file per effort, for the
// consult and delegate arms to pick from by their $effort.
const geminiThinkingEnv = "COOP_GEMINI_THINKING"
const geminiThinkingDir = "/etc/gemini-cli/thinking"

// geminiThinkingWiring selects one image-owned system file per call. Consult and delegate
// arms choose again per call, without mutating the account's shared settings.
func geminiThinkingWiring(cfg *config.Config) ([]string, error) {
	env := []string{geminiThinkingEnv + "=" + geminiThinkingDir}
	if effort := cfg.EffortFor("gemini"); effort != "" {
		if _, ok := geminiThinking[effort]; !ok {
			return nil, fmt.Errorf("gemini effort %q has no thinking setting; use low or high", effort)
		}
		env = append(env, "GEMINI_CLI_SYSTEM_SETTINGS_PATH="+geminiThinkingDir+"/"+effort+".json")
	}
	return env, nil
}

// geminiThinkingSettings is one effort's system settings: both family bases at that effort, as
// customAliases so they merge over any the user or project defines, and the update switch this file
// displaces from the image's system layer. gemini-3-flash is the one chat model the pinned client
// gives no alias, so it would inherit neither base; here it gets its family's.
func geminiThinkingSettings(effort string) string {
	thinking := geminiThinking[effort]
	base := func(config map[string]any) map[string]any {
		return map[string]any{"extends": "chat-base", "modelConfig": map[string]any{
			"generateContentConfig": map[string]any{"thinkingConfig": config}}}
	}
	settings := map[string]any{"general": geminiNoUpdates, "modelConfigs": map[string]any{"customAliases": map[string]any{
		"chat-base-3":    base(map[string]any{"thinkingLevel": thinking.level}),
		"chat-base-2.5":  base(map[string]any{"thinkingBudget": thinking.budget}),
		"gemini-3-flash": map[string]any{"extends": "chat-base-3", "modelConfig": map[string]any{"model": "gemini-3-flash"}},
	}}}
	return geminiSystemSettings(settings)
}

func geminiSystemSettings(settings map[string]any) string {
	data, _ := json.MarshalIndent(settings, "", "  ") // internal literal settings: cannot fail
	return string(append(data, '\n'))
}

// Public installed-client identifiers, not account secrets. Renewal remains host-only;
// the caller retains the response before publishing canonical account authority.
const geminiOAuthClientID = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
const geminiOAuthClientSecret = "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"
const geminiOAuthTokenURL = "https://oauth2.googleapis.com/token"

var geminiRenewClient = &http.Client{Timeout: 20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type geminiRenewal struct {
	Credential []byte
	// Received retains bounded native response bytes even when validation or later
	// readiness fails. It is host-only recovery material, never serving authority.
	Received []byte
}

// renewGeminiAuthority renews only plain native OAuth; it never initializes a global
// keychain or encrypted fallback, and does not let native asynchronous writes replace
// the caller's canonical transaction.
func renewGeminiAuthority(ctx context.Context, original []byte, deadline time.Time,
	retainReceived func([]byte) error) (geminiRenewal, error) {
	var document map[string]json.RawMessage
	if len(original) == 0 || len(original) > 1<<20 || json.Unmarshal(original, &document) != nil || document == nil {
		return geminiRenewal{}, errors.New("invalid Gemini OAuth authority")
	}
	var access, refresh string
	var expiry int64
	if json.Unmarshal(document["access_token"], &access) != nil || json.Unmarshal(document["expiry_date"], &expiry) != nil {
		return geminiRenewal{}, errors.New("invalid Gemini OAuth access state")
	}
	if validGeminiGrant(access) && expiry > deadline.UnixMilli() {
		return geminiRenewal{Credential: bytes.Clone(original)}, nil
	}
	if json.Unmarshal(document["refresh_token"], &refresh) != nil || !validGeminiGrant(refresh) {
		return geminiRenewal{}, errors.New("native Gemini OAuth authority needs host sign-in")
	}
	if retainReceived == nil {
		return geminiRenewal{}, errors.New("native Gemini renewal needs durable response custody")
	}
	if !deadline.After(time.Now()) {
		return geminiRenewal{}, errors.New("native Gemini renewal deadline expired")
	}
	values := url.Values{"client_id": {geminiOAuthClientID}, "client_secret": {geminiOAuthClientSecret},
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiOAuthTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return geminiRenewal{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := geminiRenewClient.Do(request)
	if err != nil {
		return geminiRenewal{}, errors.New("native Gemini OAuth renewal request failed")
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	result := geminiRenewal{Received: bytes.Clone(raw[:min(len(raw), 1<<20)])}
	// This happens before identity/readiness validation and ignores cancellation after
	// issuance. Failure is not safe to retry: an old refresh grant may be consumed.
	if len(result.Received) != 0 {
		if err := retainReceived(result.Received); err != nil {
			return result, fmt.Errorf("native Gemini renewal response received but custody unconfirmed: %w", err)
		}
	}
	if readErr != nil || len(raw) == 0 || len(raw) > 1<<20 {
		return result, errors.New("native Gemini OAuth renewal response incomplete; authority is uncertain, do not retry automatically")
	}
	if response.StatusCode != http.StatusOK {
		return result, errors.New("native Gemini OAuth renewal refused")
	}
	var returned map[string]json.RawMessage
	if json.Unmarshal(raw, &returned) != nil || returned == nil {
		return result, errors.New("invalid Gemini OAuth renewal document")
	}
	var nextAccess string
	var expires json.Number
	if json.Unmarshal(returned["access_token"], &nextAccess) != nil || !validGeminiGrant(nextAccess) ||
		json.Unmarshal(returned["expires_in"], &expires) != nil {
		return result, errors.New("native Gemini OAuth renewal returned unusable access")
	}
	seconds, err := strconv.ParseInt(string(expires), 10, 64)
	now := time.Now().UnixMilli()
	if err != nil || seconds <= 0 || seconds > (math.MaxInt64-now)/1000 {
		return result, errors.New("native Gemini OAuth renewal returned invalid expiry")
	}
	for _, field := range []string{"refresh_token", "scope", "token_type", "id_token"} {
		value, present := returned[field]
		if !present {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil || text == "" || strings.ContainsAny(text, "\x00\r\n") {
			return result, errors.New("native Gemini OAuth renewal returned invalid native metadata")
		}
		if field == "refresh_token" && !validGeminiGrant(text) ||
			field == "token_type" && !strings.EqualFold(text, "Bearer") {
			return result, errors.New("native Gemini OAuth renewal returned unsupported native metadata")
		}
		document[field] = bytes.Clone(value)
	}
	document["access_token"], _ = json.Marshal(nextAccess)
	document["expiry_date"], _ = json.Marshal(now + seconds*1000)
	result.Credential, err = json.Marshal(document)
	if err != nil || len(result.Credential) > 1<<20 {
		return result, errors.New("native Gemini OAuth renewal serialization failed")
	}
	result.Credential = append(result.Credential, '\n')
	if now+seconds*1000 <= deadline.UnixMilli() {
		return result, errors.New("native Gemini OAuth renewed; access does not cover the requested deadline")
	}
	return result, nil
}

func validGeminiGrant(value string) bool {
	return len(value) >= 8 && len(value) <= 32<<10 && !strings.ContainsAny(value, "\x00\r\n")
}
