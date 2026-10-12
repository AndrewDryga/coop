package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
)

type usageResetOnlyTransport struct{ body string }

func (transport usageResetOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(transport.body))}, nil
}

func TestUsageResetOnlyResultExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		code             int
	}{
		{"known zero", `{"rate_limit_reset_credits":{"available_count":0}}`, "0 available", 0},
		{"unknown", `{}`, "unavailable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("NO_COLOR", "1")
			cfg := &config.Config{ConfigDir: t.TempDir(), RuntimeName: "must-not-run"}
			importNativeFixture(t, cfg, "codex", "work", nativeauth.Files(t, "codex", "work"))
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = usageResetOnlyTransport{body: tc.body}
			a := &app{cfg: cfg}
			out := captureStdout(t, func() {
				if code, err := a.cmdUsage([]string{"codex@work"}); code != tc.code || err != nil {
					t.Errorf("reset-only inspection=(%d,%v), want %d", code, err, tc.code)
				}
			})
			if !strings.Contains(out, "Resets") || !strings.Contains(out, tc.want) || a.rtSet || strings.Contains(out, "Total  ") {
				t.Fatalf("reset-only result changed runtime or display:\n%s", out)
			}
		})
	}
}
