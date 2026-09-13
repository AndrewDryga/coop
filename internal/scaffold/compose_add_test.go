package scaffold

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

const composeOriginal = "# operator comment\nservices:\n  web:\n    image: nginx:alpine\n"

func TestAddComposeServicesBoundaries(t *testing.T) {
	for _, kind := range []string{"leaf symlink", "dangling leaf", "parent symlink", "absent outside child", "lexical escape", "absolute path", "directory", "ordinary file", "in-tree parent"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			repo, outside := filepath.Join(base, "repo"), filepath.Join(base, "outside")
			for _, dir := range []string{repo, outside} {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			protected := filepath.Join(outside, "compose.yml")
			absent := kind == "dangling leaf" || kind == "absent outside child"
			if !absent {
				writeComposeFixture(t, protected, composeOriginal)
			}
			rel := ".agent/compose.yml"
			parent := filepath.Join(repo, ".agent")
			link := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "parent symlink" || kind == "absent outside child" {
				link(outside, parent)
			} else if kind == "in-tree parent" {
				if err := os.Mkdir(filepath.Join(repo, "config"), 0o755); err != nil {
					t.Fatal(err)
				}
				link("config", parent)
				writeComposeFixture(t, filepath.Join(repo, rel), composeOriginal)
			} else {
				if err := os.Mkdir(parent, 0o755); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "leaf symlink", "dangling leaf":
					link(protected, filepath.Join(repo, rel))
				case "lexical escape":
					rel = "../outside/compose.yml"
				case "absolute path":
					rel = protected
				case "directory":
					if err := os.Mkdir(filepath.Join(repo, rel), 0o755); err != nil {
						t.Fatal(err)
					}
				case "ordinary file":
					writeComposeFixture(t, filepath.Join(repo, rel), composeOriginal)
				}
			}
			out, err := AddComposeServices(repo, rel, []string{"postgres"})
			if kind == "ordinary file" || kind == "in-tree parent" {
				if err != nil || out.Created || !slices.Equal(out.Added, []string{"postgres"}) {
					t.Fatalf("ordinary addition = %+v, %v", out, err)
				}
				body := readComposeFixture(t, filepath.Join(repo, rel))
				if !strings.HasPrefix(body, composeOriginal) || !strings.Contains(body, "  db:") {
					t.Fatalf("ordinary edit lost comment/service: %s", body)
				}
			} else if err == nil || out.Created || len(out.Added) != 0 {
				t.Errorf("unsafe target accepted: %+v, %v", out, err)
			}
			if absent {
				if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
					t.Errorf("outside directory changed: %v, %v", entries, err)
				}
			} else if got := readComposeFixture(t, protected); got != composeOriginal {
				t.Errorf("outside synthetic file changed: %q", got)
			}
			if kind == "leaf symlink" || kind == "dangling leaf" {
				if target, err := os.Readlink(filepath.Join(repo, rel)); err != nil || target != protected {
					t.Errorf("leaf link changed: %q, %v", target, err)
				}
			}
			if kind == "parent symlink" || kind == "absent outside child" || kind == "in-tree parent" {
				want := outside
				if kind == "in-tree parent" {
					want = "config"
				}
				if target, err := os.Readlink(parent); err != nil || target != want {
					t.Errorf("parent link changed: %q, %v", target, err)
				}
			}
		})
	}
}

func TestAddComposeServicesPreservesConfiguration(t *testing.T) {
	repo := t.TempDir()
	const rel = ".agent/compose.yml"
	if out, err := AddComposeServices(repo, rel, nil); err != nil || out.Created {
		t.Fatalf("empty selection = %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty selection created a parent: %v", err)
	}
	out, err := AddComposeServices(repo, rel, []string{"redis", "postgres"})
	if err != nil || !out.Created || !slices.Equal(out.Added, ComposeServices) {
		t.Fatalf("creation = %+v, %v", out, err)
	}
	path := filepath.Join(repo, rel)
	before := readComposeFixture(t, path)
	out, err = AddComposeServices(repo, rel, []string{"postgres", "redis"})
	if err != nil || out.Created || len(out.Added) != 0 || !slices.Equal(out.Existing, ComposeServices) {
		t.Fatalf("idempotent addition = %+v, %v", out, err)
	}
	if got := readComposeFixture(t, path); got != before {
		t.Fatal("no-op changed existing configuration")
	}
	for _, body := range []string{"# custom\nservices:\n  db:\n    image: example/custom-db\n", "services: ["} {
		writeComposeFixture(t, path, body)
		out, err = AddComposeServices(repo, rel, []string{"postgres", "redis"})
		if err == nil || out.Created || len(out.Added) != 0 {
			t.Fatalf("collision/invalid YAML accepted: %+v, %v", out, err)
		}
		if got := readComposeFixture(t, path); got != body {
			t.Fatal("refused addition changed existing configuration")
		}
	}
}

