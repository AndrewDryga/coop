package box

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// resetPinnedDigests keeps the per-image memo out of the next case: these tests
// deliberately reuse image IDs while changing what the image holds, which no
// real content-addressed ID ever does.
func resetPinnedDigests(t *testing.T) {
	t.Helper()
	drop := func() {
		pinnedDigests.Lock()
		defer pinnedDigests.Unlock()
		pinnedDigests.images = nil
	}
	drop()
	t.Cleanup(drop)
}

// fixtureCandidate is the qualification a filtered launch would carry on the
// fixture daemon: this host's locked client image, on linux/arm64.
func fixtureCandidate() networkstate.CandidateSpec {
	return networkstate.CandidateSpec{
		Runtime:     networkstate.RuntimeBinding{OS: "linux", Architecture: "arm64"},
		ClientImage: fixtureLockedImage, Libc: "glibc"}
}

const (
	fixtureLockedImage = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fixtureBuiltImage  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// derivedImageFixture gives the daemon a locked client image and an image built
// on it: the same pinned client files, plus one layer of the project's own.
func derivedImageFixture(t *testing.T, d *filteredDaemonFixture) agents.ClientClosure {
	t.Helper()
	resetPinnedDigests(t)
	closure, err := agents.LockedClientClosure(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]runtime.DockerFile{}
	for i, file := range pinnedClientFiles(closure) {
		files[file.path] = runtime.DockerFile{Mode: 0o755, Size: int64(1024 + i), SHA256: fmt.Sprintf("%064x", i+1)}
	}
	base := []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.images[fixtureLockedImage] = fixtureImage{id: fixtureLockedImage, layers: base, files: files}
	d.images[fixtureBuiltImage] = fixtureImage{id: fixtureBuiltImage,
		layers: append(slices.Clone(base), "sha256:"+strings.Repeat("c", 64)), files: maps.Clone(files)}
	return closure
}

// tamperBuiltImage rewrites what the built image holds, the way a project
// Dockerfile with a line too many would.
func tamperBuiltImage(d *filteredDaemonFixture, change func(*fixtureImage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	image := d.images[fixtureBuiltImage]
	image.layers = slices.Clone(image.layers)
	image.files = maps.Clone(image.files)
	change(&image)
	d.images[fixtureBuiltImage] = image
}

// The two proofs read the BUILT image, never the recipe: adding layers passes,
// and every way of not being the locked client image plus your own tools fails.
func TestDerivedImageProvesDerivationAndPinnedClients(t *testing.T) {
	_, d := filteredFixture(t)
	closure := derivedImageFixture(t, d)
	if err := proveDerivedImage(context.Background(), d, nil, fixtureLockedImage, fixtureBuiltImage, closure, ".agent/Dockerfile"); err != nil {
		t.Fatal("a project that only adds layers was refused", err)
	}
	launcher := closure.Clients[0].Launcher()
	native := closure.Clients[0].RequiredExecutables[0].Path
	for name, test := range map[string]struct {
		change func(*fixtureImage)
		want   string
	}{
		"unrelated image": {change: func(i *fixtureImage) { i.layers = []string{"sha256:" + strings.Repeat("f", 64)} },
			want: "did not build on coop's client image"},
		"base rewritten": {change: func(i *fixtureImage) { i.layers[0] = "sha256:" + strings.Repeat("f", 64) },
			want: "did not build on coop's client image"},
		"layers dropped": {change: func(i *fixtureImage) { i.layers = i.layers[:1] },
			want: "did not build on coop's client image"},
		"reordered base": {change: func(i *fixtureImage) { slices.Reverse(i.layers) },
			want: "did not build on coop's client image"},
		"replaced launcher": {change: func(i *fixtureImage) {
			file := i.files[launcher]
			file.SHA256 = strings.Repeat("9", 64)
			i.files[launcher] = file
		}, want: "changes claude's cli client at " + launcher},
		"wrapped launcher": {change: func(i *fixtureImage) {
			file := i.files[launcher]
			file.Size += 64
			i.files[launcher] = file
		}, want: "changes claude's cli client at " + launcher},
		"unreadable launcher": {change: func(i *fixtureImage) { delete(i.files, launcher) },
			want: launcher},
		"replaced native client": {change: func(i *fixtureImage) {
			file := i.files[native]
			file.SHA256 = strings.Repeat("9", 64)
			i.files[native] = file
		}, want: "at " + native},
		"unset executable bit": {change: func(i *fixtureImage) {
			file := i.files[native]
			file.Mode = 0o644
			i.files[native] = file
		}, want: "at " + native},
	} {
		t.Run(name, func(t *testing.T) {
			_, d := filteredFixture(t)
			closure := derivedImageFixture(t, d)
			tamperBuiltImage(d, test.change)
			err := proveDerivedImage(context.Background(), d, nil, fixtureLockedImage, fixtureBuiltImage, closure, ".agent/Dockerfile")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refusal did not name %q: %v", test.want, err)
			}
		})
	}
	// An image that is the locked one is not a derived one; neither is a missing
	// pair. Both stop before any container is created.
	for name, pair := range map[string][2]string{
		"identical":  {fixtureLockedImage, fixtureLockedImage},
		"no built":   {fixtureLockedImage, ""},
		"no locked":  {"", fixtureBuiltImage},
		"unknown id": {fixtureLockedImage, "sha256:" + strings.Repeat("e", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := proveDerivedImage(context.Background(), d, nil, pair[0], pair[1], closure, ".agent/Dockerfile"); err == nil {
				t.Fatal("an image that proved nothing was accepted")
			}
		})
	}
}

// The pinned files are hundreds of megabytes, and a loop launches many boxes in
// one process: an image ID is a content address, so one read per image is one
// read per proof.
func TestPinnedClientProofReadsEachImageOnce(t *testing.T) {
	_, d := filteredFixture(t)
	closure := derivedImageFixture(t, d)
	files := len(pinnedClientFiles(closure))
	for range 3 {
		if err := proveDerivedImage(context.Background(), d, nil, fixtureLockedImage, fixtureBuiltImage, closure, ".agent/Dockerfile"); err != nil {
			t.Fatal(err)
		}
	}
	d.mu.Lock()
	reads := maps.Clone(d.fileReads)
	d.mu.Unlock()
	if reads[fixtureLockedImage] != files || reads[fixtureBuiltImage] != files {
		t.Fatal("a proof re-read an image it had already identified", reads, files)
	}
	// A rebuild that produces different bytes produces a different ID, and that
	// is what the memo is keyed on — so the new image is read, not assumed.
	rebuilt := "sha256:" + strings.Repeat("3", 64)
	d.mu.Lock()
	image := d.images[fixtureBuiltImage]
	image.id = rebuilt
	d.images[rebuilt] = image
	d.mu.Unlock()
	if err := proveDerivedImage(context.Background(), d, nil, fixtureLockedImage, rebuilt, closure, ".agent/Dockerfile"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fileReads[rebuilt] != files || d.fileReads[fixtureLockedImage] != files {
		t.Fatal("a rebuilt image reused another image's identity", d.fileReads)
	}
}

// The filtered image is the project's own, on the locked client image: it must
// never be written to the tag an ordinary `coop build` owns, and a new locked
// image must not reuse the last one's build.
func TestFilteredProjectTagIsolatesTheOrdinaryImage(t *testing.T) {
	repo := t.TempDir()
	ordinary := ImageForRepo(repo, "coop-box", "")
	tag := filteredProjectTag(repo, fixtureLockedImage)
	if tag == ordinary || strings.HasPrefix(tag, ordinary+":") || !strings.HasPrefix(tag, ServicesProject(repo)+"-filtered:") {
		t.Fatal("filtered build collides with the ordinary project image", tag, ordinary)
	}
	if again := filteredProjectTag(repo, fixtureLockedImage); again != tag {
		t.Fatal("the same project on the same client image built a second tag", again, tag)
	}
	if next := filteredProjectTag(repo, fixtureBuiltImage); next == tag {
		t.Fatal("a new client image reused the old build", next)
	}
	if other := filteredProjectTag(filepath.Join(filepath.Dir(repo), "other-repo"), fixtureLockedImage); other == tag {
		t.Fatal("two projects share one filtered image", other)
	}
}

// Which Dockerfile a filtered launch builds is the one the ordinary build path
// would: .agent/Dockerfile, or box.dockerfile when it names a file that is there.
func TestFilteredProjectDockerfileFollowsTheOrdinaryBuild(t *testing.T) {
	for name, test := range map[string]struct{ yaml, file, want string }{
		"no dockerfile":    {},
		"default":          {file: ".agent/Dockerfile", want: ".agent/Dockerfile"},
		"box.dockerfile":   {yaml: "box:\n  dockerfile: docker/box.Dockerfile\n", file: "docker/box.Dockerfile", want: "docker/box.Dockerfile"},
		"named but absent": {yaml: "box:\n  dockerfile: docker/box.Dockerfile\n", file: ".agent/Dockerfile"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			if test.yaml != "" {
				writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), test.yaml)
			}
			if test.file != "" {
				writeCopyFixture(t, filepath.Join(repo, test.file), "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n")
			}
			if got := filteredProjectDockerfile(repo); got != test.want {
				t.Fatalf("built %q, want %q", got, test.want)
			}
		})
	}
	if got := filteredProjectDockerfile(""); got != "" {
		t.Fatal("a launch with no project built something", got)
	}
}

