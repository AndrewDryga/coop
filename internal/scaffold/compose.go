package scaffold

import (
	"path/filepath"
	"slices"
	"strings"
)

// ComposeServices is the menu of sibling services `coop init` can scaffold into
// compose.agent.yml — what the interactive prompt offers and what --services accepts.
var ComposeServices = []string{"postgres", "redis"}

// composeUnit is one service's compose block plus the named volume it declares.
type composeUnit struct {
	block  string // the indented "  <name>:" service definition (with a trailing newline)
	volume string // the named volume to declare under volumes:, or ""
	note   string // an optional header note (e.g. the connection-string hint)
}

var composeCatalog = map[string]composeUnit{
	"postgres": {
		block: `  db:
    image: postgres:18
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: app_dev
    volumes: ["pgdata:/var/lib/postgresql/data"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 2s
      timeout: 3s
      retries: 15
`,
		volume: "pgdata",
		note:   "# Connection string for the box, e.g.: DATABASE_URL=postgres://postgres:postgres@db:5432/app_dev",
	},
	"redis": {
		block: `  redis:
    image: redis:8
    volumes: ["redisdata:/data"]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 3s
      retries: 15
`,
		volume: "redisdata",
	},
}

// composeFor renders a compose.agent.yml holding just the chosen services (in ComposeServices
// order, unknowns ignored). Returns "" when none are chosen.
func composeFor(services []string) string {
	var blocks, vols, notes []string
	for _, name := range ComposeServices {
		if !slices.Contains(services, name) {
			continue
		}
		u := composeCatalog[name]
		blocks = append(blocks, u.block)
		if u.volume != "" {
			vols = append(vols, "  "+u.volume+":")
		}
		if u.note != "" {
			notes = append(notes, u.note)
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Sibling services for the agent. `coop up` starts them; the box reaches them\n")
	b.WriteString("# by name. Dev data is throwaway — it lives in named volumes.\n")
	for _, n := range notes {
		b.WriteString(n + "\n")
	}
	b.WriteString("services:\n")
	for _, bl := range blocks {
		b.WriteString(bl)
	}
	if len(vols) > 0 {
		b.WriteString("volumes:\n")
		for _, v := range vols {
			b.WriteString(v + "\n")
		}
	}
	return b.String()
}

// WriteCompose scaffolds compose.agent.yml for the chosen sibling services (a subset of
// ComposeServices), never clobbering an existing file. It is a no-op when no service is chosen
// — coop never adds a db/redis a project didn't ask for.
func WriteCompose(repo string, services []string) error {
	content := composeFor(services)
	if content == "" {
		return nil
	}
	s := &scaffolder{repo: repo}
	return s.writeContentIfAbsent(filepath.Join(repo, "compose.agent.yml"), content, 0o644)
}
