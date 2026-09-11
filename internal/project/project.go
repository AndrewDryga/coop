// Package project reads a repo's .agent/project.yaml — coop's per-project config, committed with the
// repo (unlike the git-ignored rest of .agent/). It carries:
//
//   - subprojects: for a monorepo, the member project dirs whose .agent/tasks queues coop aggregates
//     automatically, so you don't hand-maintain COOP_TASKS.
//   - serve.ports: container ports coop publishes so a dev server running in the box is reachable from
//     the host browser, each mapped to a stable host port.
//   - box: the committed box policy and literal environment defaults every run in this repo
//     inherits — applied below explicit user/runtime settings (box.Run overlays it).
//   - gate: the revalidation command `coop fork merge` runs in the box (an explicit COOP_GATE wins).
//
// SECURITY: this file is committed and read on the HOST from a repo you may not fully trust, so it
// must never be able to LOOSEN the user's posture. The precedence (explicit env/conf > this file >
// built-in default) makes egress tighten-only by construction — its built-in default is already the
// loosest value ("open") — and no_new_privileges is deliberately NOT a key here (its default is on;
// a committed switch could only turn it off). box.env reaches only the container, rejects Coop's
// reserved COOP_ namespace, and stays below the user's agents/env and runtime-generated values.
package project

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/egress"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// File is the repo-relative path of the project config.
const File = ".agent/project.yaml"

// Bound hostile repository input before YAML allocation or decoding.
const maxProjectBytes = 1 << 20

// Default box-input paths when project.yaml doesn't set box.dockerfile / box.compose. Both live
// under .agent/ (coop's committed home) — the Dockerfile alongside the sidecar compose file.
const (
	DefaultDockerfile = ".agent/Dockerfile"
	DefaultCompose    = ".agent/compose.yml"
)

// The host-port window: high, unprivileged, and away from the common dev ports (3000/5173/8080) so a
// coop-assigned host port rarely clashes with a service you're already running.
const (
	hostPortBase = 20000
	hostPortSpan = 40000 // → [20000, 60000)
)

// Project is the parsed .agent/project.yaml.
type Project struct {
	Subprojects []string `yaml:"subprojects"` // monorepo member dirs (repo-relative), each its own coop project
	Serve       Serve    `yaml:"serve"`
	Box         Box      `yaml:"box"`      // committed box policy (below an explicit COOP_* setting)
	Review      Review   `yaml:"review"`   // publication-review-only services and literal environment
	Context     Context  `yaml:"context"`  // path-routed instruction/rule/KB compilation (coop context)
	Services    Services `yaml:"services"` // what the sibling services ASK for; grants nothing by itself
	Gate        string   `yaml:"gate"`     // fork-merge revalidation command (an explicit COOP_GATE wins)
}

// Services is what the repo asks for on behalf of its sibling services. It is a REQUEST, never a
// grant: this file is committed and an agent in the box can edit it, so the human still approves
// once per machine (`coop up`, recorded outside every repo). Its whole job is to make the ask
// documented, travel with the repo, and let a file nobody asked for stand out at the prompt.
type Services struct {
	// RequireRealFiles are repo-relative paths whose real contents the services need — a generated
	// dev TLS key a local Keycloak reads, say — rather than the empty stand-in coop mounts over
	// anything that looks like a secret.
	RequireRealFiles []string `yaml:"require_real_files"`
}

// Context is the path-routed context configuration: which committed docs to compile for a given
// scope (touched paths). See internal/contextc for the compiler; canonical AGENTS.md/CLAUDE.md are
// always included regardless of routes.
type Context struct {
	Routes []Route `yaml:"routes"`
}

// Route includes its Docs whenever any of its Paths globs matches a scope path. Both are
// repo-relative (validated in Load); Paths support `*` within a segment and `**` across segments.
type Route struct {
	Paths   []string `yaml:"paths"`   // repo-relative globs (e.g. "portal/**", "**/*.ex")
	Include []string `yaml:"include"` // repo-relative docs to compile when a path matches
}

// Serve is the serving config: container ports to publish.
type Serve struct {
	Ports []int `yaml:"ports"` // what your dev server listens on inside the box
}

