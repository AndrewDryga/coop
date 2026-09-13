package box

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A filtered box may run a project's OWN box image — but only one this host can
// prove is the locked client image plus the project's own layers. The project's
// Dockerfile is built with COOP_BASE_IMAGE pointing at the locked image, through
// the ordinary project build path, and then two proofs stand between that image
// and the launch. NEITHER reads the Dockerfile: a `FROM` line is a claim about
// what was built, and this file only accepts evidence from the built image.
//
//  1. Derivation — the locked image's rootfs layer chain must be a PREFIX of the
//     built image's (`proveDerivedImage`). Layers are content addresses, so this
//     is what "built on top of exactly that image" means.
//  2. Pinned clients — every locked client's launcher, its exec entry points and
//     its required native executables must be byte-identical in both images
//     (`provePinnedClients`), read out of a container that is created and never
//     started. Adding tools passes; replacing, wrapping or deleting a pinned
//     client is refused by name.
//
// The host qualification keeps naming the LOCKED image. The derived image is
// never recorded as qualified: it inherits nothing except through these proofs,
// re-run on every launch.

// maxPinnedClientBytes bounds one pinned entry point. The native client binaries
// are a few hundred megabytes; anything past this is not one of them.
const maxPinnedClientBytes = 512 << 20

// The installed npm tree is much larger than one launcher, but still finite.
// Hash the stream instead of retaining it in memory.
const maxPinnedClientTreeBytes = 4 << 30

// filteredProjectDockerfile returns the repo-relative box Dockerfile a filtered
// launch must build, or "" when the project has none and the locked client image
// runs as-is. A `box.dockerfile` naming a file that is not there is "none", the
// same answer the ordinary build path gives it.
func filteredProjectDockerfile(repo string) string {
	if repo == "" {
		return ""
	}
	rel := project.DockerfilePath(repo)
	if !fileExists(filepath.Join(repo, rel)) {
		return ""
	}
	return rel
}

// filteredProjectTag names what a filtered build produces: the project's own
// image name, marked filtered, plus the identity of the locked client image it
// was built on. So an ordinary `coop build` and a filtered launch of the same
// repository never write the same tag, and a new locked image (new clients, new
// base) builds a new image instead of reusing the one before it.
func filteredProjectTag(repo, lockedImage string) string {
	sum := sha256.Sum256([]byte(lockedImage))
	return ServicesProject(repo) + "-filtered:" + hex.EncodeToString(sum[:])[:16]
}

// filteredProjectImage builds this project's box Dockerfile on the locked client
// image and returns the built image's ID once both proofs hold. It returns ""
// for a project with no Dockerfile, which runs the locked image itself.
func filteredProjectImage(ctx context.Context, rt runtime.Runtime, docker filteredDocker, store *networkstate.Store, spec RunSpec, candidate networkstate.CandidateSpec) (string, error) {
	if spec.Login {
		return "", nil // sign-in uses the locked client, never the project's build instructions
	}
	repo := projectPolicyRepo(spec)
	// The PROJECT's Dockerfile, not the workspace's: a remote session's box
	// mounts a fork of this project, and what defines the box belongs to the
	// directory a human approved, not to the copy an agent has been writing in.
	dfRel := filteredProjectDockerfile(repo)
	if dfRel == "" {
		return "", nil
	}
	definition, _, closure, err := lockedImageDefinition(agents.ClientPlatform{
		OS: candidate.Runtime.OS, Architecture: candidate.Runtime.Architecture, Libc: candidate.Libc})
	if err != nil {
		return "", err
	}
	// Docker cannot build FROM an image ID, so the build gets the locked image's
	// pinned tag — and this resolves that tag to the qualified ID first, so a
	// replaced tag is caught here rather than deep inside a build. The layer
	// proof below is the backstop either way.
	if id, _, err := docker.Image(ctx, definition.Tag); err != nil || id != candidate.ClientImage {
		return "", errors.New("the images this host was set up with disappeared while the box was starting — run it again")
	}
	tag := filteredProjectTag(repo, candidate.ClientImage)
	var buildErrOut io.Writer
	if !spec.Quiet {
		// Unlike `coop build`, this build is not a human action — a filtered launch
		// runs it. The two proofs keep the clients and the base honest, but the
		// project's own layers are still code that runs as root at build time, so
		// say when nobody has committed the file that defines them.
		if fileUntracked(repo, dfRel) {
			ui.Warning("The project Dockerfile is not tracked by Git",
				dfRel+" controls what is installed in the box.",
				"Review the file before using this image.")
		}
		// A first build takes minutes. Silence reads as a hung launch, so the
		// operator gets the same narration `coop build` gives them.
		ui.Section("Building the project box")
		ui.Note("  Using %s", dfRel)
		buildErrOut = os.Stderr
	}
	if err := buildProjectOnBase(rt, repo, dfRel, tag, definition.Tag, buildErrOut); err != nil {
		return "", fmt.Errorf("this project's %s did not build on coop's client image: %w", dfRel, err)
	}
	if !spec.Quiet {
		ui.Pass("Project box built")
	}
	built, _, err := docker.Image(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("the image %s built from this project's %s cannot be read back — run it again", tag, dfRel)
	}
	if err := proveDerivedImage(ctx, docker, store, candidate.ClientImage, built, closure, dfRel); err != nil {
		return "", err
	}
	return built, nil
}

