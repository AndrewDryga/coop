package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// fakeMounts is a runtime whose boxes mount sources. With daemon set, it is frozen to that daemon
// and lists it alone, as runtime.FreezeCompose makes the sweep's runtime.
type fakeMounts struct {
	daemon  string
	sources map[string]bool
	err     error
}

func (f fakeMounts) ComposeBinding() (string, string) { return "", f.daemon }

func (f fakeMounts) MountSourcesByLabel(
	context.Context, string, string,
) (map[string]bool, error) {
	return f.sources, f.err
}

func writeEntry(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// newRunfiles is a Coop home with an empty private runfiles root, returned with links resolved as
// the launch mounts from it (macOS's temp directory is under a /var link).
func newRunfiles(t *testing.T) (*config.Config, string) {
	t.Helper()
	cfg := &config.Config{BoxHome: t.TempDir()}
	root := filepath.Join(cfg.BoxHome, "runfiles")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, root
}

// runFolder is a run's folder in root that recorded daemon as its box's (none when empty).
func runFolder(t *testing.T, root, name, daemon string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "env"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordRunDaemon(path, daemon)
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// Reaping takes both signals, because either alone is wrong.
//
// Ownership alone races a box that has written its config and not yet started
// its container — for that moment its files are genuinely unmounted, and
// deleting one breaks the launch in a way that looks like a corrupt config.
// Age alone is wrong because a warm box outlives any threshold.
func TestReapNeedsBothUnmountedAndOld(t *testing.T) {
	cfg, root := newRunfiles(t)
	orphan := runFolder(t, root, "coop-run-orphan", "daemon-a", 3*time.Hour)
	inUse := runFolder(t, root, "coop-run-live", "daemon-a", 3*time.Hour)
	justWritten := runFolder(t, root, "coop-run-starting", "daemon-a", time.Minute)
	notOurs := writeDirEntry(t, root, "someone-elses-dir", 3*time.Hour)

	removed, err := ReapOrphanRunfiles(context.Background(),
		fakeMounts{daemon: "daemon-a", sources: map[string]bool{inUse: true}}, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d entries, want 1", removed)
	}
	if exists(orphan) {
		t.Error("the orphan survived")
	}
	for name, path := range map[string]string{
		"a mounted run folder":      inUse,
		"a box that is starting up": justWritten,
		"another program's folder":  notOurs,
	} {
		if !exists(path) {
			t.Errorf("%s was deleted", name)
		}
	}
}

func TestReapPreservesRunDirectoryWhileAChildArtifactIsMounted(t *testing.T) {
	cfg, root := newRunfiles(t)
	run := runFolder(t, root, "coop-run-live", "daemon-a", 3*time.Hour)
	artifact := filepath.Join(run, "env")
	removed, err := ReapOrphanRunfiles(context.Background(),
		fakeMounts{daemon: "daemon-a", sources: map[string]bool{artifact: true}}, cfg, time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("mounted run directory reap = %d, %v", removed, err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("mounted artifact was reaped with its parent: %v", err)
	}
}

// A sweep lists one daemon's boxes, so it proves what that daemon's boxes use and nothing more:
// after `docker context use other`, or with DOCKER_HOST pointing elsewhere, the boxes still running
// on the first daemon are not in the listing at all, and their run folders look unused. A run
// folder is removed only when it names the daemon that answered; one that names another daemon,
// or none, is kept however old it is.
func TestReapRunfilesJudgesARunFolderOnlyByItsOwnDaemon(t *testing.T) {
	cfg, root := newRunfiles(t)
	here := runFolder(t, root, "coop-run-here", "daemon-a", 3*time.Hour)
	elsewhere := runFolder(t, root, "coop-run-elsewhere", "daemon-b", 3*time.Hour)
	unknown := runFolder(t, root, "coop-run-unknown", "", 3*time.Hour)

	removed, err := ReapOrphanRunfiles(context.Background(),
		fakeMounts{daemon: "daemon-a", sources: map[string]bool{}}, cfg, time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("reap = %d, %v; want this daemon's one orphaned run folder", removed, err)
	}
	if exists(here) {
		t.Error("an orphaned run folder of the daemon that answered survived")
	}
	for name, path := range map[string]string{"another daemon's run folder": elsewhere, "a run folder that names no daemon": unknown} {
		if !exists(path) {
			t.Errorf("%s was removed on this daemon's word", name)
		}
	}

	// An unfrozen runtime names no daemon: Apple's container, or one the sweep could not pin. Its
	// listing proves no run folder unused, though what needs no daemon's word still goes.
	client := writeDirEntry(t, root, "coop-docker-client-1", 3*time.Hour)
	removed, err = ReapOrphanRunfiles(context.Background(), fakeMounts{sources: map[string]bool{}}, cfg, time.Now())
	if err != nil || removed != 1 || exists(client) || !exists(elsewhere) || !exists(unknown) {
		t.Fatalf("reap by an unfrozen runtime = %d, %v; want only the client folder removed", removed, err)
	}
}

// A frozen runtime's listing fails when its daemon stops answering, or another daemon answers on
// its socket by the end (runtime.MountSourcesByLabel). A sweep without that daemon's word removes
// nothing at all, so the next launch tries again.
func TestReapRunfilesRefusesAListingFromAnUnprovenDaemon(t *testing.T) {
	cfg, root := newRunfiles(t)
	run := runFolder(t, root, "coop-run-orphan", "daemon-a", 3*time.Hour)
	client := writeDirEntry(t, root, "coop-docker-client-1", 3*time.Hour)

	lister := fakeMounts{daemon: "daemon-a", err: errors.New("Docker daemon identity changed; custody remains pending")}
	removed, err := ReapOrphanRunfiles(context.Background(), lister, cfg, time.Now())
	if !errors.Is(err, ErrMountsUnknown) || removed != 0 {
		t.Fatalf("reap = %d, %v; want ErrMountsUnknown and nothing removed", removed, err)
	}
	if !exists(run) || !exists(client) {
		t.Fatal("a sweep that could not prove its daemon removed something")
	}
}

// A runtime that cannot be asked what is mounted must stop the reaper, not
// empty the directory.
//
// This is the failure that would turn a cleanup into an outage: docker briefly
// unavailable, every path reads as unmounted, and every running box loses its
// configuration at once.
func TestReapRefusesToGuessWhenTheRuntimeIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	orphan := writeEntry(t, dir, "coop-audit-trees-orphan", 3*time.Hour)

	removed, err := ReapOrphanTempEntries(
		context.Background(),
		fakeMounts{err: errors.New("cannot connect to the docker daemon")},
		dir, time.Now(),
	)
	if !errors.Is(err, ErrMountsUnknown) {
		t.Fatalf("an unavailable runtime = %v, want ErrMountsUnknown, never an empty mount list", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries while unable to check what is in use", removed)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("deleted a file without being able to confirm it was unused")
	}
}

func writeDirEntry(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// Coop's Docker and Compose client directories are coop entries too. A killed run leaves one
// behind every time (3,731 Docker client directories on one Mac), so the sweep of Coop's private
// runfiles root, where both now live, removes them under the same old-and-unmounted rule as
// everything else.
func TestReapRunfilesCoversCoopClientConfigs(t *testing.T) {
	cfg, root := newRunfiles(t)
	docker := writeDirEntry(t, root, "coop-docker-client-123", 3*time.Hour)
	compose := writeDirEntry(t, root, "coop-compose-client-456", 3*time.Hour)
	run := runFolder(t, root, "coop-run-789", "daemon-a", 3*time.Hour)
	starting := writeDirEntry(t, root, "coop-docker-client-012", time.Minute)
	notOurs := writeDirEntry(t, root, "someone-elses-dir", 3*time.Hour)

	removed, err := ReapOrphanRunfiles(context.Background(), fakeMounts{daemon: "daemon-a", sources: map[string]bool{}}, cfg, time.Now())
	if err != nil || removed != 3 {
		t.Fatalf("reap = %d, %v; want 3", removed, err)
	}
	for name, path := range map[string]string{"Docker client": docker, "Compose client": compose, "run": run} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the old %s directory survived: %v", name, err)
		}
	}
	for name, path := range map[string]string{"a client directory younger than an hour": starting, "another program's directory": notOurs} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

// A live run keeps handing Docker its client directory without mounting it, so in a temp directory
// other users can write, removing one would let any of them recreate it with a Docker config of
// their own. A temp directory sweep therefore never removes one, not even from a temp directory
// only this user can write, where earlier versions left theirs; everything else is swept as before.
func TestReapTempLeavesDockerClientConfigs(t *testing.T) {
	for name, mode := range map[string]os.FileMode{"private temp directory": 0o700, "shared temp directory": 0o1777} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			docker := writeDirEntry(t, dir, "coop-docker-client-123", 3*time.Hour)
			compose := writeDirEntry(t, dir, "coop-compose-client-456", 3*time.Hour)

			removed, err := ReapOrphanTempEntries(context.Background(), fakeMounts{sources: map[string]bool{}}, dir, time.Now())
			if err != nil || removed != 1 {
				t.Fatalf("reap = %d, %v; want 1", removed, err)
			}
			if _, err := os.Stat(compose); !os.IsNotExist(err) {
				t.Errorf("the old Compose client directory survived: %v", err)
			}
			if _, err := os.Stat(docker); err != nil {
				t.Errorf("a temp directory sweep removed a Docker client directory: %v", err)
			}
		})
	}
}

