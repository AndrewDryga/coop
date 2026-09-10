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
func filteredProjectImage(ctx context.Context, rt runtime.Runtime, docker filteredDocker, spec RunSpec, candidate networkstate.CandidateSpec) (string, error) {
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
		return "", errors.New("the images this host was set up with are gone — run 'coop net setup' again")
	}
	tag := filteredProjectTag(repo, candidate.ClientImage)
	var buildErrOut io.Writer
	if !spec.Quiet {
		// Unlike `coop build`, this build is not a human action — a filtered launch
		// runs it. The two proofs keep the clients and the base honest, but the
		// project's own layers are still code that runs as root at build time, so
		// say when nobody has committed the file that defines them.
		if fileUntracked(repo, dfRel) {
			ui.Info("note: %s is untracked in git — it defines this box, and an agent can author one; review it", dfRel)
		}
		// A first build takes minutes. Silence reads as a hung launch, so the
		// operator gets the same narration `coop build` gives them.
		ui.Info("building %s from %s on coop's client image", tag, dfRel)
		buildErrOut = os.Stderr
	}
	if err := buildProjectOnBase(rt, repo, dfRel, tag, definition.Tag, buildErrOut); err != nil {
		return "", fmt.Errorf("this project's %s did not build on coop's client image: %w", dfRel, err)
	}
	built, _, err := docker.Image(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("the image %s built from this project's %s cannot be read back — run it again", tag, dfRel)
	}
	if err := proveDerivedImage(ctx, docker, candidate.ClientImage, built, closure, dfRel); err != nil {
		return "", err
	}
	return built, nil
}

// proveDerivedImage refuses everything the built image cannot show it inherited.
func proveDerivedImage(ctx context.Context, docker filteredDocker, locked, built string, closure agents.ClientClosure, dfRel string) error {
	if locked == "" || built == "" || locked == built {
		// The same image means the Dockerfile added nothing at all — and an image
		// that adds nothing was not built on anything either.
		return notDerived(dfRel)
	}
	if err := proveDerivedLayers(ctx, docker, locked, built, dfRel); err != nil {
		return err
	}
	return provePinnedClients(ctx, docker, locked, built, closure, dfRel)
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
func provePinnedClients(ctx context.Context, docker filteredDocker, locked, built string, closure agents.ClientClosure, dfRel string) error {
	files := pinnedClientFiles(closure)
	if len(files) == 0 {
		return errors.New("this host's setup qualified no clients at all — run 'coop net setup'")
	}
	pinned, err := imageFileDigests(ctx, docker, locked, files)
	if err != nil {
		return fmt.Errorf("coop's own client image cannot be checked: %w — run 'coop net setup' again", err)
	}
	// A pinned entry point that cannot be read is a changed client, not a missing
	// answer: deleting one, or replacing it with a symlink or a directory, lands
	// here rather than on a digest that differs.
	observed, err := imageFileDigests(ctx, docker, built, files)
	if err != nil {
		return fmt.Errorf("the image this project's %s built cannot be checked: %w — a filtered box runs the clients this host's setup qualified, so add your tools instead of changing them", dfRel, err)
	}
	for _, file := range files {
		if observed[file.path] != pinned[file.path] {
			return fmt.Errorf("this project's %s changes %s at %s — a filtered box runs the clients this host's setup qualified, so add your tools instead of replacing them", dfRel, file.owner, file.path)
		}
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

// imageFileDigests identifies each pinned file inside one image, reading them
// out of a container that is created and NEVER started. Running anything from
// the image to describe itself would let a tampered image write its own proof.
func imageFileDigests(ctx context.Context, docker filteredDocker, image string, files []pinnedFile) (map[string]runtime.DockerFile, error) {
	if cached := cachedFileDigests(image, files); cached != nil {
		return cached, nil
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ref := runtime.DockerRef{Name: "coop-image-proof-" + hex.EncodeToString(nonce),
		Labels: map[string]string{"coop.image.proof": image}}
	// `true` overrides nothing that matters and is never run: it only keeps the
	// create honest for an image whose own entrypoint a project cleared.
	id, err := docker.CreateContainer(ctx, runtime.DockerCreate{Ref: ref, Image: image, Command: []string{"true"}})
	if id != "" {
		// Removed on every path, including a create that failed after submitting.
		// A removal that itself fails leaves one never-started container behind and
		// is not worth failing an otherwise complete proof over: it holds no
		// resource, and its exact name says where it came from.
		ref.ID = id
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
	return digests, nil
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
