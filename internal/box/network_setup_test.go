package box

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A passing setup prints one line per property the smoke proved, in the
// script's order, and one verdict with the exact count — and nothing that
// belongs in retained evidence: no hashes, timings, ids or client inventory.
func TestSetupChecksRenderEveryProvedPropertyAndOneVerdict(t *testing.T) {
	var b bytes.Buffer
	if err := writeSetupChecks(&b, ui.Palette{}, 0, nil); err != nil {
		t.Fatalf("a passing smoke returned %v", err)
	}
	want := "  ✓ approved TLS access to example.com works\n" +
		"  ✓ unapproved domains are blocked\n" +
		"  ✓ direct IP connections cannot bypass domain rules\n" +
		"  ✓ the cloud metadata address is blocked\n" +
		"  ✓ DNS does not resolve unapproved domains\n" +
		"\n✓ All 5 checks passed — this host is ready for filtered runs\n"
	if b.String() != want {
		t.Errorf("transcript:\n%s\nwant:\n%s", b.String(), want)
	}
	for _, forbidden := range []string{"sha256", "records", "daemon", "ms", "record ", "next steps", "runtime", "gateway "} {
		if strings.Contains(b.String(), forbidden) {
			t.Errorf("transcript carries the ledger word %q:\n%s", forbidden, b.String())
		}
	}
}

// A failure keeps the passes before it, replaces the failed property with the
// smoke's own reason, claims nothing about the checks after it, and ends with
// the verdict and the fact that nothing was saved.
func TestSetupChecksStopAtTheFailedProperty(t *testing.T) {
	for code, want := range map[int]string{
		31: "  ✗ the allowed TLS destination was not reachable without proxy variables\n",
		32: "  ✗ the allowed TLS destination returned an empty body\n",
		33: "  ✓ approved TLS access to example.com works\n  ✗ a destination outside the policy was reachable\n",
		34: "  ✓ approved TLS access to example.com works\n  ✓ unapproved domains are blocked\n  ✗ a raw IP dial bypassed the allowed-name policy\n",
		35: "  ✓ approved TLS access to example.com works\n  ✓ unapproved domains are blocked\n  ✓ direct IP connections cannot bypass domain rules\n  ✗ the link-local metadata address was reachable\n",
		36: "  ✓ approved TLS access to example.com works\n  ✓ unapproved domains are blocked\n  ✓ direct IP connections cannot bypass domain rules\n  ✓ the cloud metadata address is blocked\n  ✗ a denied name resolved through the gateway resolver\n",
	} {
		var b bytes.Buffer
		err := writeSetupChecks(&b, ui.Palette{}, code, nil)
		if !errors.Is(err, ErrNetworkSetupFailed) {
			t.Fatalf("exit %d returned %v, want ErrNetworkSetupFailed", code, err)
		}
		if b.String() != want+"\n✗ this host is not ready for filtered runs\n  No setup was saved\n" {
			t.Errorf("exit %d transcript:\n%s", code, b.String())
		}
		if strings.Count(b.String(), "✓")+strings.Count(b.String(), "✗") != strings.Count(want, "✓")+2 {
			t.Errorf("exit %d claimed a check the script never ran:\n%s", code, b.String())
		}
	}
	// A smoke that reached no verdict at all claims no check, and says why. A
	// reason joined from several errors keeps every line at the same depth.
	for name, tc := range map[string]struct {
		code int
		err  error
		want string
	}{
		"did not finish": {err: errors.New("the setup check did not finish\ndocker: gateway image is missing"), want: "\n✗ this host is not ready for filtered runs\n  the setup check did not finish\n  docker: gateway image is missing\n  No setup was saved\n"},
		"stray exit":     {code: 1, want: "\n✗ this host is not ready for filtered runs\n  the setup check exited 1 without reaching a verdict\n  No setup was saved\n"},
	} {
		var b bytes.Buffer
		if err := writeSetupChecks(&b, ui.Palette{}, tc.code, tc.err); !errors.Is(err, ErrNetworkSetupFailed) {
			t.Fatalf("%s returned %v", name, err)
		}
		if b.String() != tc.want {
			t.Errorf("%s transcript:\n%q\nwant:\n%q", name, b.String(), tc.want)
		}
		// Nothing that belongs in retained evidence reaches a failure either.
		for _, forbidden := range []string{"sha256", "records", " daemon ", "record ", "next steps", "qualification"} {
			if strings.Contains(b.String(), forbidden) {
				t.Errorf("%s transcript carries the ledger word %q:\n%s", name, forbidden, b.String())
			}
		}
	}
}

