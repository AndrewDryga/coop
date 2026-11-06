package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
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
// (block) apart from a tool failure — a crash, a parse error, a broken toolchain — which fails
// open with one warning naming the tool, the files it could not check and its first complaint,
// unlike gofmt and terraform fmt, which stop the commit. These snippets are shell embedded in a
// Go string: shellcheck never sees them, and CI has no mix/cargo toolchain to catch a regression
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

		if code, stderr := run(t, repo, "elixir", path, []string{"lib/good.ex"}); code != 0 || stderr != "" {
			t.Errorf("clean file: exit=%d stderr=%q, want 0 and nothing said", code, stderr)
		}
		const blocked = "pre-commit blocked — these need mix format: lib/needs_format.ex\n"
		if code, stderr := run(t, repo, "elixir", path, []string{"lib/good.ex", "lib/needs_format.ex"}); code != 9 ||
			!strings.Contains(stderr, blocked) || strings.Contains(stderr, "lib/good.ex") {
			t.Errorf("real diff: exit=%d stderr=%q, want 9 naming only needs_format.ex", code, stderr)
		}
		// A tool error is no formatting diff, so the commit goes through, but never in silence.
		const crashed = "pre-commit: mix format failed on lib/crash.ex, so formatting was not checked: ** (TokenMissingError) boom\n"
		if code, stderr := run(t, repo, "elixir", path, []string{"lib/crash.ex"}); code != 0 || stderr != crashed {
			t.Errorf("tool crash: exit=%d stderr=%q, want 0 (fail open) and %q", code, stderr, crashed)
		}
		// A crash on one file must not swallow a real diff reported on another, nor join its list.
		if code, stderr := run(t, repo, "elixir", path, []string{"lib/needs_format.ex", "lib/crash.ex"}); code != 9 ||
			!strings.Contains(stderr, blocked) || !strings.Contains(stderr, crashed) {
			t.Errorf("diff + crash: exit=%d stderr=%q, want 9 with %q and %q", code, stderr, blocked, crashed)
		}
		// A shim with no version set fails on every file; one line names them all.
		shim := t.TempDir()
		writeStub(t, shim, "mix", "echo 'No version is set for command mix' >&2\nexit 126\n")
		const unset = "pre-commit: mix format failed on lib/a.ex lib/b.ex, so formatting was not checked: No version is set for command mix\n"
		if code, stderr := run(t, repo, "elixir", shim+string(os.PathListSeparator)+os.Getenv("PATH"), []string{"lib/a.ex", "lib/b.ex"}); code != 0 || stderr != unset {
			t.Errorf("no version set: exit=%d stderr=%q, want 0 and %q", code, stderr, unset)
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

		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/good.rs"}); code != 0 || stderr != "" {
			t.Errorf("clean file: exit=%d stderr=%q, want 0 and nothing said", code, stderr)
		}
		const blocked = "pre-commit blocked — these need rustfmt: src/needs_format.rs\n"
		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/good.rs", "src/needs_format.rs"}); code != 9 ||
			!strings.Contains(stderr, blocked) || strings.Contains(stderr, "src/good.rs") {
			t.Errorf("real diff: exit=%d stderr=%q, want 9 naming only needs_format.rs", code, stderr)
		}
		// A tool error is no formatting diff, so the commit goes through, but never in silence.
		const crashed = "pre-commit: rustfmt failed on src/crash.rs, so formatting was not checked: error: this file contains an unclosed delimiter\n"
		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/crash.rs"}); code != 0 || stderr != crashed {
			t.Errorf("tool crash: exit=%d stderr=%q, want 0 (fail open) and %q", code, stderr, crashed)
		}
		// A crash on one file must not swallow a real diff reported on another, nor join its list.
		if code, stderr := run(t, noCargoToml, "rust", path, []string{"src/needs_format.rs", "src/crash.rs"}); code != 9 ||
			!strings.Contains(stderr, blocked) || !strings.Contains(stderr, crashed) {
			t.Errorf("diff + crash: exit=%d stderr=%q, want 9 with %q and %q", code, stderr, blocked, crashed)
		}
		// A shim with no version set fails on every file; one line names them all.
		shim := t.TempDir()
		writeStub(t, shim, "rustfmt", "echo 'No version is set for command rustfmt' >&2\nexit 126\n")
		const unset = "pre-commit: rustfmt failed on src/a.rs src/b.rs, so formatting was not checked: No version is set for command rustfmt\n"
		if code, stderr := run(t, noCargoToml, "rust", shim+string(os.PathListSeparator)+os.Getenv("PATH"), []string{"src/a.rs", "src/b.rs"}); code != 0 || stderr != unset {
			t.Errorf("no version set: exit=%d stderr=%q, want 0 and %q", code, stderr, unset)
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

// A box without the language's toolchain cannot check a commit's format. The commit still goes
// through, since a missing tool is no formatting diff, but the committer reads why nothing was
// checked; a commit without that language's files says nothing.
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

// The rendered pre-commit hook, run by a real git commit. With a working formatter only a real
// diff blocks; one that fails (a shim with no version set, a file it can't parse) checked
// nothing, so the commit stops with one line naming the tool and its complaint; a missing one
// skips the check with a notice. PATH holds the stub formatter, git and grep, and nothing else.
func TestPreCommitHookStopsWhenTheFormatterFails(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	grepBin, err := exec.LookPath("grep")
	if err != nil {
		t.Skip("grep not available")
	}
	// An asdf shim for a tool with no version set for the folder, as the real one answers.
	shim := func(tool string) string {
		return "echo 'No version is set for command " + tool + "' >&2\n" +
			"echo 'Consider adding one of the following versions in your config file' >&2\nexit 126\n"
	}
	const lastArg = `for a; do f="$a"; done` + "\n"
	for _, tc := range []struct {
		name, lang, file, tool string
		stub                   string // the formatter's script; "" leaves it uninstalled
		commits                bool
		want                   string // everything the commit prints
	}{
		{"go clean", "go", "main.go", "gofmt", "exit 0\n", true, ""},
		{"go diff", "go", "main.go", "gofmt", lastArg + `echo "$f"` + "\n", false,
			"pre-commit blocked — these need gofmt:\n  main.go\nfix: gofmt -w <files>   (skip once: git commit --no-verify)\n"},
		{"go exit 126", "go", "main.go", "gofmt", shim("gofmt"), false,
			"pre-commit blocked — gofmt failed: No version is set for command gofmt\n"},
		{"go syntax error", "go", "main.go", "gofmt", lastArg + `echo "$f:2:12: expected ')', found '{'" >&2; exit 2` + "\n", false,
			"pre-commit blocked — gofmt failed: main.go:2:12: expected ')', found '{'\n"},
		{"go absent", "go", "main.go", "gofmt", "", true,
			"pre-commit: gofmt is not installed here, so the Go format check was skipped\n"},
		{"terraform clean", "terraform", "main.tf", "terraform", "exit 0\n", true, ""},
		{"terraform diff", "terraform", "main.tf", "terraform", lastArg + `echo "$f"; exit 3` + "\n", false,
			"pre-commit blocked — these need terraform fmt: main.tf\nfix: terraform fmt <files>   (skip once: git commit --no-verify)\n"},
		{"terraform exit 126", "terraform", "main.tf", "terraform", shim("terraform"), false,
			"pre-commit blocked — terraform fmt failed on main.tf: No version is set for command terraform\n"},
		// terraform -no-color opens its diagnostics with a blank line, as terraform 1.17 does.
		{"terraform syntax error", "terraform", "main.tf", "terraform", `printf '\nError: Invalid expression\n\n  on main.tf line 2:\n' >&2; exit 2` + "\n", false,
			"pre-commit blocked — terraform fmt failed on main.tf: Error: Invalid expression\n"},
		{"terraform absent", "terraform", "main.tf", "terraform", "", true,
			"pre-commit: terraform is not installed here, so the Terraform format check was skipped\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			bin := t.TempDir()
			for name, target := range map[string]string{"git": gitBin, "grep": grepBin} {
				if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.stub != "" {
				write(filepath.Join(bin, tc.tool), "#!/bin/bash\n"+tc.stub, 0o755)
			}
			write(filepath.Join(repo, ".githooks", "pre-commit"), preCommitHook([]string{tc.lang}), 0o755)
			write(filepath.Join(repo, tc.file), "x\n", 0o644)
			git("config", "core.hooksPath", ".githooks")
			git("add", tc.file)
			commit := exec.Command(gitBin, "-c", "maintenance.auto=false", "commit", "-q", "-m", "x")
			commit.Dir = repo
			commit.Env = append(os.Environ(), "PATH="+bin,
				"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"),
				"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "nosystem"))
			out, err := commit.CombinedOutput()
			if committed := err == nil; committed != tc.commits {
				t.Errorf("committed = %t, want %t (%v)", committed, tc.commits, err)
			}
			if string(out) != tc.want {
				t.Errorf("the commit printed %q, want %q", out, tc.want)
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