// proveDerivedImage refuses everything the built image cannot show it inherited.
func proveDerivedImage(ctx context.Context, docker filteredDocker, store *networkstate.Store, locked, built string, closure agents.ClientClosure, dfRel string) error {
	if locked == "" || built == "" || locked == built {
		// The same image means the Dockerfile added nothing at all — and an image
		// that adds nothing was not built on anything either.
		return notDerived(dfRel)
	}
	if err := proveDerivedLayers(ctx, docker, locked, built, dfRel); err != nil {
		return err
	}
	return provePinnedClients(ctx, docker, store, locked, built, closure, dfRel)
}

// proveDerivedLayers reads both layer chains from the runtime. A derived image
// carries its base's layers, in order, before its own.
func proveDerivedLayers(ctx context.Context, docker filteredDocker, locked, built, dfRel string) error {
	unreadable := fmt.Errorf("coop could not check the image this project's %s built against its own client image — run it again, or run without --egress filtered", dfRel)
	lockedID, base, err := docker.ImageLayers(ctx, locked)
	if err != nil || lockedID != locked {
		return unreadable
	}
	builtID, layers, err := docker.ImageLayers(ctx, built)
	if err != nil || builtID != built {
		return unreadable
	}
	if len(layers) < len(base) {
		return notDerived(dfRel)
	}
	for i, layer := range base {
		if layers[i] != layer {
			return notDerived(dfRel)
		}
	}
	return nil
}

func notDerived(dfRel string) error {
	return fmt.Errorf("this project's %s did not build on coop's client image — start it with `ARG COOP_BASE_IMAGE` and `FROM ${COOP_BASE_IMAGE}`, or run without --egress filtered", dfRel)
}

// pinnedFile is one path a filtered launch will not accept a change to, and the
// client it belongs to — so a refusal names what was replaced, not just a path.
type pinnedFile struct{ path, owner string }

// pinnedClientFiles lists every entry point the qualification's clients run
// through: the absolute launcher coop installed, each element of its exec argv
// (the interpreter included — swapping node swaps the client), and the native
// executables the client requires. Paths are deduplicated in a stable order, so
// the codex CLI and its ACP adapter share one read of the binary they share.
func pinnedClientFiles(closure agents.ClientClosure) []pinnedFile {
	var files []pinnedFile
	seen := make(map[string]bool)
	add := func(path, owner string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		files = append(files, pinnedFile{path: path, owner: owner})
	}
	for _, client := range closure.Clients {
		owner := fmt.Sprintf("%s's %s client", client.Provider, client.Client)
		add(client.Launcher(), owner)
		for _, arg := range client.Exec {
			add(arg, owner)
		}
		for _, executable := range client.RequiredExecutables {
			add(executable.Path, owner)
		}
	}
	return files
}

