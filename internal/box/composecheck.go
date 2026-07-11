package box

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateComposeFile reports whether the sibling-services compose file at path declares ONLY
// directives that are safe to auto-run on the HOST daemon — nil when safe, else an error naming
// the first offending key/path/value. coop runs this before every `compose up` (EnsureServices),
// so the compose path no longer has to be shadowed read-only in the box: an in-box agent MAY
// author the file, but the host refuses to run anything that reaches outside a repo-scoped,
// loopback-only container. repoRoot bounds bind mounts; path's own dir anchors relative binds.
//
// The allowlist is the STRUCT SHAPE: composeDoc/serviceSpec model exactly the safe subset, and
// the decoder runs with KnownFields(true), so every host-privilege / host-reaching directive
// (privileged, cap_add, devices, security_opt, userns_mode, pid/ipc/network_mode, env_file,
// secrets, configs, build, extends, include, a volume's driver_opts, …) is rejected because it
// is simply absent from the structs — a deny-by-construction that also covers directives compose
// hasn't invented yet. Only three value checks remain for the fields we DO allow: bind sources
// must stay within the repo (symlinks resolved), published ports must bind loopback only, and
// neither may carry a `$` (the file is validated PRE-interpolation, so `${HOME}/.ssh` would read
// in-repo here yet escape once compose expands it).
func ValidateComposeFile(path, repoRoot string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc composeDoc
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // an unknown key (privileged, build, env_file, …) fails the decode
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("not a plain sibling-services compose file (only image/environment/ports/volumes/healthcheck-style keys are allowed): %w", err)
	}
	composeDir := filepath.Dir(path)
	realRepo, err := resolveExisting(repoRoot)
	if err != nil {
		return err
	}
	for name, svc := range doc.Services {
		if strings.TrimSpace(svc.Image) == "" {
			return fmt.Errorf("service %q: an image is required (build: is not allowed — publish a pre-built image)", name)
		}
		for _, p := range svc.Ports {
			if err := checkPort(name, p); err != nil {
				return err
			}
		}
		for _, v := range svc.Volumes {
			if err := checkVolume(name, v, composeDir, realRepo); err != nil {
				return err
			}
		}
	}
	return nil
}

// composeDoc / serviceSpec / volumeDecl model the SAFE subset only. Fields we don't inspect are
// `any` (shape-flexible, inert — no host reach), so a new inert compose feature just needs adding
// here; a DANGEROUS one is rejected by KnownFields for not being present at all. Do not add a
// field without checking it can't reach the host (a path, a socket, a namespace, a privilege).
type composeDoc struct {
	Version  string                 `yaml:"version"` // ignored (compose schema version)
	Name     string                 `yaml:"name"`
	Services map[string]serviceSpec `yaml:"services"`
	Volumes  map[string]volumeDecl  `yaml:"volumes"`
	Networks map[string]networkDecl `yaml:"networks"`
}

type serviceSpec struct {
	Image           string `yaml:"image"`
	Command         any    `yaml:"command"`
	Entrypoint      any    `yaml:"entrypoint"`
	Environment     any    `yaml:"environment"` // inline map or KEY=VAL list; env_file is absent → rejected
	Ports           []any  `yaml:"ports"`       // checked: loopback-only, no `$`
	Volumes         []any  `yaml:"volumes"`     // checked: named volumes, or binds within the repo
	Expose          any    `yaml:"expose"`      // container-network only, never published to the host
	DependsOn       any    `yaml:"depends_on"`
	Healthcheck     any    `yaml:"healthcheck"`
	Restart         string `yaml:"restart"`
	WorkingDir      string `yaml:"working_dir"`
	User            string `yaml:"user"`
	Labels          any    `yaml:"labels"`
	Profiles        any    `yaml:"profiles"`
	StopGracePeriod string `yaml:"stop_grace_period"`
	Tmpfs           any    `yaml:"tmpfs"` // container tmpfs (no host source)
	MemLimit        any    `yaml:"mem_limit"`
	Cpus            any    `yaml:"cpus"`
	Networks        any    `yaml:"networks"`
}

// volumeDecl is a top-level named volume: docker-managed storage (no host path). `external: true`
// reuses an existing named volume. driver/driver_opts is absent on purpose — `driver_opts: {type:
// none, o: bind, device: /host}` is a bind mount in disguise, so KnownFields rejects it.
type volumeDecl struct {
	External bool   `yaml:"external"`
	Name     string `yaml:"name"`
}

// networkDecl is a top-level project-local network. driver/external absent → a `driver: host`
// (host networking) or an external network is rejected by KnownFields.
type networkDecl struct {
	Labels any `yaml:"labels"`
}

