package box

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

func TestFilesystemSnapshotPreservesDeciderAndClosesExistingAliases(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".coopignore", ".env.example\n")
	if err := os.Link(filepath.Join(repo, "secrets/api-token"), filepath.Join(repo, "public-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(repo, ".coopignore"), filepath.Join(repo, "policy-alias")); err != nil {
		t.Fatal(err)
	}
	s, err := captureFilesystemSnapshot(repo, fsfilter.Profile{Fold: true, Normalize: true}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]uint32{}
	for _, e := range s.policy.Entries {
		flags[e.Path] = e.Flags
	}
	for _, name := range []string{".env", ".env.example", "secrets", "secrets/api-token", "public-alias", "deploy.pem"} {
		if flags[name]&fsfilter.Secret == 0 {
			t.Errorf("lost secret %s", name)
		}
	}
	for _, name := range []string{".coopignore", "policy-alias"} {
		if flags[name] != fsfilter.ReadOnly {
			t.Errorf("policy alias %s = %d", name, flags[name])
		}
	}
	for _, name := range []string{"src/app.js", ".git/config"} {
		if flags[name] != 0 {
			t.Errorf("public path %s hidden", name)
		}
	}
	if _, err := captureFilesystemSnapshot(repo, s.policy.Profile, []string{"policy-alias"}, nil, ""); err == nil {
		t.Fatal("policy alias granted")
	}
	if _, err := captureFilesystemSnapshot(repo, s.policy.Profile, []string{"secrets"}, nil, ""); err == nil {
		t.Fatal("directory granted")
	}
	if _, err := captureFilesystemSnapshot(repo, s.policy.Profile, []string{"secrets/api-token"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	write("src/app.js", "ordinary public edit")
	write(".env", "ordinary host in-place secret edit")
	if err := s.revalidate(""); err != nil {
		t.Fatal(err)
	}
	write("new.pem", "new secret")
	if err := s.revalidate(""); err == nil {
		t.Fatal("new secret did not require admission")
	}
}

func TestFilesystemSnapshotInventoriesPrunedDirectoriesWithoutReadingTheirPolicies(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	for _, dir := range []string{"secrets", ".git"} {
		// A directory at the policy filename is unreadable as policy content.
		// The mount walk prunes these subtrees before policy discovery.
		if err := os.Mkdir(filepath.Join(repo, dir, ".coopignore"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, dir, "private.pem"), []byte("inert"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(repo, "secrets/private.pem"), filepath.Join(repo, "public-name")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureFilesystemSnapshot(repo, fsfilter.Profile{}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]uint32{}
	for _, e := range snapshot.policy.Entries {
		flags[e.Path] = e.Flags
	}
	if flags["public-name"]&fsfilter.Secret == 0 || flags["secrets/private.pem"]&fsfilter.Secret == 0 || flags[".git/private.pem"] != 0 {
		t.Fatal(flags)
	}
	if _, ok := snapshot.objects[".git/private.pem"]; !ok {
		t.Fatal("git identity inventory skipped")
	}
	if err := os.Mkdir(filepath.Join(repo, "src/.coopignore"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := captureFilesystemSnapshot(repo, fsfilter.Profile{}, nil, nil, ""); err == nil {
		t.Fatal("unreadable public policy accepted")
	}
}

func TestFilesystemMovedAliasDoesNotSuppressGoverningPolicyError(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	secret := filepath.Join(repo, "key.pem")
	if err := os.WriteFile(secret, []byte("inert"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureRememberedFilesystemSnapshot(t.Context(), repo, fsfilter.Profile{}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "public"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(secret, filepath.Join(repo, "public", "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "public", ".coopignore"), []byte(strings.Repeat("# oversized policy\n", 70000)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := computeMounts(repo, "/workspace", false); err == nil {
		t.Fatal("fixture did not produce a mount-policy error")
	}
	if _, err := captureFilesystemSnapshot(repo, fsfilter.Profile{}, nil, nil, ""); err == nil {
		t.Fatal("remembered alias suppressed policy error")
	}
}

func TestFilesystemSnapshotRejectsAtomicReplacementAndRetainsOldAliases(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	alias := filepath.Join(repo, "a-alias")
	if err := os.Mkdir(filepath.Join(repo, "z"), 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(repo, "z", "credential.pem")
	if err := os.WriteFile(secret, []byte("inert secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, alias); err != nil {
		t.Fatal(err)
	}
	s, err := captureFilesystemSnapshot(repo, fsfilter.Profile{}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(repo, "temporary")
	if err := os.WriteFile(temporary, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, secret); err != nil {
		t.Fatal(err)
	}
	if err := s.revalidate(""); err == nil {
		t.Fatal("changed protected identity accepted during admission")
	}
	if err := s.remember(); err != nil {
		t.Fatal(err)
	}
	next, err := captureFilesystemSnapshot(repo, s.policy.Profile, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range next.policy.Entries {
		if e.Path == "a-alias" && e.Flags&fsfilter.Secret != 0 {
			return
		}
	}
	t.Fatal("retry forgot the old protected alias")
}

func TestFilesystemConcurrentAdmissionsKeepCapturedAliasIdentity(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	secret := filepath.Join(repo, "deploy.pem")
	if err := os.Link(secret, filepath.Join(repo, "a-alias")); err != nil {
		t.Fatal(err)
	}
	captured := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		first <- withHiddenFilesLock(ctx, repo, func() error {
			snapshot, err := captureFilesystemSnapshot(repo, fsfilter.Profile{}, nil, nil, "")
			close(captured)
			if err != nil {
				return err
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return snapshot.remember()
		})
	}()
	select {
	case <-captured:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	next := filepath.Join(repo, "replacement")
	if err := os.WriteFile(next, []byte("inert new secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, secret); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() {
		_, err := captureRememberedFilesystemSnapshot(ctx, repo, fsfilter.Profile{}, nil, "")
		second <- err
	}()
	close(release)
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	hidden, err := NewShadowDecider(repo)("a-alias")
	if err != nil || !hidden {
		t.Fatalf("concurrent launch forgot existing secret alias: %t %v", hidden, err)
	}
}

// protectsSecret reports whether the snapshot's policy hides path as a secret.
func protectsSecret(s *filesystemSnapshot, path string) bool {
	for _, entry := range s.policy.Entries {
		if entry.Path == path && entry.Flags&fsfilter.Secret != 0 {
			return true
		}
	}
	return false
}

// recordedProbe is a probe directory at repo that another live admission made and recorded, as a
// parallel launch in the same project does (an editor's warm boxes start together).
func recordedProbe(t *testing.T, repo string) *filesystemProbe {
	t.Helper()
	supervisor := supervisorLabelValue(workspaceScope(repo), os.Getpid())
	if supervisor == "" {
		t.Skip("this host has no stable process identity, so no admission can be recorded")
	}
	return recordedProbeBy(t, repo, supervisor)
}

// recordedProbeBy is recordedProbe for an admission that supervisor (a supervisor label) runs.
func recordedProbeBy(t *testing.T, repo, supervisor string) *filesystemProbe {
	t.Helper()
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := "coop-fs-" + strings.Repeat("ab", 16)
	dir, err := os.MkdirTemp(state, parent+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	other := &filesystemAdmission{dir: dir, record: filesystemRecord{Version: 1, Parent: parent, Endpoint: "unix:///docker.sock",
		DaemonID: "daemon-a", Helper: filesystemRef(parent, "owner"), Workspace: repo, Supervisor: supervisor}}
	probe, err := newFilesystemProbe(repo, func(record filesystemProbeRecord) error {
		other.record.Probes = append(other.record.Probes, record)
		return other.persist()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.close() })
	return probe
}

// Each launch makes a probe directory at the project root and removes it once its shares qualify.
// A parallel launch's walk took that directory, and the .env inside it, for project content, so
// when the other launch removed it the final check failed with "lstat <repo>/.coop-fs-probe-…: no
// such file or directory". Another live admission's recorded probe is no part of a snapshot,
// whether it is still there or already gone.
func TestFilesystemSnapshotLeavesOutAnotherAdmissionsProbe(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	other := recordedProbe(t, repo)
	own, err := newFilesystemProbe(repo, func(filesystemProbeRecord) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer own.close()
	snapshot, err := captureRememberedFilesystemSnapshot(t.Context(), repo, own.profile, nil, own.relative)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range snapshot.policy.Entries {
		if strings.HasPrefix(entry.Path, other.relative) {
			t.Fatalf("the snapshot protects another admission's probe: %+v", entry)
		}
	}
	if !protectsSecret(snapshot, ".env") {
		t.Fatal("the snapshot lost the project's own .env")
	}
	if err := other.close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.revalidate(own.relative); err != nil {
		t.Fatalf("final check after the other admission retired its probe: %v", err)
	}
}

// A probe-named folder no live admission recorded is project content like any other, so its
// secret is protected, and one that vanishes mid-walk fails the walk as before.
func TestFilesystemSnapshotWalksAnUnrecordedProbeName(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	planted := filepath.Join(repo, ".coop-fs-probe-planted")
	if err := os.Mkdir(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, ".env"), []byte("SECRET=x"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureFilesystemSnapshot(repo, fsfilter.Profile{}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !protectsSecret(snapshot, ".coop-fs-probe-planted/.env") {
		t.Fatalf("an unrecorded probe-named folder's secret is unprotected: %+v", snapshot.policy.Entries)
	}
}

// What a walk cannot read under a probe-named folder waits until the walk knows whose folder it is:
// a live admission's is left out, while anyone else's still fails the walk.
func TestFilesystemWalkJudgesAnUnreadableProbeByItsRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	pinHome(t)
	repo := fixture(t)
	probe := recordedProbe(t, repo)
	locked := filepath.Join(repo, probe.relative)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	inventory, err := filesystemObjects(repo, "")
	if err != nil {
		t.Fatalf("a live admission's unreadable probe failed the walk: %v", err)
	}
	if _, ok := inventory.objects[probe.relative]; ok {
		t.Fatal("a live admission's probe is in the inventory")
	}

	planted := filepath.Join(repo, filesystemProbePrefix+"planted")
	if err := os.Mkdir(planted, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(planted, 0o755) })
	if _, err := filesystemObjects(repo, ""); err == nil {
		t.Fatal("an unreadable probe-named folder nobody recorded passed the walk")
	}
}

// A dead admission's probe is walked like project content: its owner can no longer retire it, so
// anything planted there stays protected.
func TestFilesystemSnapshotWalksADeadAdmissionsProbe(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	dead := recordedProbeBy(t, repo, goneSupervisorLabel(t, repo))
	snapshot, err := captureFilesystemSnapshot(repo, fsfilter.Profile{}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !protectsSecret(snapshot, dead.relative+"/.env") {
		t.Fatalf("a dead admission's probe was left out: %+v", snapshot.policy.Entries)
	}
}

// leaveOutProbes drops a live admission's probe whether the walk read it, lost it mid-read or found
// it gone, keeps any other probe-named entry as content, and fails the walk on what it could not
// read under one that is not a live admission's.
func TestLeaveOutProbesDropsOnlyALiveAdmissionsProbe(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	probe := recordedProbe(t, repo)
	info, err := os.Lstat(filepath.Join(repo, probe.relative))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := identityOf(info)
	other := fileIdentity{dev: id.dev, ino: id.ino + 1}
	planted := filesystemProbePrefix + "planted"
	vanished := &fs.PathError{Op: "lstat", Path: probe.relative, Err: fs.ErrNotExist}
	for _, tc := range []struct {
		name               string
		leaf               string
		seen               *fileIdentity // nil: the walk found the entry gone
		unsettled, dropped bool
		fails              bool
	}{
		{name: "a live probe the walk read", leaf: probe.relative, seen: &id, dropped: true},
		{name: "a live probe emptied while the walk read it", leaf: probe.relative, seen: &id, unsettled: true, dropped: true},
		{name: "a live probe gone before the walk read it", leaf: probe.relative, unsettled: true, dropped: true},
		{name: "a probe-named folder nobody recorded", leaf: planted, seen: &other},
		{name: "an unreadable probe-named folder nobody recorded", leaf: planted, seen: &other, unsettled: true, fails: true},
		{name: "another folder under a live probe's name", leaf: probe.relative, seen: &other, unsettled: true, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := map[string]filesystemObject{".": {mode: fs.ModeDir}, "src": {mode: fs.ModeDir}}
			children := map[string][]string{".": {"src"}}
			if tc.seen != nil {
				objects[tc.leaf] = filesystemObject{id: *tc.seen, mode: fs.ModeDir}
				objects[tc.leaf+"/.env"] = filesystemObject{}
				children["."] = append(children["."], tc.leaf)
				children[tc.leaf] = []string{tc.leaf + "/.env"}
			}
			unsettled := map[string]error{}
			if tc.unsettled {
				unsettled[tc.leaf] = vanished
			}
			err := leaveOutProbes(repo, objects, children, unsettled)
			if (err != nil) != tc.fails {
				t.Fatalf("leaveOutProbes = %v, want a failure: %v", err, tc.fails)
			}
			if tc.fails {
				return
			}
			_, kept := objects[tc.leaf+"/.env"]
			listed := slices.Contains(children["."], tc.leaf)
			if tc.dropped && (kept || listed) || !tc.dropped && (!kept || !listed) {
				t.Fatalf("probe kept=%v listed=%v, want dropped=%v", kept, listed, tc.dropped)
			}
			if _, ok := objects["src"]; !ok || !slices.Contains(children["."], "src") {
				t.Fatal("the project's own folder was dropped")
			}
		})
	}
}

func TestFilesystemAliasesSurviveMountBackendPersistence(t *testing.T) {
	pinHome(t)
	repo := fixture(t)
	secret := filepath.Join(repo, "secrets", "api-token")
	alias := filepath.Join(repo, ".git", "alias")
	if err := os.Link(secret, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := captureRememberedFilesystemSnapshot(t.Context(), repo, fsfilter.Profile{}, nil, ""); err != nil {
		t.Fatal(err)
	}
	mounts, err := computeMounts(repo, "/workspace", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := RememberHiddenFiles(repo, "/workspace", mounts); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(repo, "replacement")
	if err := os.WriteFile(next, []byte("new inert secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, secret); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRememberedFilesystemSnapshot(t.Context(), repo, fsfilter.Profile{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range snapshot.policy.Entries {
		if entry.Path == ".git/alias" && entry.Flags&fsfilter.Secret != 0 {
			return
		}
	}
	t.Fatal("mount backend discarded the hidden descendant's existing alias")
}

func TestFilesystemRuleRemovalRespectsSurvivingAliases(t *testing.T) {
	for _, alias := range []bool{false, true} {
		name := "without alias"
		if alias {
			name = "with remembered alias"
		}
		t.Run(name, func(t *testing.T) {
			pinHome(t)
			repo := t.TempDir()
			policy := filepath.Join(repo, ".coopignore")
			if err := os.WriteFile(policy, []byte("private-data\n"), 0600); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(repo, "private-data")
			if err := os.WriteFile(original, []byte("inert fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if alias {
				if err := os.Link(original, filepath.Join(repo, "other-name")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := captureRememberedFilesystemSnapshot(t.Context(), repo, fsfilter.Profile{}, nil, ""); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(policy, nil, 0600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := captureRememberedFilesystemSnapshot(t.Context(), repo, fsfilter.Profile{}, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			hidden := map[string]bool{}
			for _, entry := range snapshot.policy.Entries {
				hidden[entry.Path] = entry.Flags&fsfilter.Secret != 0
			}
			if hidden["private-data"] != alias || hidden["other-name"] != alias {
				t.Fatalf("unexpected declassification: %v", hidden)
			}
			if got := len(loadHiddenFiles(repo)); got != 0 && !alias || got != 1 && alias {
				t.Fatalf("remembered %d identities after rule removal", got)
			}
		})
	}
}

func TestFilesystemFinalInventoryPreservesReclassificationChecks(t *testing.T) {
	for _, mutation := range []string{"public replacement", "new secret", "secret replacement", "grant replacement", "ancestor replacement", "new hidden descendant", "new policy", "unreadable policy", "new remembered identity", "unhidden policy error", "new secret alias", "new policy alias"} {
		t.Run(mutation, func(t *testing.T) {
			pinHome(t)
			repo := t.TempDir()
			write := func(name, value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, dir := range []string{"nested", "secrets", "hidden"} {
				if err := os.Mkdir(filepath.Join(repo, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("nested/.env", "inert")
			write(".env.granted", "grant")
			write("public", "public")
			write(".coopignore", "hidden\n")
			if err := os.Mkdir(filepath.Join(repo, "hidden/.coopignore"), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := captureRememberedFilesystemSnapshot(t.Context(), repo, fsfilter.Profile{}, []string{".env.granted"}, "")
			if err != nil {
				t.Fatal(err)
			}
			replace := func(name string) {
				t.Helper()
				write("replacement", "replacement")
				if err := os.Rename(filepath.Join(repo, "replacement"), filepath.Join(repo, name)); err != nil {
					t.Fatal(err)
				}
			}
			wantOK := false
			switch mutation {
			case "public replacement":
				replace("public")
				wantOK = true
			case "new secret":
				write("new.pem", "inert")
			case "secret replacement":
				replace("nested/.env")
			case "grant replacement":
				replace(".env.granted")
			case "ancestor replacement":
				if err := os.Rename(filepath.Join(repo, "nested"), filepath.Join(repo, "old")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, "nested"), 0700); err != nil {
					t.Fatal(err)
				}
				write("nested/.env", "inert")
			case "new hidden descendant":
				write("secrets/new", "inert")
			case "new policy":
				write("nested/.coopignore", "public\n")
			case "unreadable policy":
				if err := os.Mkdir(filepath.Join(repo, "nested/.coopignore"), 0700); err != nil {
					t.Fatal(err)
				}
			case "unhidden policy error":
				write(".coopignore", "")
			case "new remembered identity":
				write(".coopignore", "hidden\npublic\n")
				if _, err := captureRememberedFilesystemSnapshot(t.Context(), repo, fsfilter.Profile{}, nil, ""); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(repo, "public"), filepath.Join(repo, "ordinary")); err != nil {
					t.Fatal(err)
				}
				write(".coopignore", "hidden\n")
			case "new secret alias", "new policy alias":
				source := "nested/.env"
				if mutation == "new policy alias" {
					source = ".coopignore"
				}
				if err := os.Link(filepath.Join(repo, source), filepath.Join(repo, "alias")); err != nil {
					t.Fatal(err)
				}
				wantOK = filesystemProfileCandidate(repo)
			}
			if err := s.revalidate(""); (err == nil) != wantOK {
				t.Fatalf("validation = %v, want allowed %t", err, wantOK)
			}
		})
	}
}