// Color is deliberate on a terminal — green passes, red failures, a bold
// green or bold red verdict — and absent everywhere else, with the same bytes
// otherwise.
func TestSetupChecksColorOnlyOnATerminal(t *testing.T) {
	var colored, plain bytes.Buffer
	_ = writeSetupChecks(&colored, ui.Colored(), 33, nil)
	_ = writeSetupChecks(&plain, ui.Palette{}, 33, nil)
	for _, want := range []string{"  \x1b[32m✓\x1b[0m approved TLS access", "  \x1b[31m✗\x1b[0m a destination outside the policy was reachable",
		"\x1b[1m\x1b[31m✗ this host is not ready for filtered runs\x1b[0m\x1b[0m\n  No setup was saved\n"} {
		if !strings.Contains(colored.String(), want) {
			t.Errorf("terminal transcript is missing %q:\n%q", want, colored.String())
		}
	}
	var passed bytes.Buffer
	_ = writeSetupChecks(&passed, ui.Colored(), 0, nil)
	if !strings.HasSuffix(passed.String(), "\x1b[1m\x1b[32m✓ All 5 checks passed — this host is ready for filtered runs\x1b[0m\x1b[0m\n") {
		t.Errorf("the success verdict is not bold green:\n%q", passed.String())
	}
	if strings.Contains(plain.String(), "\x1b") {
		t.Errorf("a plain stream received ANSI:\n%q", plain.String())
	}
	stripped := strings.NewReplacer("\x1b[1m", "", "\x1b[2m", "", "\x1b[31m", "", "\x1b[32m", "", "\x1b[0m", "").Replace(colored.String())
	if stripped != plain.String() {
		t.Errorf("color changed the bytes:\n%q\nvs\n%q", stripped, plain.String())
	}
	if setupPalette(&plain).Enabled() {
		t.Error("a buffer got a colored palette")
	}
}

// A redirected transcript — `coop net setup > setup.log`, or any run under
// NO_COLOR — is the terminal one with the styling taken out: not one ANSI byte,
// the same icons, the same two-space gutters, the same meaning. A reason that
// arrived as several joined errors keeps every line at that gutter, with color
// and without.
func TestSetupChecksRedirectedAndNoColorKeepIconsAndGutters(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	file, err := os.Create(filepath.Join(t.TempDir(), "setup.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if p := setupPalette(file); p.Enabled() {
		t.Fatal("a redirected stream got a colored palette")
	}
	_ = writeSetupChecks(file, setupPalette(file), 34, nil)
	redirected, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	_ = writeSetupChecks(&plain, ui.Palette{}, 34, nil)
	if string(redirected) != plain.String() {
		t.Errorf("redirected transcript:\n%q\nis not the plain one:\n%q", redirected, plain.String())
	}
	if strings.ContainsRune(string(redirected), 0x1b) {
		t.Errorf("a redirected stream received ANSI:\n%q", redirected)
	}
	for _, want := range []string{"  ✓ approved TLS access to example.com works\n", "  ✗ a raw IP dial bypassed the allowed-name policy\n",
		"\n✗ this host is not ready for filtered runs\n", "  No setup was saved\n"} {
		if !strings.Contains(string(redirected), want) {
			t.Errorf("redirected transcript lost %q:\n%s", want, redirected)
		}
	}

	joined := errors.New("the setup check did not finish\ndocker: gateway image is missing")
	for name, p := range map[string]ui.Palette{"plain": {}, "colored": ui.Colored()} {
		var b bytes.Buffer
		_ = writeSetupChecks(&b, p, 0, joined)
		for _, want := range []string{"\n  the setup check did not finish\n", "\n  docker: gateway image is missing\n", "\n  No setup was saved\n"} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("%s continuation lines:\n%q\nwant to contain %q", name, b.String(), want)
			}
		}
		for _, line := range strings.Split(b.String(), "\n") {
			if body := strings.TrimLeft(line, " "); body != line && len(line)-len(body) != 2 {
				t.Errorf("%s indented a line by %d spaces, want exactly 2: %q", name, len(line)-len(body), line)
			}
			if strings.HasPrefix(line, "\t") {
				t.Errorf("%s indented with a tab: %q", name, line)
			}
		}
	}
}