// checkVolume rejects a service volume that binds a host path outside the repo. A bare
// `name:/target` (or an anonymous `/target`) is a docker-managed named volume — always safe; a
// source that looks like a path is a bind and must resolve within repoRoot. Both the short string
// form and the long `{type,source,target}` form are handled.
func checkVolume(svc string, entry any, composeDir, realRepo string) error {
	var source string
	switch v := entry.(type) {
	case string:
		// "source:target[:mode]" — but an anonymous volume is just "/data" (one field, no source).
		parts := strings.SplitN(v, ":", 2)
		if len(parts) < 2 {
			return nil // anonymous volume, no host source
		}
		source = parts[0]
	case map[string]any:
		if t, _ := v["type"].(string); t != "" && t != "bind" {
			return nil // volume/tmpfs/npipe — no host bind source
		}
		source, _ = v["source"].(string)
		if source == "" {
			return nil
		}
	default:
		return fmt.Errorf("service %q: unrecognized volume entry %v", svc, entry)
	}
	if !looksLikePath(source) {
		return nil // a named volume token (e.g. "pgdata"), not a host bind
	}
	return checkBindSource(svc, source, composeDir, realRepo)
}

// looksLikePath reports whether a volume source is a host bind (a path) rather than a named-volume
// token. Docker treats a source containing a path separator or starting with `.`/`~` as a bind;
// a bare token (`pgdata`) is a named volume.
func looksLikePath(source string) bool {
	return strings.ContainsAny(source, "/\\") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~")
}

// checkBindSource requires a bind source to resolve strictly within repoRoot. It rejects `$`
// (the file is validated before compose interpolates, so `${HOME}/.ssh` would look in-repo here
// but escape at run time), `~` (a home-relative path), then joins a relative source onto the
// compose file's dir and resolves symlinks on the longest existing prefix — so a symlinked
// ancestor pointing outside the repo, an absolute path, or a `..`-escape all fail containment.
func checkBindSource(svc, source, composeDir, realRepo string) error {
	if strings.Contains(source, "$") {
		return fmt.Errorf("service %q: bind mount %q uses variable interpolation, which can escape the repo — not allowed", svc, source)
	}
	if strings.HasPrefix(source, "~") {
		return fmt.Errorf("service %q: bind mount %q is home-relative — only repo-relative bind mounts are allowed", svc, source)
	}
	abs := source
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(composeDir, source)
	}
	real, err := resolveExisting(abs)
	if err != nil {
		return fmt.Errorf("service %q: bind mount %q: %v", svc, source, err)
	}
	if real != realRepo && !strings.HasPrefix(real, realRepo+string(filepath.Separator)) {
		return fmt.Errorf("service %q: bind mount %q resolves outside the repo (%s) — only paths within the repo may be mounted", svc, source, realRepo)
	}
	return nil
}

// checkPort rejects a published port bound to any host interface other than loopback (D2): a bare
// "5432:5432" binds 0.0.0.0 (LAN-exposed), so a host port must name 127.0.0.1/localhost/::1
// explicitly. A single-field "5432" (container-only, no host publish) and a `$`-free long form are
// checked the same way. `$` is rejected for the same pre-interpolation reason as bind sources.
func checkPort(svc string, entry any) error {
	switch v := entry.(type) {
	case string:
		return checkPortSpec(svc, v)
	case int:
		return nil // "5432" as a bare int — container port only, not published to the host
	case map[string]any:
		hostIP, _ := v["host_ip"].(string)
		if v["published"] == nil {
			return nil // no published port — container-network only
		}
		if hostIP == "" {
			return fmt.Errorf("service %q: port publishes to all interfaces — set host_ip: 127.0.0.1 to bind loopback only", svc)
		}
		if !isLoopback(hostIP) {
			return fmt.Errorf("service %q: port host_ip %q is not loopback — only 127.0.0.1/localhost/::1 may be published", svc, hostIP)
		}
		return nil
	default:
		return nil
	}
}

func checkPortSpec(svc, spec string) error {
	if strings.Contains(spec, "$") {
		return fmt.Errorf("service %q: port %q uses variable interpolation — not allowed", svc, spec)
	}
	spec = strings.TrimSuffix(spec, "/tcp")
	spec = strings.TrimSuffix(spec, "/udp")
	// IPv6 host_ip is bracketed: "[::1]:5432:5432". Peel the bracket, then the rest splits on ':'.
	if strings.HasPrefix(spec, "[") {
		end := strings.Index(spec, "]")
		if end < 0 {
			return fmt.Errorf("service %q: malformed port %q", svc, spec)
		}
		if !isLoopback(spec[1:end]) {
			return fmt.Errorf("service %q: port %q binds a non-loopback address — publish to 127.0.0.1/::1 only", svc, spec)
		}
		return nil
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		return nil // "5432" — container port only, not published to the host
	case 2:
		// "hostPort:containerPort" — no host_ip means docker binds 0.0.0.0 (LAN-exposed).
		return fmt.Errorf("service %q: port %q publishes to all interfaces — write \"127.0.0.1:%s\" to bind loopback only", svc, spec, spec)
	default:
		if !isLoopback(parts[0]) {
			return fmt.Errorf("service %q: port %q binds a non-loopback address %q — publish to 127.0.0.1/localhost only", svc, spec, parts[0])
		}
		return nil
	}
}

func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// resolveExisting returns the absolute, symlink-resolved form of p. p need not exist yet (a bind
// source can be created by compose): it resolves symlinks on the longest existing ANCESTOR and
// re-appends the missing tail, so a symlinked directory in the path can't disguise an escape.
func resolveExisting(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(abs)
		if parent == abs { // reached the root without an existing prefix
			return filepath.Join(abs, rest), nil
		}
		rest = filepath.Join(filepath.Base(abs), rest)
		abs = parent
	}
}
