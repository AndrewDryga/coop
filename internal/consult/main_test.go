package consult

import (
	"os"
	"testing"
)

// TestMain keeps a surrounding box's PATH out of these tests. Inside a coop box COOP_BOX_PATH is
// set, and the wrapper under test would trade the stub directory these tests put on PATH for it.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("COOP_BOX_PATH")
	os.Exit(m.Run())
}
