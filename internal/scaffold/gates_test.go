package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDetectStacks(t *testing.T) {
	write := func(repo, rel, content string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// go.mod → [go]
	repo := t.TempDir()
	write(repo, "go.mod", "module x\n")
	if got := DetectStacks(repo); !slices.Equal(got, []string{"go"}) {
		t.Errorf("go.mod → %v, want [go]", got)
	}
	// a *.tf file → [terraform]
	repo = t.TempDir()
	write(repo, "main.tf", "resource \"null_resource\" \"x\" {}\n")
	if got := DetectStacks(repo); !slices.Equal(got, []string{"terraform"}) {
		t.Errorf("*.tf → %v, want [terraform]", got)
	}
	// .tool-versions drives detection too, in GateLangs order.
	repo = t.TempDir()
	write(repo, ".tool-versions", "elixir 1.18.3-otp-27\nterraform 1.14.0\n")
	if got := DetectStacks(repo); !slices.Equal(got, []string{"terraform", "elixir"}) {
		t.Errorf(".tool-versions → %v, want [terraform elixir]", got)
	}
	// Nothing detected → none (the no-pollute case — coop won't guess).
	if got := DetectStacks(t.TempDir()); got != nil {
		t.Errorf("empty repo → %v, want nil", got)
	}
}

func TestGateGeneration(t *testing.T) {
	// A detected stack's check goes into the hook for that stack only.
	pre := preCommitHook([]string{"terraform"})
	if !strings.Contains(pre, "command -v terraform") || strings.Contains(pre, "command -v gofmt") {
		t.Errorf("terraform pre-commit should check terraform, not go:\n%s", pre)
	}
	if !strings.Contains(pre, "exit 1") { // a git hook blocks on a nonzero exit
		t.Error("pre-commit hook should block with exit 1")
	}
	// The Claude gate blocks the tool call with exit 2.
	claude := testNativeCommitGate("claude", []string{"go"})
	if !strings.Contains(claude, "command -v gofmt") || !strings.Contains(claude, "exit 2") {
		t.Errorf("claude gate should gofmt-check and block with exit 2:\n%s", claude)
	}
	// Rust checks the binary it actually shells out to (rustfmt), not the cargo wrapper — cargo
	// fmt can't be scoped to just the staged files (it only ever formats the whole crate).
	rust := gateSnippet("rust", "1")
	if !strings.Contains(rust, "command -v rustfmt") || strings.Contains(rust, "command -v cargo") {
		t.Errorf("rust gate should check rustfmt, not cargo:\n%s", rust)
	}
	// No stack → a neutral, inert gate (no active check) that still exits 0.
	neutral := preCommitHook(nil)
	if strings.Contains(neutral, "command -v gofmt") {
		t.Errorf("neutral gate must not impose a check:\n%s", neutral)
	}
	if !strings.Contains(neutral, "intentionally empty") || !strings.HasSuffix(strings.TrimSpace(neutral), "exit 0") {
		t.Errorf("neutral gate should be documented-but-inert and end in exit 0:\n%s", neutral)
	}
	// The exit code is parameterized; an unknown lang yields no block.
	if !strings.Contains(gateSnippet("go", "1"), "exit 1") || !strings.Contains(gateSnippet("go", "2"), "exit 2") {
		t.Error("gateSnippet should carry the requested exit code")
	}
	if gateSnippet("nonsense", "1") != "" {
		t.Error("unknown lang → empty snippet")
	}
}

// TestElixirRustGateListBased proves the elixir and rust snippets tell a real formatting diff
// (block) apart from a tool failure — a crash, a parse error, a broken toolchain (fail open) —
// the same way the go/terraform snippets already do. These snippets are shell embedded in a Go
// string: shellcheck never sees them, and CI has no mix/cargo toolchain to catch a regression
// either, so this test is the only guard on their behavior. It runs the generated text in bash
// against stub mix/rustfmt binaries shaped like the real tools' documented exit code and
// stdout/stderr split (verified by hand against installed Elixir 1.14–1.20 and rustfmt 1.9).
func TestElixirRustGateListBased(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	writeStub := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// run executes lang's snippet (block exit code 9, a sentinel that can't collide with a
	// stub's own exit 0/1) with staged set directly, under the IFS/set -f preamble the real
	// hooks establish around gateBody. Returns 0 ("didn't block") or the snippet's exit code.
	run := func(t *testing.T, repo, lang, path string, staged []string, extraEnv ...string) (int, string) {
		t.Helper()
		script := "set -f\nIFS=$'\\n'\nstaged=\"$STAGED\"\n" + gateSnippet(lang, "9") + "\nexit 0\n"
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = repo
		env := append(os.Environ(), "PATH="+path, "STAGED="+strings.Join(staged, "\n"))
		cmd.Env = append(env, extraEnv...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), stderr.String()
			}
			t.Fatalf("%s gate run: %v", lang, err)
		}
		return 0, stderr.String()
	}

	t.Run("elixir", func(t *testing.T) {
		bin := t.TempDir()
		repo := t.TempDir()
		// mix's own --check-formatted failure always carries this exact stable text (true on
		// every Elixir from 1.14 through 1.20 — see lib/mix/tasks/format.ex's check!/2); a
		// "crash" file stands in for a parse error or any other failure, which does not.
		writeStub(t, bin, "mix", `file="$3"
case "$file" in
  *needs_format*) echo "** (Mix) mix format failed due to --check-formatted." >&2; exit 1 ;;
  *crash*) echo "mix format failed for file: $file" >&2; echo "** (TokenMissingError) boom" >&2; exit 1 ;;
  *) exit 0 ;;
esac
`)
		path := bin + string(os.PathListSeparator) + os.Getenv("PATH")

		if code, stderr := run(t, repo, "elixir", path, []string{"lib/good.ex"}); code != 0 {
			t.Errorf("clean file: exit=%d stderr=%q, want 0 (not blocked)", code, stderr)
		}
		if code, stderr := run(t, repo, "elixir", path, []string{"lib/good.ex", "lib/needs_format.ex"}); code != 9 ||
			!strings.Contains(stderr, "lib/needs_format.ex") || strings.Contains(stderr, "lib/good.ex") {
			t.Errorf("real diff: exit=%d stderr=%q, want 9 naming only needs_format.ex", code, stderr)
		}
		// The regression this task fixes: a broken toolchain must not block like a real diff.
		if code, stderr := run(t, repo, "elixir", path, []string{"lib/crash.ex"}); code != 0 {
			t.Errorf("tool crash: exit=%d stderr=%q, want 0 (fail open, not a block)", code, stderr)
		}
		// A crash on one file must not swallow a real diff reported on another.
		if code, stderr := run(t, repo, "elixir", path, []string{"lib/needs_format.ex", "lib/crash.ex"}); code != 9 ||
			!strings.Contains(stderr, "lib/needs_format.ex") || strings.Contains(stderr, "lib/crash.ex") {
			t.Errorf("diff + crash: exit=%d stderr=%q, want 9 naming only needs_format.ex", code, stderr)
		}
	})

	t.Run("rust", func(t *testing.T) {
		bin := t.TempDir()
		noCargoToml := t.TempDir()
		// rustfmt -l prints only the names of files with a genuine diff, to stdout; any other
		// failure (a parse error, here "crash") goes to stderr only, so it never reaches the
		// list — mirrored from a real rustfmt 1.9 run. Logs its argv so the edition subtests
		// below can confirm the snippet actually read Cargo.toml and passed it through.
		writeStub(t, bin, "rustfmt", `log="$RUSTFMT_ARGV_LOG"
[ -n "$log" ] && printf '%s\n' "$*" >> "$log"
for a; do file="$a"; done
case "$file" in
  *needs_format*) echo "$file"; exit 1 ;;
  *crash*) echo "error: this file contains an unclosed delimiter" >&2; exit 1 ;;
  *) exit 0 ;;
esac
`)
		path := bin + string(os.PathListSeparator) + os.Getenv("PATH")

		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/good.rs"}); code != 0 {
			t.Errorf("clean file: exit=%d stderr=%q, want 0 (not blocked)", code, stderr)
		}
		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/good.rs", "src/needs_format.rs"}); code != 9 ||
			!strings.Contains(stderr, "src/needs_format.rs") || strings.Contains(stderr, "src/good.rs") {
			t.Errorf("real diff: exit=%d stderr=%q, want 9 naming only needs_format.rs", code, stderr)
		}
		// The regression this task fixes: a broken toolchain must not block like a real diff.
		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/crash.rs"}); code != 0 {
			t.Errorf("tool crash: exit=%d stderr=%q, want 0 (fail open, not a block)", code, stderr)
		}
		// A crash on one file must not swallow a real diff reported on another.
		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/needs_format.rs", "src/crash.rs"}); code != 9 ||
			!strings.Contains(stderr, "src/needs_format.rs") || strings.Contains(stderr, "src/crash.rs") {
			t.Errorf("diff + crash: exit=%d stderr=%q, want 9 naming only needs_format.rs", code, stderr)
		}

		t.Run("reads the crate's edition off Cargo.toml", func(t *testing.T) {
			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, "Cargo.toml"), []byte("[package]\nedition = \"2021\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(t.TempDir(), "argv.log")
			if code, stderr := run(t, repo, "rust", path, []string{"src/good.rs"}, "RUSTFMT_ARGV_LOG="+log); code != 0 {
				t.Fatalf("clean file: exit=%d stderr=%q, want 0", code, stderr)
			}
			argv, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(argv), "--edition=2021") {
				t.Errorf("rustfmt argv = %q, want --edition=2021 read from Cargo.toml", argv)
			}
		})

		t.Run("no Cargo.toml means no --edition flag", func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "argv.log")
			if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/good.rs"}, "RUSTFMT_ARGV_LOG="+log); code != 0 {
				t.Fatalf("clean file: exit=%d stderr=%q, want 0", code, stderr)
			}
			argv, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(argv), "--edition") {
				t.Errorf("rustfmt argv = %q, want no --edition flag without a Cargo.toml", argv)
			}
		})
	})
}

