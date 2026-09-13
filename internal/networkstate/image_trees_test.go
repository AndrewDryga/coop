package networkstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageTreeDigestIsScopedToOneImageAndRoot(t *testing.T) {
	store := openStore(t)
	image := "sha256:" + strings.Repeat("1", 64)
	root := "/opt/coop/clients"
	tree := ImageTree{Size: 4096, SHA256: strings.Repeat("a", 64)}
	if _, ok := store.ImageTreeDigest(image, root); ok {
		t.Fatal("an absent record was returned")
	}
	if err := store.RememberImageTree(image, root, tree); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.ImageTreeDigest(image, root); !ok || got != tree {
		t.Fatal("record was not returned", got, ok)
	}
	for _, changed := range [][2]string{{"sha256:" + strings.Repeat("2", 64), root}, {image, "/opt/other"}} {
		if _, ok := store.ImageTreeDigest(changed[0], changed[1]); ok {
			t.Fatal("record escaped its identity", changed)
		}
	}
}

func TestImageTreeDigestTreatsDamageAsAMiss(t *testing.T) {
	store := openStore(t)
	image := "sha256:" + strings.Repeat("1", 64)
	root := "/opt/coop/clients"
	tree := ImageTree{Size: 4096, SHA256: strings.Repeat("a", 64)}
	if err := store.RememberImageTree(image, root, tree); err != nil {
		t.Fatal(err)
	}
	id, _ := store.imageTreeID(image, root)
	if err := os.WriteFile(filepath.Join(store.Path(), imageTreeRecordName(id)), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.ImageTreeDigest(image, root); ok {
		t.Fatal("a damaged record was returned")
	}
	if err := store.RememberImageTree(image, root, ImageTree{}); err == nil {
		t.Fatal("an empty digest was recorded")
	}
}