// Every pinned entry point is covered exactly once, and the launcher's own bytes
// carry the argv — so the list is the whole surface a client is reached through.
func TestPinnedClientFilesCoverEveryEntryPointOnce(t *testing.T) {
	closure, err := agents.LockedClientClosure(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	files := pinnedClientFiles(closure)
	covered := map[string]string{}
	for _, file := range files {
		if _, repeated := covered[file.path]; repeated {
			t.Fatal("one path is read twice", file.path)
		}
		if !strings.HasPrefix(file.path, "/") || file.owner == "" {
			t.Fatal("unnamed or relative pinned path", file)
		}
		covered[file.path] = file.owner
	}
	for _, client := range closure.Clients {
		for _, path := range append([]string{client.Launcher()}, client.Exec...) {
			if _, ok := covered[path]; !ok {
				t.Fatal("entry point left unproven", path)
			}
		}
		for _, executable := range client.RequiredExecutables {
			if _, ok := covered[executable.Path]; !ok {
				t.Fatal("required executable left unproven", executable.Path)
			}
		}
		if !strings.Contains(covered[client.Launcher()], client.Provider) {
			t.Fatal("a refusal would not name the client", client.Binary, covered[client.Launcher()])
		}
	}
	if len(files) < len(closure.Clients)*2 {
		t.Fatal("fewer proven paths than clients have entry points", len(files))
	}
}

// A project with no Dockerfile runs the locked client image itself: nothing is
// built, and nothing is proven about an image that was never made.
func TestFilteredProjectImageSkipsAProjectWithoutADockerfile(t *testing.T) {
	_, d := filteredFixture(t)
	repo := t.TempDir()
	image, err := filteredProjectImage(context.Background(), runtime.Runtime{Name: "must-not-execute"}, d, nil,
		RunSpec{Repo: repo}, fixtureCandidate())
	if image != "" || err != nil {
		t.Fatal("a project without a Dockerfile built an image", image, err)
	}
	// A Dockerfile with no qualified client image present stops before the build:
	// there is nothing to build ON.
	writeCopyFixture(t, filepath.Join(repo, ".agent", "Dockerfile"), "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n")
	if _, err := os.Stat(filepath.Join(repo, ".agent", "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	image, err = filteredProjectImage(context.Background(), runtime.Runtime{Name: "must-not-execute"}, d, nil,
		RunSpec{Repo: repo}, fixtureCandidate())
	if image != "" || err == nil || !strings.Contains(err.Error(), "disappeared while the box was starting") {
		t.Fatal("a missing client image was built on anyway", image, err)
	}
}

// imageFileRecords is every digest record this host has written, by file name.
func imageFileRecords(t *testing.T, store *networkstate.Store) []string {
	t.Helper()
	entries, err := os.ReadDir(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "imagefiles-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// The durable half of the memo. A second PROCESS reads neither image again,
// because this host already recorded what those exact image ids hold — and
// every way that record can be wrong (gone, damaged, or for another image)
// reads the image instead, which is the only fallback there is.
func TestPinnedClientProofReusesThisHostsRecordInANewProcess(t *testing.T) {
	f, d := filteredFixture(t)
	closure := derivedImageFixture(t, d)
	files := len(pinnedClientFiles(closure))
	prove := func() error {
		return proveDerivedImage(context.Background(), d, f.store, fixtureLockedImage, fixtureBuiltImage, closure, ".agent/Dockerfile")
	}
	reads := func() (int, int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.fileReads[fixtureLockedImage], d.fileReads[fixtureBuiltImage]
	}
	if err := prove(); err != nil {
		t.Fatal(err)
	}
	if locked, built := reads(); locked != files || built != files {
		t.Fatalf("the first proof read %d locked and %d built files, want %d of each", locked, built, files)
	}
	if names := imageFileRecords(t, f.store); len(names) != 2 {
		t.Fatalf("recorded %v, want one record per image", names)
	}
	// A NEW process: the in-memory memo is gone, this host's record is not.
	resetPinnedDigests(t)
	if err := prove(); err != nil {
		t.Fatal(err)
	}
	if locked, built := reads(); locked != files || built != files {
		t.Fatalf("a second process re-read the images (%d/%d) instead of using this host's record", locked, built)
	}
	// Damaged: the bytes are there and unusable. That is a miss, so the images
	// are read again — and the proof still holds.
	for _, name := range imageFileRecords(t, f.store) {
		if err := os.WriteFile(filepath.Join(f.store.Path(), name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resetPinnedDigests(t)
	if err := prove(); err != nil {
		t.Fatal(err)
	}
	if locked, built := reads(); locked != 2*files || built != 2*files {
		t.Fatalf("a damaged record was used instead of read past (%d/%d)", locked, built)
	}
	// Gone: the same fallback, for the same reason.
	for _, name := range imageFileRecords(t, f.store) {
		if err := os.Remove(filepath.Join(f.store.Path(), name)); err != nil {
			t.Fatal(err)
		}
	}
	resetPinnedDigests(t)
	if err := prove(); err != nil {
		t.Fatal(err)
	}
	if locked, built := reads(); locked != 3*files || built != 3*files {
		t.Fatalf("a missing record did not fall back to reading (%d/%d)", locked, built)
	}
}

// A record is keyed by the image ID, which is a content address: a rebuild that
// changes a pinned client is a DIFFERENT id, so it is read, and refused by name.
func TestPinnedClientProofReadsARebuiltImageItNeverRecorded(t *testing.T) {
	f, d := filteredFixture(t)
	closure := derivedImageFixture(t, d)
	launcher := closure.Clients[0].Launcher()
	if err := proveDerivedImage(context.Background(), d, f.store, fixtureLockedImage, fixtureBuiltImage, closure, ".agent/Dockerfile"); err != nil {
		t.Fatal(err)
	}
	rebuilt := "sha256:" + strings.Repeat("3", 64)
	d.mu.Lock()
	image := d.images[fixtureBuiltImage]
	image.id, image.files = rebuilt, maps.Clone(image.files)
	file := image.files[launcher]
	file.SHA256 = strings.Repeat("9", 64)
	image.files[launcher] = file
	d.images[rebuilt] = image
	d.mu.Unlock()
	resetPinnedDigests(t)
	err := proveDerivedImage(context.Background(), d, f.store, fixtureLockedImage, rebuilt, closure, ".agent/Dockerfile")
	if err == nil || !strings.Contains(err.Error(), "changes claude's cli client at "+launcher) {
		t.Fatalf("a rebuilt image that replaced a client was accepted: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fileReads[rebuilt] == 0 {
		t.Fatal("the rebuilt image was never read")
	}
}

// What `coop net setup` contributes: the locked image is read once, at setup,
// where the daemon is already in hand — so the FIRST filtered launch of a
// project with its own Dockerfile reads only the image that Dockerfile built.
func TestSetupRecordsTheLockedClientDigestsForTheFirstLaunch(t *testing.T) {
	f, d := filteredFixture(t)
	closure := derivedImageFixture(t, d)
	files := len(pinnedClientFiles(closure))
	// A memo that works costs the transcript no line.
	if line := setupClientFiles(context.Background(), d, f.store, fixtureCandidate(), closure); line != "" {
		t.Fatalf("setup line = %q, want silence", line)
	}
	if names := imageFileRecords(t, f.store); len(names) != 1 {
		t.Fatalf("setup recorded %v, want exactly the locked image", names)
	}
	// A launch in another process: only the built image is read.
	resetPinnedDigests(t)
	if err := proveDerivedImage(context.Background(), d, f.store, fixtureLockedImage, fixtureBuiltImage, closure, ".agent/Dockerfile"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fileReads[fixtureLockedImage] != files || d.fileReads[fixtureBuiltImage] != files {
		t.Fatalf("the first launch read %d locked and %d built files, want the locked side read only by setup",
			d.fileReads[fixtureLockedImage], d.fileReads[fixtureBuiltImage])
	}
}

// Setup is not the place a missing client entry point is refused: it says so and
// carries on, because the launch that needs those digests reads them itself.
func TestSetupSaysWhenTheLockedClientDigestsCannotBeRead(t *testing.T) {
	f, d := filteredFixture(t)
	closure := derivedImageFixture(t, d)
	launcher := closure.Clients[0].Launcher()
	d.mu.Lock()
	image := d.images[fixtureLockedImage]
	image.files = maps.Clone(image.files)
	delete(image.files, launcher)
	d.images[fixtureLockedImage] = image
	d.mu.Unlock()
	line := setupClientFiles(context.Background(), d, f.store, fixtureCandidate(), closure)
	if !strings.Contains(line, "could not be recorded") || !strings.Contains(line, launcher) {
		t.Fatalf("setup line = %q, want it to name the entry point it could not read", line)
	}
	if names := imageFileRecords(t, f.store); len(names) != 0 {
		t.Fatalf("a partial read was recorded: %v", names)
	}
}