// Box is the committed per-repo box policy. Every field is optional; an unset field keeps the
// user's own setting (env/conf) or coop's built-in default. The booleans are pointers because
// absent must stay distinguishable from false (their defaults are true).
type Box struct {
	Dockerfile string            `yaml:"dockerfile"` // box image definition, repo-relative ("" ⇒ .agent/Dockerfile)
	Compose    string            `yaml:"compose"`    // sidecar services compose file, repo-relative ("" ⇒ .agent/compose.yml)
	Env        map[string]string `yaml:"env"`        // literal box-only environment defaults

	Egress      string        `yaml:"egress"`       // "" (unset) | "open" | "filtered" | "none" — the file spells none "offline"
	EgressRules []egress.Rule `yaml:"egress_rules"` // requests only; host approval supplies authority
	AutoUp      *bool         `yaml:"auto_up"`      // auto-start .agent/compose.yml services (default true)
	Network     *bool         `yaml:"network"`      // join the sibling-services network (default true)
	Memory      string        `yaml:"memory"`       // docker --memory syntax, passed through (e.g. 4g)
	CPUs        string        `yaml:"cpus"`         // docker --cpus value
	Pids        string        `yaml:"pids"`         // --pids-limit: a positive integer, or ""/0/unlimited for none
}

// Review is the trusted, committed environment used only for a disposable review candidate.
// It lets a repository use a minimal CI-like dependency stack instead of copying ignored local
// development state into the scratch clone.
type Review struct {
	Compose string            `yaml:"compose"`
	Env     map[string]string `yaml:"env"`
}

// Load reads <repo>/.agent/project.yaml. A missing file is not an error — it returns an empty Project,
// the common single-repo case. A present-but-invalid file (bad YAML, an unknown key, an out-of-range
// port, a bad box value, or a subproject path that escapes the repo) IS an error, so a typo surfaces
// instead of silently doing nothing. Subproject paths are cleaned in place.
// FileError is a refusal to read the project file, kept as the sentence that says what is wrong
// with it. The file's name is not in it — every one of these is about File — and the block a person
// sees is built by the CLI, which owns the terminal (internal/importdag_test.go).
type FileError struct{ Cause string }

func (e *FileError) Error() string { return "read " + File + ": " + e.Cause }

func readError(cause string) error { return &FileError{Cause: cause} }