// provePinnedClients compares each pinned entry point in the built image against
// the same path in the locked one. A project that ADDS tools leaves every one of
// them untouched; one that replaces, wraps or deletes a client changes exactly
// these files.
func provePinnedClients(ctx context.Context, docker filteredDocker, store *networkstate.Store, locked, built string, closure agents.ClientClosure, dfRel string) error {
	files := pinnedClientFiles(closure)
	if len(files) == 0 {
		return errors.New("this host's setup qualified no clients at all — run 'coop net setup'")
	}
	pinned, err := imageFileDigests(ctx, docker, store, locked, files)
	if err != nil {
		return fmt.Errorf("coop's own client image cannot be checked: %w — run 'coop net setup' again", err)
	}
	// A pinned entry point that cannot be read is a changed client, not a missing
	// answer: deleting one, or replacing it with a symlink or a directory, lands
	// here rather than on a digest that differs.
	observed, err := imageFileDigests(ctx, docker, store, built, files)
	if err != nil {
		return fmt.Errorf("the image this project's %s built cannot be checked: %w — a filtered box runs the clients this host's setup qualified, so add your tools instead of changing them", dfRel, err)
	}
	for _, file := range files {
		if observed[file.path] != pinned[file.path] {
			return fmt.Errorf("this project's %s changes %s at %s — a filtered box runs the clients this host's setup qualified, so add your tools instead of replacing them", dfRel, file.owner, file.path)
		}
	}
	pinnedTree, err := imageTreeDigest(ctx, docker, store, locked, closure.ClientRoot)
	if err != nil {
		return fmt.Errorf("coop's locked JavaScript clients cannot be checked: %w — run 'coop net setup' again", err)
	}
	observedTree, err := imageTreeDigest(ctx, docker, store, built, closure.ClientRoot)
	if err != nil {
		return fmt.Errorf("the image this project's %s built cannot check the locked JavaScript clients: %w — add tools outside %s", dfRel, err, closure.ClientRoot)
	}
	if observedTree != pinnedTree {
		return fmt.Errorf("this project's %s changes the locked JavaScript clients below %s — add tools outside Coop's client installation", dfRel, closure.ClientRoot)
	}
	return nil
}

// maxPinnedDigestImages bounds the memo below. A host has one locked image and a
// launch has one built image; more than a handful means something is churning,
// and dropping the memo is always safe.
const maxPinnedDigestImages = 4

// pinnedDigests remembers what a proof already read out of an image ID. An image
// ID is a content address, so the same ID is the same bytes — without this the
// loop would re-read the same few hundred megabytes for every box it starts.
var pinnedDigests struct {
	sync.Mutex
	images map[string]map[string]runtime.DockerFile
}

var pinnedTreeDigests struct {
	sync.Mutex
	trees map[string]runtime.DockerTree
}

// imageFileDigests identifies each pinned file inside one image, reading them
// out of a container that is created and NEVER started. Running anything from
// the image to describe itself would let a tampered image write its own proof.
func imageFileDigests(ctx context.Context, docker filteredDocker, store *networkstate.Store, image string, files []pinnedFile) (map[string]runtime.DockerFile, error) {
	if cached := cachedFileDigests(image, files); cached != nil {
		return cached, nil
	}
	// This host may have read this exact image id before, in another process.
	// Keep it in the process memo too, so a loop reads neither the image nor the
	// store again for the boxes that follow.
	if remembered := rememberedFileDigests(store, image, files); remembered != nil {
		storeFileDigests(image, remembered)
		return remembered, nil
	}
	ref, err := createImageProofContainer(ctx, docker, image)
	if ref.ID != "" {
		// Removed on every path, including a create that failed after submitting.
		// A removal that itself fails leaves one never-started container behind and
		// is not worth failing an otherwise complete proof over: it holds no
		// resource, and its exact name says where it came from.
		defer func() { _ = docker.RemoveContainer(ctx, ref) }()
	}
	if err != nil {
		return nil, err
	}
	digests := make(map[string]runtime.DockerFile, len(files))
	for _, file := range files {
		digest, err := docker.FileDigest(ctx, ref, file.path, maxPinnedClientBytes)
		if err != nil {
			return nil, fmt.Errorf("%s at %s could not be read (%w)", file.owner, file.path, err)
		}
		digests[file.path] = digest
	}
	storeFileDigests(image, digests)
	rememberFileDigests(store, image, files, digests)
	return digests, nil
}

func createImageProofContainer(ctx context.Context, docker filteredDocker, image string) (runtime.DockerRef, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return runtime.DockerRef{}, err
	}
	ref := runtime.DockerRef{Name: "coop-image-proof-" + hex.EncodeToString(nonce),
		Labels: map[string]string{"coop.image.proof": image}}
	id, err := docker.CreateContainer(ctx, runtime.DockerCreate{Ref: ref, Image: image, Command: []string{"true"}})
	ref.ID = id
	return ref, err
}