func TestAddComposeServicesPublicationFailures(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial create", true: "partial replacement"}[present], func(t *testing.T) {
			repo := t.TempDir()
			path := filepath.Join(repo, "compose.yml")
			if present {
				writeComposeFixture(t, path, composeOriginal)
			}
			interrupted := errors.New("interrupted write")
			out, err := addComposeServices(repo, "compose.yml", []string{"postgres"}, func(file *os.File, data []byte) error {
				if _, err := file.Write(data[:len(data)/2]); err != nil {
					return err
				}
				return interrupted
			}, nil)
			if !errors.Is(err, interrupted) || out.Created || len(out.Added) != 0 {
				t.Fatalf("failed publication = %+v, %v", out, err)
			}
			wantEntries := 0
			if present {
				wantEntries = 1
				if got := readComposeFixture(t, path); got != composeOriginal {
					t.Fatal("partial replacement corrupted original")
				}
			}
			if entries, err := os.ReadDir(repo); err != nil || len(entries) != wantEntries {
				t.Fatalf("partial publication left files: %v, %v", entries, err)
			}
		})
	}
	for _, kind := range []string{"edit", "replacement", "symlink", "creation", "created symlink"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			path := filepath.Join(repo, "compose.yml")
			absent := kind == "creation" || kind == "created symlink"
			if !absent {
				writeComposeFixture(t, path, composeOriginal)
			}
			outside := filepath.Join(t.TempDir(), "outside.yml")
			writeComposeFixture(t, outside, composeOriginal)
			const edited = "# concurrent edit\nservices: {}\n"
			out, err := addComposeServices(repo, "compose.yml", []string{"postgres"}, writeAndSync, func() error {
				switch kind {
				case "symlink", "created symlink":
					if !absent {
						if err := os.Remove(path); err != nil {
							return err
						}
					}
					return os.Symlink(outside, path)
				case "replacement":
					tmp := filepath.Join(repo, "replacement.yml")
					writeComposeFixture(t, tmp, composeOriginal)
					return os.Rename(tmp, path)
				default:
					return os.WriteFile(path, []byte(edited), 0o644)
				}
			})
			if err == nil || out.Created || len(out.Added) != 0 {
				t.Fatalf("intervening change accepted: %+v, %v", out, err)
			}
			if strings.Contains(err.Error(), "project.yaml") {
				t.Fatalf("wrong error context: %v", err)
			}
			if kind == "edit" || kind == "replacement" || kind == "creation" {
				if !strings.Contains(err.Error(), "compose file compose.yml changed") || !strings.Contains(err.Error(), "retry coop init --services") {
					t.Fatalf("missing retry context: %v", err)
				}
				want := edited
				if kind == "replacement" {
					want = composeOriginal
				}
				if got := readComposeFixture(t, path); got != want {
					t.Fatal("intervening edit overwritten")
				}
			} else if target, err := os.Readlink(path); err != nil || target != outside {
				t.Errorf("intervening link replaced: %q, %v", target, err)
			}
			if got := readComposeFixture(t, outside); got != composeOriginal {
				t.Fatal("intervening link wrote outside repository")
			}
			if entries, err := os.ReadDir(repo); err != nil || len(entries) != 1 {
				t.Fatalf("temporary files left behind: %v, %v", entries, err)
			}
		})
	}
}

// Isolate the process-wide umask and bound a FIFO regression without leaving a blocked goroutine.
func TestComposeFileSpecialCases(t *testing.T) {
	if repo := os.Getenv("COOP_SCAFFOLD_SPECIAL_REPO"); repo != "" {
		if os.Getenv("COOP_SCAFFOLD_SPECIAL_CASE") == "fifo" {
			path := filepath.Join(repo, "compose.yml")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(repo)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if _, _, err := readProjectFile(root, "compose.yml", "compose.yml"); err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("shared reader did not refuse FIFO: %v", err)
			}
			if out, err := AddComposeServices(repo, "compose.yml", []string{"postgres"}); err == nil || out.Created || len(out.Added) != 0 {
				t.Fatalf("Compose did not refuse FIFO: %+v, %v", out, err)
			}
			return
		}
		syscall.Umask(0o077)
		for _, rel := range []string{"compose.yml", ".agent/project.yaml"} {
			path := filepath.Join(repo, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			body := composeOriginal
			if rel != "compose.yml" {
				body = "subprojects:\n  - portal\n"
			}
			writeComposeFixture(t, path, body)
			if err := os.Chmod(path, 0o664); err != nil {
				t.Fatal(err)
			}
			var err error
			if rel == "compose.yml" {
				_, err = AddComposeServices(repo, rel, []string{"postgres"})
			} else {
				_, err = RegisterSubprojects(repo, []string{"portal", "infra"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o664 {
				t.Fatalf("replacement changed permissions: %v, %v", info, err)
			}
		}
		return
	}
	for _, kind := range []string{"fifo", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestComposeFileSpecialCases$", "-test.v")
			cmd.Env = append(os.Environ(), "COOP_SCAFFOLD_SPECIAL_REPO="+t.TempDir(), "COOP_SCAFFOLD_SPECIAL_CASE="+kind)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s subprocess: %v (deadline: %v)\n%s", kind, err, ctx.Err(), output)
			}
		})
	}
}

func writeComposeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readComposeFixture(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