// fileReason turns a filesystem error into the one sentence a person can act on: the bare reason
// ("Permission denied."), without the operation and path the headline already carries.
func fileReason(err error) string {
	var perr *os.PathError
	if errors.As(err, &perr) {
		err = perr.Err
	}
	msg := err.Error()
	if msg == "" {
		return "It could not be read."
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

func Load(repo string) (*Project, error) {
	path := filepath.Join(repo, filepath.FromSlash(File))
	agentDir := filepath.Dir(path)
	dirInfo, err := os.Lstat(agentDir)
	if errors.Is(err, os.ErrNotExist) {
		return &Project{}, nil
	}
	if err != nil {
		return nil, readError(fileReason(err))
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return nil, readError("Its " + filepath.Base(agentDir) + " folder is a symbolic link. Coop reads project config without following links.")
	}
	if !dirInfo.IsDir() {
		return nil, readError("Its " + filepath.Base(agentDir) + " path is not a folder.")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Project{}, nil
	}
	if err != nil {
		return nil, readError(fileReason(err))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, readError("The file is a symbolic link. Coop requires a regular project file.")
	}
	if !info.Mode().IsRegular() {
		return nil, readError("The path is not a regular file. Coop requires a regular project file.")
	}
	data, err := readProjectFile(path, dirInfo, info)
	if err != nil {
		return nil, readError(fileReason(err))
	}
	return Parse(data)
}

func readProjectFile(path string, parent, before os.FileInfo) ([]byte, error) {
	dir, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() || !os.SameFile(parent, info) {
		return nil, errors.New("project config parent changed during capture")
	}
	// Open relative to the pinned directory, not a path an agent can replace
	// after the metadata checks. NONBLOCK also makes a FIFO swap fail promptly.
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) || info.Size() > maxProjectBytes {
		return nil, errors.New("project config changed or exceeds its 1 MiB limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxProjectBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxProjectBytes {
		return nil, errors.New("project config exceeds its 1 MiB limit")
	}
	return data, nil
}

// Parse validates one exact project.yaml byte sequence. Load owns filesystem policy; callers that
// already hold a checked file descriptor use Parse so validation cannot race a second path read.
func Parse(data []byte) (*Project, error) {
	var p Project
	var err error
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)                                             // an unknown key is a typo doing nothing — fail loudly instead
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) { // EOF = an all-comments/empty file
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("%s: %w", File, err)
		}
		return nil, fmt.Errorf("%s must not contain more than one YAML document", File)
	}
	for _, port := range p.Serve.Ports {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("%s: serve port %d out of range (1-65535)", File, port)
		}
	}
	for i, sub := range p.Subprojects {
		clean := filepath.Clean(sub)
		if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s: subproject %q must be a relative path inside the repo", File, sub)
		}
		p.Subprojects[i] = clean
	}
	for i, path := range p.Services.RequireRealFiles {
		clean := filepath.Clean(path)
		if path == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s: services.require_real_files %q must be a relative path inside the repo", File, path)
		}
		p.Services.RequireRealFiles[i] = filepath.ToSlash(clean)
	}
	// The file says "offline" — the human word — and the rest of coop keeps its
	// internal "none". This is the one place the two meet; there is no alias.
	switch p.Box.Egress {
	case "", "open", "filtered":
	case "offline":
		p.Box.Egress = "none"
	default:
		return nil, fmt.Errorf("%s: box.egress %q — use filtered, offline or open", File, p.Box.Egress)
	}
	if len(p.Box.EgressRules) > 0 {
		if p.Box.Egress != "" && p.Box.Egress != "filtered" {
			return nil, fmt.Errorf("%s: box.egress_rules require filtered mode", File)
		}
		p.Box.EgressRules, err = egress.NormalizeRules(p.Box.EgressRules)
		if err != nil {
			return nil, fmt.Errorf("%s: box.egress_rules: %w", File, err)
		}
	}
	switch p.Box.Pids {
	case "", "0", "unlimited":
	default:
		if n, err := strconv.Atoi(p.Box.Pids); err != nil || n < 1 {
			return nil, fmt.Errorf("%s: box.pids %q — use a positive integer, or 0/unlimited for no cap", File, p.Box.Pids)
		}
	}
	if p.Box.Dockerfile, err = boxRelPath("dockerfile", p.Box.Dockerfile); err != nil {
		return nil, err
	}
	if p.Box.Compose, err = boxRelPath("compose", p.Box.Compose); err != nil {
		return nil, err
	}
	if p.Review.Compose, err = reviewRelPath("compose", p.Review.Compose); err != nil {
		return nil, err
	}
	if err := validateLiteralEnv("box.env", p.Box.Env); err != nil {
		return nil, err
	}
	if err := validateLiteralEnv("review.env", p.Review.Env); err != nil {
		return nil, err
	}
	// context.routes: every include doc and match glob must be a relative path inside the repo — the
	// compiler reads committed files on the host from a possibly-untrusted repo, so a route can never
	// point outside it (a missing include is caught at compile time, when it's actually selected).
	for i := range p.Context.Routes {
		r := &p.Context.Routes[i]
		for j, inc := range r.Include {
			clean := filepath.Clean(inc)
			if inc == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s: context.routes[%d].include %q must be a relative path inside the repo", File, i, inc)
			}
			r.Include[j] = clean
		}
		for _, g := range r.Paths {
			if g == "" || filepath.IsAbs(g) || g == ".." || strings.HasPrefix(g, "../") {
				return nil, fmt.Errorf("%s: context.routes[%d].paths %q must be a non-empty relative glob inside the repo", File, i, g)
			}
		}
	}
	return &p, nil
}

func validateLiteralEnv(field string, values map[string]string) error {
	for key, value := range values {
		switch {
		case !validEnvName(key):
			return fmt.Errorf("%s: %s key %q must be a POSIX environment name", File, field, key)
		case strings.HasPrefix(key, "COOP_"):
			return fmt.Errorf("%s: %s key %q uses Coop's reserved COOP_ namespace", File, field, key)
		case strings.ContainsAny(value, "\r\n\x00"):
			return fmt.Errorf("%s: %s value for %s must be a single literal line", File, field, key)
		}
	}
	return nil
}