// A box without the language's toolchain used to pass every commit with no format check and no
// word about it. The commit still goes through, since a missing tool is no formatting diff, but the
// committer now reads why nothing was checked; a commit without that language's files says nothing.
func TestGateWarnsWhenTheFormatterIsMissing(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	grep, err := exec.LookPath("grep")
	if err != nil {
		t.Skip("grep not available")
	}
	// PATH holds grep alone, so no formatter is found even on a host that has one.
	bin := t.TempDir()
	if err := os.Symlink(grep, filepath.Join(bin, "grep")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ lang, file, want string }{
		{"go", "main.go", "pre-commit: gofmt is not installed here, so the Go format check was skipped\n"},
		{"terraform", "infra/main.tf", "pre-commit: terraform is not installed here, so the Terraform format check was skipped\n"},
		{"elixir", "lib/app.ex", "pre-commit: mix is not installed here, so the Elixir format check was skipped\n"},
		{"rust", "src/main.rs", "pre-commit: rustfmt is not installed here, so the Rust format check was skipped\n"},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			for staged, want := range map[string]string{tc.file: tc.want, "README.md": ""} {
				script := "set -f\nIFS=$'\\n'\nstaged=\"$STAGED\"\n" + gateSnippet(tc.lang, "9") + "\nexit 0\n"
				cmd := exec.Command(bash, "-c", script)
				cmd.Dir = t.TempDir()
				cmd.Env = []string{"PATH=" + bin, "STAGED=" + staged}
				var stderr strings.Builder
				cmd.Stderr = &stderr
				if err := cmd.Run(); err != nil {
					t.Errorf("staged %s: the commit was blocked (%v); stderr %q", staged, err, stderr.String())
				}
				if stderr.String() != want {
					t.Errorf("staged %s: stderr = %q, want %q", staged, stderr.String(), want)
				}
			}
		})
	}
}