func imageTreeDigest(ctx context.Context, docker filteredDocker, store *networkstate.Store, image, root string) (runtime.DockerTree, error) {
	key := image + "\x00" + root
	pinnedTreeDigests.Lock()
	tree, ok := pinnedTreeDigests.trees[key]
	pinnedTreeDigests.Unlock()
	if ok {
		return tree, nil
	}
	if store != nil {
		if remembered, ok := store.ImageTreeDigest(image, root); ok {
			tree = runtime.DockerTree{Size: remembered.Size, SHA256: remembered.SHA256}
			storeTreeDigest(key, tree)
			return tree, nil
		}
	}
	ref, err := createImageProofContainer(ctx, docker, image)
	if ref.ID != "" {
		defer func() { _ = docker.RemoveContainer(ctx, ref) }()
	}
	if err != nil {
		return runtime.DockerTree{}, err
	}
	tree, err = docker.TreeDigest(ctx, ref, root, maxPinnedClientTreeBytes)
	if err != nil {
		return runtime.DockerTree{}, err
	}
	storeTreeDigest(key, tree)
	if store != nil {
		_ = store.RememberImageTree(image, root, networkstate.ImageTree{Size: tree.Size, SHA256: tree.SHA256})
	}
	return tree, nil
}

func storeTreeDigest(key string, tree runtime.DockerTree) {
	pinnedTreeDigests.Lock()
	defer pinnedTreeDigests.Unlock()
	if len(pinnedTreeDigests.trees) >= maxPinnedDigestImages*2 {
		pinnedTreeDigests.trees = nil
	}
	if pinnedTreeDigests.trees == nil {
		pinnedTreeDigests.trees = make(map[string]runtime.DockerTree, maxPinnedDigestImages*2)
	}
	pinnedTreeDigests.trees[key] = tree
}

func cachedFileDigests(image string, files []pinnedFile) map[string]runtime.DockerFile {
	pinnedDigests.Lock()
	defer pinnedDigests.Unlock()
	digests, ok := pinnedDigests.images[image]
	if !ok {
		return nil
	}
	for _, file := range files {
		if _, ok := digests[file.path]; !ok {
			return nil // a different closure asks for paths this read never covered
		}
	}
	return digests
}

func storeFileDigests(image string, digests map[string]runtime.DockerFile) {
	pinnedDigests.Lock()
	defer pinnedDigests.Unlock()
	if len(pinnedDigests.images) >= maxPinnedDigestImages {
		pinnedDigests.images = nil
	}
	if pinnedDigests.images == nil {
		pinnedDigests.images = make(map[string]map[string]runtime.DockerFile, maxPinnedDigestImages)
	}
	pinnedDigests.images[image] = digests
}

// rememberedFileDigests is the DURABLE half of the memo: what this host already
// read out of this exact image id, kept in the owner-private store so the next
// process does not copy the same few hundred megabytes back out. Nothing here
// can make a changed file pass — a record is keyed by the image id and the exact
// path set, so anything missing, unreadable or foreign is a miss, and a miss
// reads the image.
func rememberedFileDigests(store *networkstate.Store, image string, files []pinnedFile) map[string]runtime.DockerFile {
	if store == nil {
		return nil
	}
	remembered := store.ImageFileDigests(image, pinnedPaths(files))
	if len(remembered) == 0 {
		return nil
	}
	digests := make(map[string]runtime.DockerFile, len(remembered))
	for path, file := range remembered {
		digests[path] = runtime.DockerFile{Mode: file.Mode, Size: file.Size, SHA256: file.SHA256}
	}
	return digests
}

// rememberFileDigests keeps what a read cost, for the next process. A memo that
// could not be written costs a re-read and nothing else: every store operation
// this launch actually depends on would fail on the same fault, and each of
// those is authority — this one is not.
func rememberFileDigests(store *networkstate.Store, image string, files []pinnedFile, digests map[string]runtime.DockerFile) {
	if store == nil {
		return
	}
	record := make(map[string]networkstate.ImageFile, len(digests))
	for path, file := range digests {
		record[path] = networkstate.ImageFile{Mode: file.Mode, Size: file.Size, SHA256: file.SHA256}
	}
	_ = store.RememberImageFiles(image, pinnedPaths(files), record)
}

func pinnedPaths(files []pinnedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	return paths
}
