package scaffold

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// dockerFinds is what detectDocker turned up: the repo's own Dockerfiles and compose files
// (coop's own .agent/Dockerfile / .agent/compose.yml live in the hidden .agent/, never scanned),
// plus the service names from the first compose file.
type dockerFinds struct {
	dockerfiles []string // repo-relative paths
	composes    []string // repo-relative paths
	services    []string // service names from composes[0]
}

func (f dockerFinds) any() bool { return len(f.dockerfiles) > 0 || len(f.composes) > 0 }

// dockerSkipDirs are directories detectDocker never descends into.
var dockerSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "deps": true, "_build": true, "target": true,
	".terraform": true,
}

// detectDocker scans the repo (root + its immediate subdirs) for Dockerfiles and compose
// files, skipping coop's own and heavy/irrelevant dirs.
func detectDocker(repo string) dockerFinds {
	var f dockerFinds
	root := filepath.Clean(repo)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			// Skip hidden dirs, known heavy dirs, and anything below the first level.
			if strings.HasPrefix(d.Name(), ".") || dockerSkipDirs[d.Name()] || strings.Contains(rel, string(filepath.Separator)) {
				return fs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case isDockerfile(d.Name()):
			f.dockerfiles = append(f.dockerfiles, rel)
		case isComposeFile(d.Name()):
			f.composes = append(f.composes, rel)
		}
		return nil
	})
	if len(f.composes) > 0 {
		f.services = composeServices(filepath.Join(root, f.composes[0]))
	}
	return f
}

// isDockerfile matches Dockerfile and Dockerfile.<x>. coop's own box Dockerfile lives at
// .agent/Dockerfile — inside the hidden .agent/ that detectDocker never scans — so it needs no
// exclusion here.
func isDockerfile(name string) bool {
	return name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.")
}

// isComposeFile matches docker-compose / compose .yml/.yaml (incl. .<x>.yml overlays). coop's own
// sibling-services file lives at .agent/compose.yml — inside the hidden .agent/ that detectDocker
// never scans — so it needs no exclusion here.
func isComposeFile(name string) bool {
	for _, pre := range []string{"docker-compose", "compose"} {
		if name == pre+".yml" || name == pre+".yaml" {
			return true
		}
		if strings.HasPrefix(name, pre+".") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			return true
		}
	}
	return false
}

// serviceKeyRe matches a top-level compose service name: a 2-space-indented "name:".
var serviceKeyRe = regexp.MustCompile(`^  ([A-Za-z0-9._-]+):\s*$`)

// composeServices shallow-scans a compose file's top-level services: block for the service
// names (no YAML dependency — coop is stdlib-light, and the names are all the suggestion needs).
func composeServices(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var names []string
	inServices := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "services:" {
			inServices = true
			continue
		}
		if !inServices {
			continue
		}
		if len(trimmed) > 0 && !strings.HasPrefix(trimmed, " ") && !strings.HasPrefix(trimmed, "\t") {
			break // a new top-level key ends the services block
		}
		if m := serviceKeyRe.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// ComposeServiceNames is the exported reader for composeServices — the service names actually
// defined in the compose file at path (nil if there's no file or no services), e.g. for `coop help`
// to list what `coop up` would start. Distinct from the ComposeServices var (the offerable menu):
// picking "postgres" writes a service named "db", so the real name comes from the file.
func ComposeServiceNames(path string) []string { return composeServices(path) }

// dockerfileSuggestion is the "base the box on your image" template; %s is the agent npm
// package list (from agents.Packages(), so it never drifts from the asdf image).
const dockerfileSuggestion = `
  Box image — base the agent box on your image so the agent runs in your app's
  environment. Save as .agent/Dockerfile, then 'coop build':

    FROM your-app-image:latest AS base          # or a build stage from your Dockerfile
    USER root
    RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
     && apt-get install -y --no-install-recommends nodejs git ca-certificates \
     && npm install -g %s \
     && git config --system --add safe.directory '*' \
     && (id -u node >/dev/null 2>&1 || useradd -m -u 1000 -s /bin/bash node)
    USER node
    WORKDIR /workspace
    # (apt shown — swap for apk/etc. if your base isn't Debian/Ubuntu)
`

// SuggestDocker prints (docs only, never writes) how to build the agent box on the repo's
// existing Docker. It runs only when the box isn't set up yet — a Dockerized repo with no
// .agent/Dockerfile is the gap it fills; it never nags an already-configured one. The caller
// (cmdInit) runs it after the summary anchor so it reads as box-setup guidance before the steps.
func SuggestDocker(repo string) {
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(project.DockerfilePath(repo)))); err == nil {
		return
	}
	f := detectDocker(repo)
	if !f.any() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", ui.Bold("this repo already has Docker — you can build the agent box on it:"))
	if len(f.dockerfiles) > 0 {
		fmt.Fprintf(&b, "  Dockerfiles:  %s\n", strings.Join(f.dockerfiles, ", "))
	}
	if len(f.composes) > 0 {
		line := strings.Join(f.composes, ", ")
		if len(f.services) > 0 {
			line += "  (services: " + strings.Join(f.services, ", ") + ")"
		}
		fmt.Fprintf(&b, "  compose:      %s\n", line)
	}
	if len(f.dockerfiles) > 0 {
		fmt.Fprintf(&b, dockerfileSuggestion, strings.Join(agents.Packages(), " "))
	}
	if len(f.composes) > 0 {
		fmt.Fprintf(&b, "\n  Sibling services — coop starts deps for the box from .agent/compose.yml (reached\n"+
			"  by name). Copy the services your code needs from %s into it, then 'coop up'.\n", f.composes[0])
	}
	fmt.Fprint(os.Stderr, b.String())
}