// A detected language the project does not pin comes back as the .tool-versions lines that would
// pin it: Go with the release its go.mod names, the others with a version the person fills in.
func TestToolchains(t *testing.T) {
	repo := func(files map[string]string) string {
		dir := t.TempDir()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	all := []string{"go", "terraform", "elixir", "rust"}
	got := Toolchains(repo(map[string]string{"go.mod": "module x\n\ngo 1.26\n"}), all)
	want := []Toolchain{
		{"Go", []string{"golang 1.26.0"}},
		{"Terraform", []string{"terraform <version>"}},
		{"Elixir", []string{"erlang <version>", "elixir <version>"}},
		{"Rust", []string{"rust <version>"}},
	}
	if !slices.EqualFunc(got, want, func(a, b Toolchain) bool { return a.Name == b.Name && slices.Equal(a.Pins, b.Pins) }) {
		t.Errorf("Toolchains = %+v, want %+v", got, want)
	}
	// A pinned tool is already the box's; only the rest are missing.
	if got := Toolchains(repo(map[string]string{".tool-versions": "golang 1.26.5\nrust 1.90.0\n"}), all); len(got) != 2 || got[0].Name != "Terraform" || got[1].Name != "Elixir" {
		t.Errorf("Toolchains with go and rust pinned = %+v, want Terraform and Elixir", got)
	}
	if got := Toolchains(repo(nil), nil); got != nil {
		t.Errorf("Toolchains with no language = %+v, want none", got)
	}
	for goMod, version := range map[string]string{
		"module x\ngo 1.26.5\n":                   "1.26.5",
		"module x\ngo 1.26\ntoolchain go1.27.1\n": "1.27.1",
		"module x\ngo 1.20\n":                     "1.20",
		"module x\ngo 1.26\ntoolchain default\n":  "1.26.0",
		"module x\n":                              "",
		"module x\ngo 1.26.5\x1b[31m\n":           "",
		"module x\ngo 1.27rc1\ntoolchain local\n": "",
	} {
		if got := goVersion(repo(map[string]string{"go.mod": goMod})); got != version {
			t.Errorf("goVersion(%q) = %q, want %q", goMod, got, version)
		}
	}
}
