package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestMainBarePrintsHelp verifies bare `coop` prints help and exits 0 without a
// container runtime (it returns before runtime detection) — so a stray `coop`
// never launches an agent; running one is explicit (`coop claude`).
func TestMainBarePrintsHelp(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := Main(nil)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if code != 0 {
		t.Errorf("bare coop exit = %d, want 0", code)
	}
	if s := string(out); !strings.Contains(s, "Usage") || !strings.Contains(s, "coop claude") {
		t.Errorf("bare coop should print help listing `coop claude`; got:\n%s", s)
	}
}
