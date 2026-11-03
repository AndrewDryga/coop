package box

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/fsfilter"
)

// The volume profile is measured beside Coop's private state, never in the project, and must
// match what the volume itself does with another letter case and another Unicode form.
func TestFilesystemVolumeProfileMeasuresPrivately(t *testing.T) {
	root, private := t.TempDir(), t.TempDir()
	profile, err := filesystemVolumeProfile(root, private)
	if err != nil {
		t.Fatal(err)
	}
	oracle := t.TempDir()
	if err := os.WriteFile(filepath.Join(oracle, "Caf\u00e9"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	base, err := os.Stat(filepath.Join(oracle, "Caf\u00e9"))
	if err != nil {
		t.Fatal(err)
	}
	same := func(name string) bool {
		info, err := os.Stat(filepath.Join(oracle, name))
		return err == nil && os.SameFile(base, info)
	}
	if want := (fsfilter.Profile{Fold: same("CAF\u00c9") || same("cAF\u00e9"), Normalize: same("Cafe\u0301")}); profile != want {
		t.Fatalf("profile = %+v, the volume behaves as %+v", profile, want)
	}
	for _, dir := range []string{root, private} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("measuring left files in %s: %v %v", dir, entries, err)
		}
	}
}

// A root on another device cannot be measured privately, so it gets both equivalences: the policy
// may deny spellings that volume would not resolve, but never misses one it would.
func TestFilesystemVolumeProfileAssumesBothOnAnotherDevice(t *testing.T) {
	private := t.TempDir()
	rootInfo, rootErr := os.Stat("/dev")
	privateInfo, privateErr := os.Stat(private)
	if rootErr != nil || privateErr != nil {
		t.Skip("no second device to compare against")
	}
	rootID, _ := identityOf(rootInfo)
	privateID, _ := identityOf(privateInfo)
	if rootID.dev == privateID.dev {
		t.Skip("/dev shares the temporary directory's device here")
	}
	profile, err := filesystemVolumeProfile("/dev", private)
	if err != nil || profile != (fsfilter.Profile{Fold: true, Normalize: true}) {
		t.Fatalf("profile = %+v, %v; want both equivalences", profile, err)
	}
	if entries, err := os.ReadDir(private); err != nil || len(entries) != 0 {
		t.Fatalf("measuring left files privately: %v %v", entries, err)
	}
}
