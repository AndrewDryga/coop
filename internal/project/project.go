// Package project reads a repo's .agent/project.yaml — coop's per-project config, committed with the
// repo (unlike the git-ignored rest of .agent/). It carries:
//
//   - subprojects: for a monorepo, the member project dirs whose .agent/tasks queues coop aggregates
//     automatically, so you don't hand-maintain COOP_TASKS.
//   - serve.ports: container ports coop publishes so a dev server running in the box is reachable from
//     the host browser, each mapped to a stable host port.
//   - box: the committed box policy every run in this repo inherits (egress, services toggles,
//     resource caps) — applied below an explicit COOP_* env/conf setting (box.Run overlays it).
//   - gate: the revalidation command `coop fork merge` runs in the box (an explicit COOP_GATE wins).
//
// SECURITY: this file is committed and read on the HOST from a repo you may not fully trust, so it
// must never be able to LOOSEN the user's posture. The precedence (explicit env/conf > this file >
// built-in default) makes egress tighten-only by construction — its built-in default is already the
// loosest value ("open") — and no_new_privileges is deliberately NOT a key here (its default is on;
// a committed switch could only turn it off).
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

	"gopkg.in/yaml.v3"
)

// File is the repo-relative path of the project config.
const File = ".agent/project.yaml"

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
	Box         Box      `yaml:"box"`  // committed box policy (below an explicit COOP_* setting)
	Gate        string   `yaml:"gate"` // fork-merge revalidation command (an explicit COOP_GATE wins)
}

// Serve is the serving config: container ports to publish.
type Serve struct {
	Ports []int `yaml:"ports"` // what your dev server listens on inside the box
}

// Box is the committed per-repo box policy. Every field is optional; an unset field keeps the
// user's own setting (env/conf) or coop's built-in default. The booleans are pointers because
// absent must stay distinguishable from false (their defaults are true).
type Box struct {
	Egress  string `yaml:"egress"`  // "" (unset) | "open" | "none" — anything else fails Load
	AutoUp  *bool  `yaml:"auto_up"` // auto-start .agent/compose.yml services (default true)
	Network *bool  `yaml:"network"` // join the sibling-services network (default true)
	Memory  string `yaml:"memory"`  // docker --memory syntax, passed through (e.g. 4g)
	CPUs    string `yaml:"cpus"`    // docker --cpus value
	Pids    string `yaml:"pids"`    // --pids-limit: a positive integer, or ""/0/unlimited for none
}

// Load reads <repo>/.agent/project.yaml. A missing file is not an error — it returns an empty Project,
// the common single-repo case. A present-but-invalid file (bad YAML, an unknown key, an out-of-range
// port, a bad box value, or a subproject path that escapes the repo) IS an error, so a typo surfaces
// instead of silently doing nothing. Subproject paths are cleaned in place.
func Load(repo string) (*Project, error) {
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(File)))
	if os.IsNotExist(err) {
		return &Project{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", File, err)
	}
	var p Project
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)                                             // an unknown key is a typo doing nothing — fail loudly instead
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) { // EOF = an all-comments/empty file
		return nil, fmt.Errorf("%s: %w", File, err)
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
	switch p.Box.Egress {
	case "", "open", "none":
	default:
		return nil, fmt.Errorf("%s: box.egress %q — use open or none", File, p.Box.Egress)
	}
	switch p.Box.Pids {
	case "", "0", "unlimited":
	default:
		if n, err := strconv.Atoi(p.Box.Pids); err != nil || n < 1 {
			return nil, fmt.Errorf("%s: box.pids %q — use a positive integer, or 0/unlimited for no cap", File, p.Box.Pids)
		}
	}
	return &p, nil
}

// HostPort maps a repo + container port to a host port deterministically: the same project always
// publishes to the same host port (bookmarkable, and stable across box restarts), while two different
// projects almost never collide. Deterministic rather than first-come so the URL reporter and the box
// itself agree on the mapping without having to coordinate.
func HostPort(repo string, port int) int {
	return hostPortBase + int(crc32.ChecksumIEEE([]byte(repo+":"+fmt.Sprint(port)))%hostPortSpan)
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
