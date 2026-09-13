package agent

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/egress"
)

//go:embed locked-clients/package.json locked-clients/package-lock.json
var lockedClientFiles embed.FS

const lockedClientRoot = "/opt/coop/clients"

type ClientPlatform struct{ OS, Architecture, Libc string }

func (p ClientPlatform) valid() bool {
	return p.OS == "linux" && p.Libc == "glibc" && (p.Architecture == "amd64" || p.Architecture == "arm64")
}

// LockedClient is adapter-owned construction data. Exec is an absolute argv
// prefix; callers append native arguments without shell interpretation.
// Provider is stamped by the closure from the registry, never by the adapter:
// a build cannot claim to belong to another agent.
type LockedClient struct {
	Provider                 string
	Client                   egress.Client
	Package, Version, Binary string
	Exec, UnsetEnv           []string
	RequiredExecutables      []LockedExecutable
	NativeArtifact           *LockedNativeArtifact
}

// Version is the exact npm package version, not the adapter or engine version.
// An ACP adapter can carry a differently versioned native SDK.
type LockedExecutable struct{ Path, Version string }

// LockedNativeArtifact is the non-npm arm of the same closed client supply chain.
// The adapter owns an immutable vendor URL and Coop owns the expected digest; the
// image verifies the downloaded bytes before decompressing or executing them.
type LockedNativeArtifact struct {
	URL, SHA256, Destination string
}

func (c LockedClient) Launcher() string { return "/usr/local/bin/" + c.Binary }

type ClientClosure struct {
	Platform ClientPlatform
	Digest   string
	Clients  []LockedClient
	Files    map[string][]byte
}

var lockedVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// LockedClientClosure returns fresh embedded-only inputs. No repository, npm
// configuration, caller package list or runtime-installed shim can alter them.
func LockedClientClosure(platform ClientPlatform) (ClientClosure, error) {
	if !platform.valid() {
		return ClientClosure{}, errors.New("locked clients require Linux amd64/arm64 with glibc")
	}
	files := make(map[string][]byte)
	for _, name := range []string{"package.json", "package-lock.json"} {
		data, err := lockedClientFiles.ReadFile("locked-clients/" + name)
		if err != nil {
			return ClientClosure{}, err
		}
		files[name] = data
	}
	var clients []LockedClient
	for _, name := range Names() {
		for _, client := range registry[name].LockedClients(platform) {
			client.Provider = name
			clients = append(clients, client)
		}
	}
	if err := validateClientClosure(platform, files, clients); err != nil {
		return ClientClosure{}, err
	}
	for _, client := range clients {
		// Node interpolation and executable overrides must not change the
		// Coop-launched client's identity. This is not an in-box code/DLP policy.
		script := "#!/bin/sh\nset -eu\nunset NODE_OPTIONS NODE_PATH"
		for _, key := range client.UnsetEnv {
			script += " " + key
		}
		script += "\nexec"
		for _, arg := range client.Exec {
			script += " '" + arg + "'"
		}
		script += " \"$@\"\n"
		files["launchers/"+client.Binary] = []byte(script)
	}
	// JSON maps sort keys, so definitions, platform and bytes have one stable
	// identity. The image ID remains the final construction observation.
	identity, err := json.Marshal(struct {
		Platform ClientPlatform
		Clients  []LockedClient
		Files    map[string][]byte
	}{platform, clients, files})
	if err != nil {
		return ClientClosure{}, err
	}
	digest := sha256.Sum256(identity)
	return ClientClosure{Platform: platform, Digest: hex.EncodeToString(digest[:]), Clients: clients, Files: files}, nil
}

