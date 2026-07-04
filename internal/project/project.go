// Package project reads a repo's .agent/project.yaml — coop's per-project config, committed with the
// repo (unlike the git-ignored rest of .agent/). It carries two things:
//
//   - subprojects: for a monorepo, the member project dirs whose .agent/tasks queues coop aggregates
//     automatically, so you don't hand-maintain COOP_TASKS.
//   - serve.ports: container ports coop publishes so a dev server running in the box is reachable from
//     the host browser, each mapped to a stable host port.
package project

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
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
}

// Serve is the serving config: container ports to publish.
type Serve struct {
	Ports []int `yaml:"ports"` // what your dev server listens on inside the box
}

// Load reads <repo>/.agent/project.yaml. A missing file is not an error — it returns an empty Project,
// the common single-repo case. A present-but-invalid file (bad YAML, an out-of-range port, or a
// subproject path that escapes the repo) IS an error, so a typo surfaces instead of silently doing
// nothing. Subproject paths are cleaned in place.
func Load(repo string) (*Project, error) {
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(File)))
	if os.IsNotExist(err) {
		return &Project{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", File, err)
	}
	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
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
