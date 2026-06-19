package scaffold

import (
	"strings"
	"testing"
)

func TestComposeFor(t *testing.T) {
	// Nothing chosen → empty (so no compose.agent.yml is written).
	if got := composeFor(nil); got != "" {
		t.Errorf("composeFor(nil) = %q, want empty", got)
	}
	// redis only → redis service + redisdata volume; no db, no DATABASE_URL note.
	r := composeFor([]string{"redis"})
	if !strings.Contains(r, "  redis:") || !strings.Contains(r, "redisdata:") {
		t.Errorf("redis-only compose wrong:\n%s", r)
	}
	if strings.Contains(r, "  db:") || strings.Contains(r, "pgdata") || strings.Contains(r, "DATABASE_URL") {
		t.Errorf("redis-only compose leaked postgres:\n%s", r)
	}
	// postgres only → db + pgdata + the DATABASE_URL hint; no redis.
	p := composeFor([]string{"postgres"})
	if !strings.Contains(p, "  db:") || !strings.Contains(p, "pgdata:") || !strings.Contains(p, "DATABASE_URL") {
		t.Errorf("postgres-only compose wrong:\n%s", p)
	}
	if strings.Contains(p, "  redis:") {
		t.Errorf("postgres-only compose leaked redis:\n%s", p)
	}
	// Both → both services, rendered in ComposeServices order (postgres before redis) regardless
	// of the argument order.
	b := composeFor([]string{"redis", "postgres"})
	if i, j := strings.Index(b, "  db:"), strings.Index(b, "  redis:"); i < 0 || j < 0 || i > j {
		t.Errorf("both-compose: want db then redis:\n%s", b)
	}
	// Unknown service is ignored.
	if got := composeFor([]string{"mongo"}); got != "" {
		t.Errorf("unknown service should yield empty: %q", got)
	}
}