// Earlier versions put each run's box files straight into the temp directory. None of them says
// which daemon its box ran on, so no listing can prove one unused, and the sweep leaves them all;
// what no box mounts, such as an audit's tree snapshot, it still removes.
func TestReapTempKeepsEarlierVersionsBoxFiles(t *testing.T) {
	dir := t.TempDir()
	var boxFiles []string
	for _, name := range []string{"coop-run-1", "coop-mcp-1", "coop-decoy-1", "coop-githooks-1", "coop-skills-claude-1"} {
		boxFiles = append(boxFiles, writeDirEntry(t, dir, name, 72*time.Hour))
	}
	audit := writeDirEntry(t, dir, "coop-audit-trees-1", 72*time.Hour)

	removed, err := ReapOrphanTempEntries(context.Background(), fakeMounts{daemon: "daemon-a", sources: map[string]bool{}}, dir, time.Now())
	if err != nil || removed != 1 || exists(audit) {
		t.Fatalf("reap = %d, %v (audit snapshot left: %v); want only the audit snapshot removed", removed, err, exists(audit))
	}
	for _, path := range boxFiles {
		if !exists(path) {
			t.Errorf("an earlier version's box file %s was removed", filepath.Base(path))
		}
	}
}

// The sweep removes in-use Docker client folders only because no other user can recreate one in
// Coop's own runfiles. A runfiles that is a link, or that someone else owns, is not that folder, so
// the sweep refuses it and removes nothing there.
func TestReapRunfilesRefusesALinkedOrForeignRoot(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, runfiles, target string){
		"link": func(t *testing.T, runfiles, target string) {
			if err := os.Symlink(target, runfiles); err != nil {
				t.Fatal(err)
			}
		},
		"someone else's": func(t *testing.T, runfiles, target string) {
			if os.Geteuid() != 0 {
				t.Skip("giving the folder another owner needs root")
			}
			if err := os.Rename(target, runfiles); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(runfiles, 65534, 65534); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{BoxHome: t.TempDir()}
			runfiles := filepath.Join(cfg.BoxHome, "runfiles")
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			writeDirEntry(t, target, "coop-docker-client-123", 3*time.Hour)
			setup(t, runfiles, target)

			removed, err := ReapOrphanRunfiles(context.Background(), fakeMounts{sources: map[string]bool{}}, cfg, time.Now())
			if err == nil || removed != 0 {
				t.Fatalf("reap of a %s runfiles = %d, %v; want a refusal", name, removed, err)
			}
			if _, err := os.Stat(filepath.Join(runfiles, "coop-docker-client-123")); err != nil {
				t.Fatalf("the refused sweep removed a Docker client folder: %v", err)
			}
		})
	}
}

