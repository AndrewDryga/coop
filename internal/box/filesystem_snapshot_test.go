package box

import (
	"context"
	"os"
	"path/filepath"
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