func validEnvName(key string) bool {
	if key == "" || !isEnvNameStart(key[0]) {
		return false
	}
	for i := 1; i < len(key); i++ {
		if !isEnvNameStart(key[i]) && (key[i] < '0' || key[i] > '9') {
			return false
		}
	}
	return true
}

func isEnvNameStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// boxRelPath validates an optional repo-relative box path (box.dockerfile / box.compose): empty
// stays empty (the default applies later); otherwise it must be a relative path inside the repo,
// returned cleaned — the same tighten-only rule as subprojects, so a committed file can't point
// coop's build/compose at something outside the repo.
func boxRelPath(field, val string) (string, error) {
	if val == "" {
		return "", nil
	}
	clean := filepath.Clean(val)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: box.%s %q must be a relative path inside the repo", File, field, val)
	}
	return clean, nil
}

func reviewRelPath(field, val string) (string, error) {
	if val == "" {
		return "", nil
	}
	clean := filepath.Clean(val)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: review.%s %q must be a relative path inside the repo", File, field, val)
	}
	return clean, nil
}

// DockerfileRel / ComposeRel project the box-input paths off a loaded Project: the configured
// value, else the default. Callers that already hold a *Project (e.g. Build, which Loads to fail
// loudly on a bad file) use these; the package-level helpers below are for the path-check sites.
func (p *Project) DockerfileRel() string {
	if p.Box.Dockerfile != "" {
		return p.Box.Dockerfile
	}
	return DefaultDockerfile
}

func (p *Project) ComposeRel() string {
	if p.Box.Compose != "" {
		return p.Box.Compose
	}
	return DefaultCompose
}

// DockerfilePath returns the repo-relative path to the box's Dockerfile — box.dockerfile from
// project.yaml, else DefaultDockerfile. Best-effort: a project.yaml that won't load (which the
// build/run paths surface loudly on their own Load) falls back to the default here rather than
// erroring, so path-existence checks stay total.
func DockerfilePath(repo string) string {
	p, err := Load(repo)
	if err != nil {
		return DefaultDockerfile
	}
	return p.DockerfileRel()
}

// ComposePath returns the repo-relative path to the sidecar compose file — box.compose from
// project.yaml, else DefaultCompose. Best-effort, like DockerfilePath.
func ComposePath(repo string) string {
	p, err := Load(repo)
	if err != nil {
		return DefaultCompose
	}
	return p.ComposeRel()
}

// HostPort maps a repo + container port to a host port deterministically: the same project always
// publishes to the same host port (bookmarkable, and stable across box restarts), while two different
// projects almost never collide. Deterministic rather than first-come so the URL reporter and the box
// itself agree on the mapping without having to coordinate.
func HostPort(repo string, port int) int {
	return HostPortFor(repo, strconv.Itoa(port))
}

// HostPortFor maps a repo + an arbitrary key to a host port deterministically — the same key always
// publishes to the same host port, two different keys almost never collide. HostPort is the
// port-only case (key = the port); sidecars key on "<service>:<port>" so two services sharing a
// container port — or a sidecar port equal to a serve.port — still get distinct host ports.
func HostPortFor(repo, key string) int {
	return hostPortBase + int(crc32.ChecksumIEEE([]byte(repo+":"+key))%hostPortSpan)
}

// TaskDirs returns the repo-relative .agent/tasks queue(s) for repo, aggregating a monorepo's
// subprojects so no one has to hand-maintain COOP_TASKS. A single repo (no subprojects) yields just
// ".agent/tasks". A monorepo yields each subproject's queue, plus the root's own if it has one. A
// missing project.yaml falls back to ".agent/tasks"; an invalid one returns the error.
func TaskDirs(repo string) ([]string, error) {
	p, err := Load(repo)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(".agent", "tasks")
	if len(p.Subprojects) == 0 {
		return []string{root}, nil
	}
	var dirs []string
	if isDir(filepath.Join(repo, root)) {
		dirs = append(dirs, root) // the root can carry its own queue alongside the members'
	}
	for _, sub := range p.Subprojects {
		dirs = append(dirs, filepath.Join(sub, ".agent", "tasks"))
	}
	return dirs, nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