// A host that never staged a run has no runfiles root. Looking for one creates nothing and asks
// the runtime nothing, so an unavailable runtime is no error there.
func TestReapRunfilesWithoutARootDoesNothing(t *testing.T) {
	cfg := &config.Config{BoxHome: filepath.Join(t.TempDir(), "box-home")}
	removed, err := ReapOrphanRunfiles(context.Background(), fakeMounts{err: errors.New("cannot connect to the docker daemon")}, cfg, time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("reap without a root = %d, %v; want 0, nil", removed, err)
	}
	if _, err := os.Stat(cfg.BoxHome); !os.IsNotExist(err) {
		t.Fatalf("looking for the runfiles root created state: %v", err)
	}
}

// Only coop's own temp entries are candidates. The temp directory is shared.
func TestReapTouchesOnlyCoopEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"important-user-data", "go-build123", ".hidden", "coopish-but-not-ours",
	} {
		writeEntry(t, dir, name, 72*time.Hour)
	}
	removed, err := ReapOrphanTempEntries(
		context.Background(), fakeMounts{sources: map[string]bool{}}, dir, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d entries that were not coop's", removed)
	}
}

// A launch names its daemon inside its run folder from the daemon probe it already makes, and again
// just before the box starts: the CLI starts the box on whichever daemon the current Docker context
// selects by then, and the record is all a later sweep can go on. After a `docker context use`
// while services or brokers start, the folder names the daemon the box started on; when no daemon
// can be named then, it names none, so the sweep keeps it.
func TestRunRecordsTheDaemonItsBoxStartsOn(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) (*config.Config, RunSpec){
		"ordinary": func(t *testing.T) (*config.Config, RunSpec) {
			return &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"},
				RunSpec{Image: "i", Repo: t.TempDir(), Workdir: "/workspace", Cmd: []string{"true"}, Batch: true, Quiet: true}
		},
	} {
		for _, tc := range []struct{ name, atStart, want string }{
			{name: "the same daemon", atStart: "echo daemon-a", want: "daemon-a\n"},
			{name: "another daemon after a context switch", atStart: "echo daemon-b", want: "daemon-b\n"},
			{name: "no daemon answers", atStart: "echo 'failed to connect to the docker API' >&2; exit 1", want: ""},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				cfg, spec := setup(t)
				dir := t.TempDir()
				probed, recorded := filepath.Join(dir, "probed"), filepath.Join(dir, "recorded-daemon")
				// Named docker: only the Docker kind is probed. The launch's probe answers daemon-a, the
				// one at the box's start answers per case, and the box reports its run folder's record.
				shim := filepath.Join(t.TempDir(), "docker")
				script := "#!/bin/sh\ncase \"$1\" in\n" +
					"info) [ \"$2 $3\" = '--format {{.ID}}' ] || exit 0\n" +
					"\tif [ -e " + strconv.Quote(probed) + " ]; then " + tc.atStart + "; else : > " + strconv.Quote(probed) + "; echo daemon-a; fi ;;\n" +
					"run) cat " + strconv.Quote(filepath.Join(cfg.ConfigDir, "runfiles")) + "/coop-run-*/" + runDaemonFile + " 2>/dev/null >> " + strconv.Quote(recorded) + " ;;\n" +
					"esac\nexit 0\n"
				if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				if code, err := Run(cfg, runtime.Runtime{Name: shim}, spec); code != 0 || err != nil {
					t.Fatalf("Run = %d, %v; want 0, nil", code, err)
				}
				if got, err := os.ReadFile(recorded); err != nil || string(got) != tc.want {
					t.Fatalf("the box started with its run folder naming %q (%v), want %q", got, err, tc.want)
				}
			})
		}
	}
}

