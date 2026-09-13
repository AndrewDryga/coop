package scaffold

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/project"
)

// ComposeServices is the menu of sibling services `coop init` can scaffold into
// .agent/compose.yml — what the interactive prompt offers and what --services accepts.
var ComposeServices = []string{"postgres", "redis"}

// composeUnit is one service's compose block plus the named volume it declares.
type composeUnit struct {
	service string // the Compose service name this catalog entry writes
	port    int    // the port agents use for this service
	image   string // the image it writes, so an existing service of the same name can be recognised
	block   string // the indented "  <name>:" service definition (with a trailing newline)
	volume  string // the named volume to declare under volumes:, or ""
	note    string // an optional header note (e.g. the connection-string hint)
}

var composeCatalog = map[string]composeUnit{
	"postgres": {
		service: "db",
		port:    5432,
		image:   "postgres:",
		block: `  db:
    image: postgres:18
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: dev-password
      POSTGRES_DB: app_dev
    volumes: ["pgdata:/var/lib/postgresql"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 2s
      timeout: 3s
      retries: 15
`,
		volume: "pgdata",
		note:   "# Postgres in the box:\n# DATABASE_URL=postgres://postgres:dev-password@db:5432/app_dev",
	},
	"redis": {
		service: "redis",
		port:    6379,
		image:   "redis:",
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

// composeFor renders a .agent/compose.yml holding just the chosen services (in ComposeServices
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
	// Named volumes outlive a stop, so the header says so and names the one command that does
	// delete them: calling this data throwaway invited someone to find out otherwise.
	b.WriteString("# Services for this project. Start with coop up; stop with coop down.\n")
	b.WriteString("# Agents reach each service by its name below.\n")
	b.WriteString("# Data stays in named volumes when services stop.\n")
	b.WriteString("# coop down --delete-volumes deletes those volumes and their data.\n")
	for _, n := range notes {
		b.WriteString("#\n" + n + "\n")
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

// WriteCompose scaffolds .agent/compose.yml for the chosen sibling services (a subset of
// ComposeServices), never clobbering an existing file. It is a no-op when no service is chosen
// — coop never adds a db/redis a project didn't ask for. It writes the DEFAULT location
// (project.DefaultCompose); a repo that later moves the file says so via box.compose.
func WriteCompose(repo string, services []string) error {
	content := composeFor(services)
	if content == "" {
		return nil
	}
	dest := filepath.Join(repo, filepath.FromSlash(project.DefaultCompose))
	s := &scaffolder{repo: repo}
	return s.writeContentIfAbsent(dest, content, 0o644)
}