func validateClientClosure(platform ClientPlatform, files map[string][]byte, clients []LockedClient) error {
	invalid := errors.New("embedded locked client manifest, lock or adapter declaration is inconsistent")
	var manifest struct {
		Private      bool
		Dependencies map[string]string
	}
	var lock struct {
		LockfileVersion int
		Packages        map[string]struct {
			Version, Resolved, Integrity string
			Link                         bool
			Dependencies                 map[string]string
			Bin                          map[string]string
		}
	}
	if len(files["package-lock.json"]) > 1<<20 || json.Unmarshal(files["package.json"], &manifest) != nil || !manifest.Private || json.Unmarshal(files["package-lock.json"], &lock) != nil || lock.LockfileVersion != 3 || len(lock.Packages) > 4096 || !maps.Equal(manifest.Dependencies, lock.Packages[""].Dependencies) {
		return invalid
	}
	expected := map[string]string{"playwright": "1.63.0"}
	binaries := make(map[string]LockedClient)
	packages := make(map[string]LockedClient)
	variants := make(map[string]bool)
	for _, client := range clients {
		if client.Provider == "" || (client.Client != egress.ClientCLI && client.Client != egress.ClientACP) || !lockedVersion.MatchString(client.Version) || client.Binary == "" || strings.Trim(client.Binary, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || len(client.Exec) < 1 || len(client.Exec) > 2 {
			return invalid
		}
		if prior, ok := binaries[client.Binary]; ok {
			left, right := prior, client
			left.Client, right.Client = "", ""
			if !reflect.DeepEqual(left, right) {
				return invalid
			}
		} else {
			binaries[client.Binary] = client
		}
		variant := client.Provider + "\x00" + string(client.Client)
		if variants[variant] {
			return invalid
		}
		variants[variant] = true
		if client.NativeArtifact == nil {
			if client.Package == "" || lock.Packages["node_modules/"+client.Package].Bin[client.Binary] == "" {
				return invalid
			}
			if prior, ok := packages[client.Package]; ok {
				left, right := prior, client
				left.Client, right.Client = "", ""
				if !reflect.DeepEqual(left, right) {
					return invalid
				}
			} else {
				packages[client.Package] = client
			}
			if prior, ok := expected[client.Package]; ok && prior != client.Version {
				return invalid
			}
			expected[client.Package] = client.Version
		} else if client.Package != "" || !validLockedNativeArtifact(*client.NativeArtifact, client, platform) {
			return invalid
		}
		for i, arg := range client.Exec {
			allowed := strings.HasPrefix(arg, lockedClientRoot+"/node_modules/") || client.NativeArtifact != nil && arg == client.NativeArtifact.Destination || i == 0 && len(client.Exec) == 2 && arg == "/usr/local/bin/node"
			if path.Clean(arg) != arg || strings.Trim(arg, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/_@.-") != "" || !allowed {
				return invalid
			}
			if client.NativeArtifact == nil && arg != "/usr/local/bin/node" && lock.Packages[lockedExecutablePackage(arg)].Version == "" {
				return invalid
			}
			if client.NativeArtifact == nil && len(client.Exec) == 2 && i == 1 && lockedExecutablePackage(arg) != "node_modules/"+client.Package {
				return invalid
			}
		}
		for _, key := range client.UnsetEnv {
			if key == "" || strings.Trim(key, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789") != "" {
				return invalid
			}
		}
		if len(client.RequiredExecutables) == 0 || len(client.RequiredExecutables) > 8 {
			return invalid
		}
		for _, executable := range client.RequiredExecutables {
			validPath := strings.HasPrefix(executable.Path, lockedClientRoot+"/node_modules/")
			validVersion := lock.Packages[lockedExecutablePackage(executable.Path)].Version == executable.Version
			if client.NativeArtifact != nil {
				validPath = executable.Path == client.NativeArtifact.Destination
				validVersion = executable.Version == client.Version
			}
			if path.Clean(executable.Path) != executable.Path || !validPath || strings.Trim(executable.Path, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/_@.-") != "" || !lockedVersion.MatchString(executable.Version) || !validVersion {
				return invalid
			}
		}
	}
	if !maps.Equal(expected, manifest.Dependencies) {
		return invalid
	}
	for name, item := range lock.Packages {
		if name == "" {
			continue
		}
		if path.Clean(name) != name || !strings.HasPrefix(name, "node_modules/") || item.Link || !lockedVersion.MatchString(item.Version) {
			return invalid
		}
		u, err := url.Parse(item.Resolved)
		if err != nil || u.Scheme != "https" || u.Host != "registry.npmjs.org" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, ".tgz") {
			return invalid
		}
		algorithm, encoded, ok := strings.Cut(item.Integrity, "-")
		hash, err := base64.StdEncoding.DecodeString(encoded)
		if !ok || algorithm != "sha512" || err != nil || len(hash) != 64 {
			return invalid
		}
	}
	for name, version := range expected {
		if lock.Packages["node_modules/"+name].Version != version {
			return invalid
		}
	}
	return nil
}

func validLockedNativeArtifact(artifact LockedNativeArtifact, client LockedClient, platform ClientPlatform) bool {
	u, err := url.Parse(artifact.URL)
	arch := "aarch64"
	if platform.Architecture == "amd64" {
		arch = "x86_64"
	}
	wantPath := "/grok-build-public-artifacts/cli/grok-" + client.Version + "-linux-" + arch + ".gz"
	if err != nil || u.Scheme != "https" || u.Host != "storage.googleapis.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != wantPath ||
		strings.Trim(u.Path, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/_.-") != "" {
		return false
	}
	if len(artifact.SHA256) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil {
		return false
	}
	return artifact.Destination == lockedClientRoot+"/native/"+client.Binary && len(client.Exec) == 1 && client.Exec[0] == artifact.Destination
}

func lockedExecutablePackage(executable string) string {
	rel, ok := strings.CutPrefix(executable, lockedClientRoot+"/")
	if !ok {
		return ""
	}
	start := strings.LastIndex("/"+rel, "/node_modules/")
	if start < 0 {
		return ""
	}
	start += len("node_modules/")
	parts := strings.Split(rel[start:], "/")
	count := 1
	if strings.HasPrefix(parts[0], "@") {
		count = 2
	}
	if len(parts) <= count {
		return ""
	}
	return rel[:start] + strings.Join(parts[:count], "/")
}

// FileNames returns paths in archive order, useful for deterministic
// build contexts. Files and Clients are fresh per closure, never mutable globals.
func (c ClientClosure) FileNames() []string { return slices.Sorted(maps.Keys(c.Files)) }