// A run with native credentials starts its box through the daemon its credential owner is bound
// to, not through whatever the plain CLI would reach, so that is the daemon its run folder must
// name, at the box's start too. Here the plain probe answers with a different ID than the bound
// daemon reports.
func TestANativeRunRecordsTheDaemonItsOwnerIsBoundTo(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) (*config.Config, RunSpec){
		"ordinary": func(t *testing.T) (*config.Config, RunSpec) {
			cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open"}
			if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=inert-environment-secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			claude, _ := agents.Get("claude")
			return cfg, RunSpec{Image: "i", Repo: t.TempDir(), Cmd: claude.Interactive(cfg), Agent: "claude",
				AgentCommand: true, Homes: true, Batch: true, Quiet: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, spec := setup(t)
			dir := t.TempDir()
			fixture := nativeOnlineRuntime(t, filepath.Join(dir, "calls"), filepath.Join(dir, "box-env"))
			recorded := filepath.Join(dir, "recorded-daemon")
			// The fixture's plain `info --format` answers linux/amd64; its bound daemon is fixture-daemon.
			shim := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\nprev=\nfor arg in \"$@\"; do\n" +
				"\t[ \"$prev\" = --env-file ] && cat \"$(dirname \"$arg\")/" + runDaemonFile + "\" >> " + strconv.Quote(recorded) + "\n" +
				"\tprev=$arg\ndone\nexec " + strconv.Quote(fixture.Name) + " \"$@\"\n"
			if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if code, err := Run(cfg, runtime.Runtime{Name: shim}, spec); code != 0 || err != nil {
				t.Fatalf("native Run = %d, %v; want 0, nil", code, err)
			}
			if got, err := os.ReadFile(recorded); err != nil || string(got) != "fixture-daemon\n" {
				t.Fatalf("the native box started with its run folder naming %q (%v), want the bound fixture-daemon", got, err)
			}
		})
	}
}