func TestSetupImageSentenceNamesEachOutcome(t *testing.T) {
	for _, tc := range []struct {
		gateway, client bool
		want            string
	}{
		{true, true, "The gateway and client images are already available and will be reused."},
		{false, false, "The gateway and client images need to be built.\nThe first setup can take several minutes."},
		{true, false, "The gateway image will be reused; the client image needs to be built."},
		{false, true, "The gateway image needs to be built; the client image will be reused."},
	} {
		if got := setupImageSentence(tc.gateway, tc.client); got != tc.want {
			t.Errorf("gateway=%v client=%v: %q, want %q", tc.gateway, tc.client, got, tc.want)
		}
	}
	if strings.Contains(setupImageSentence(true, false), "first setup") {
		t.Error("a partial build was called a first setup")
	}
}

// An ordinary launch qualifies the host itself when no current proof exists
// and continues on the proof setup left behind; a current proof costs no setup;
// a failed setup stops the launch; and a caller that may not set the host up —
// the session daemon — is refused with the explicit preparation instead.
func TestEnsureNetworkQualificationSetsTheHostUpOnceThenContinues(t *testing.T) {
	proof := &networkstate.Qualification{ID: strings.Repeat("a", 64)}
	t.Run("missing proof is made", func(t *testing.T) {
		var reads, setups int
		got, err := ensureNetworkQualification(context.Background(), func() (*networkstate.Qualification, error) {
			reads++
			if setups == 0 {
				return nil, nil
			}
			return proof, nil
		}, func(context.Context) error { setups++; return nil })
		if err != nil || got != proof || setups != 1 || reads != 2 {
			t.Fatalf("got %v err=%v setups=%d reads=%d, want the new proof after one setup and a re-read", got, err, setups, reads)
		}
	})
	t.Run("current proof is silent", func(t *testing.T) {
		got, err := ensureNetworkQualification(context.Background(), func() (*networkstate.Qualification, error) { return proof, nil },
			func(context.Context) error { t.Fatal("a current host was set up again"); return nil })
		if err != nil || got != proof {
			t.Fatalf("got %v err=%v", got, err)
		}
	})
	t.Run("failed setup stops the launch", func(t *testing.T) {
		reads := 0
		_, err := ensureNetworkQualification(context.Background(), func() (*networkstate.Qualification, error) { reads++; return nil, nil },
			func(context.Context) error {
				return errors.New("Claude Code cannot start — this host is not ready for filtered runs: a destination outside the policy was reachable")
			})
		if err == nil || !strings.Contains(err.Error(), "not ready for filtered runs") || reads != 1 {
			t.Fatalf("err=%v reads=%d", err, reads)
		}
	})
	t.Run("setup that does not cover the run", func(t *testing.T) {
		_, err := ensureNetworkQualification(context.Background(), func() (*networkstate.Qualification, error) { return nil, nil },
			func(context.Context) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "does not cover this run") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("no permission to set up", func(t *testing.T) {
		_, err := ensureNetworkQualification(context.Background(), func() (*networkstate.Qualification, error) { return nil, nil }, nil)
		if err == nil || !strings.Contains(err.Error(), "run 'coop net setup' first") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("docker failure is the reason", func(t *testing.T) {
		_, err := ensureNetworkQualification(context.Background(), func() (*networkstate.Qualification, error) {
			return nil, errors.New("Cannot connect to the Docker daemon")
		},
			func(context.Context) error { t.Fatal("setup ran without Docker"); return nil })
		if err == nil || !strings.Contains(err.Error(), "Docker daemon") {
			t.Fatalf("err=%v", err)
		}
	})
}

// The refusal a pending request earns names who cannot start and the one
// review that settles it, and nothing else.
func TestNetworkLaunchNameIsTheAgentOrTheBox(t *testing.T) {
	if got := networkLaunchName(RunSpec{Agent: "claude"}); got != "Claude Code" {
		t.Errorf("claude = %q", got)
	}
	if got := networkLaunchName(RunSpec{}); got != "This box" {
		t.Errorf("raw command = %q", got)
	}
}
