package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Coop tags each image it builds by the definition it was built from, so an upgrade that changes
// that definition leaves the previous one TAGGED — `docker image prune` reclaims only dangling
// images, so those ~3 GB stay forever. Removing every other definition's image is not the answer
// either: a second installed Coop (another checkout, an older deployment) runs its own, and taking
// it would restart the rebuild ping-pong the per-definition tag was introduced to end.
//
// So the rule is one boring sentence: after a successful build, remove the images of that same
// family which nothing has used for reclaimAfter and no container references. A Coop still in use
// touches its own image's record on every launch (markImageUsed), which is what keeps it.
const (
	reclaimAfter    = 14 * 24 * time.Hour
	reclaimTimeout  = 2 * time.Minute
	imageUseDirName = "image-use"
)

// reclaimFamilies are the image repositories Coop builds and may therefore remove. An operator's
// own COOP_BASE_IMAGE is never one of them, whatever it is tagged.
var reclaimFamilies = []string{ManagedBaseRepository, lockedClientRepository, gatewayimage.Repository}

// derivedImageLabel marks an image Coop built from a project's own box Dockerfile, with the project
// it was built for as the value. Those images are tagged per definition like the shared families
// (`<project>-filtered:<16 hex>`), so they accumulate the same way — but their repository is the
// PROJECT's name, which says nothing about who built it. The label is what makes one Coop's to
// remove: a `my-app-filtered` image an operator built by hand carries none and is never a candidate.
const derivedImageLabel = "coop.derived"

// derivedImageTagSuffix is what `filteredProjectTag` appends to a project's name.
const derivedImageTagSuffix = "-filtered"

// derivedImageProject names the project a derived image was built for, or "" when img is not one.
func derivedImageProject(img string) string {
	repository, tag, ok := strings.Cut(img, ":")
	if !ok || len(tag) != 16 || strings.Trim(tag, "0123456789abcdef") != "" {
		return ""
	}
	if strings.Contains(repository, "/") {
		return "" // a registry path is somebody else's; Coop's own names never have one
	}
	project, ok := strings.CutSuffix(repository, derivedImageTagSuffix)
	if !ok || project == "" {
		return ""
	}
	return project
}

func imageUsePath(cfg *config.Config, img string) string {
	return filepath.Join(cfg.BoxHome, imageUseDirName, safeImageFileName(img))
}

// markImageUsed records that this launch used img, so a later build keeps it. Only an image Coop
// itself builds is recorded: an operator's own image is not Coop's to reclaim, so its use is not
// Coop's to track. Best-effort — a run must never fail over bookkeeping.
func markImageUsed(cfg *config.Config, img string) {
	if cfg == nil || cfg.BoxHome == "" || !reclaimable(img) {
		return
	}
	path := imageUsePath(cfg, img)
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	_ = os.WriteFile(path, []byte(img+"\n"), 0o644)
}

// reclaimable reports whether img is one Coop built and tagged by its definition — the only kind
// this file ever removes. A `:latest` or any other tag of those repositories is left alone: Coop
// no longer creates one, and an operator may be pinning it.
func reclaimable(img string) bool {
	if derivedImageProject(img) != "" {
		// Shape only. Whether this one is COOP's is a question for the runtime (the label), asked
		// before anything is removed; recording the use of an image that turns out to be an
		// operator's costs nothing.
		return true
	}
	repository, tag, ok := strings.Cut(img, ":")
	if !ok || len(tag) != 32 || strings.Trim(tag, "0123456789abcdef") != "" {
		return false
	}
	for _, family := range reclaimFamilies {
		if repository == family {
			return true
		}
	}
	return false
}

// reclaimCandidates lists the images this rule may weigh against keep: every tag of keep's shared
// family, or — for a project's derived image — only the ones carrying Coop's own label, since that
// repository is the project's name and anyone may have built into it.
func reclaimCandidates(ctx context.Context, rt runtime.Runtime, keep string) ([]string, error) {
	family, _, _ := strings.Cut(keep, ":")
	if project := derivedImageProject(keep); project != "" {
		return rt.ImageTagsLabeled(ctx, family, derivedImageLabel+"="+project)
	}
	return rt.ImageTags(ctx, family)
}

// reclaimSupersededImages removes the images of keep's family that nothing has used for
// reclaimAfter and no container references, and returns what it removed, in tag order. keep itself
// always stays, as does any image whose last use this Coop — or another one on this host — recorded
// recently. A runtime that cannot be asked what exists or what is running removes nothing: not
// knowing is never permission to delete.
func reclaimSupersededImages(ctx context.Context, rt runtime.Runtime, cfg *config.Config, keep string) ([]string, error) {
	if cfg == nil || cfg.BoxHome == "" || !reclaimable(keep) {
		return nil, nil
	}
	markImageUsed(cfg, keep)
	tags, err := reclaimCandidates(ctx, rt, keep)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cutoff := now.Add(-reclaimAfter)
	var removed []string
	var failures []error
	family, _, _ := strings.Cut(keep, ":")
	for _, tag := range tags {
		// A candidate is one of KEEP's own family. The queries above already narrow to it; this is
		// the same rule stated where the removal happens, so a looser query can never widen it.
		if tag == keep || !strings.HasPrefix(tag, family+":") || !reclaimable(tag) {
			continue
		}
		info, err := os.Stat(imageUsePath(cfg, tag))
		switch {
		case err != nil:
			// Never recorded — by a Coop that predates this record, by an operator's own
			// `docker build --build-arg COOP_BASE_IMAGE=…`, or simply by never having run yet.
			// An absent record is not evidence of disuse, so it is SEEDED here and judged by the
			// next build: this rule may only remove what it watched go unused.
			markImageUsed(cfg, tag)
			continue
		case info.ModTime().After(now):
			// A record dated in the future (a clock that jumped, a restored backup) would otherwise
			// read as fresh until that date arrives. Re-seed it to now and judge it from here.
			markImageUsed(cfg, tag)
			continue
		case info.ModTime().After(cutoff):
			continue // in use by a Coop on this host, however recently installed
		}
		used, err := rt.ImageInUse(ctx, tag)
		if err != nil {
			failures = append(failures, err)
			continue // unknown reads as in use
		}
		if used {
			continue
		}
		if err := rt.RemoveImage(ctx, tag); err != nil {
			// One image that cannot go — a child image still derived from it, a race with another
			// build — must not stop the rest of the scan.
			failures = append(failures, err)
			continue
		}
		_ = os.Remove(imageUsePath(cfg, tag))
		removed = append(removed, tag)
	}
	sort.Strings(removed)
	return removed, errors.Join(failures...)
}

// markLaunchImages records every image Coop built that this launch depends on: the box image it
// runs, the managed base a project image was built on, and — for a filtered run — the qualified
// client and gateway images that run was set up with. Recording the box image alone would miss
// exactly the ones a filtered host cannot rebuild cheaply.
func markLaunchImages(cfg *config.Config, images ...string) {
	for _, image := range images {
		markImageUsed(cfg, image)
	}
}

// clientImageTag and gatewayImageTag name the images a qualified filtered host was set up with,
// from the definition hashes its candidate recorded.
func clientImageTag(definition string) string { return familyTag(lockedClientRepository, definition) }

// gatewayImageTag is clientImageTag for the network helper.
func gatewayImageTag(source string) string { return familyTag(gatewayimage.Repository, source) }

func familyTag(family, definition string) string {
	if len(definition) < 32 {
		return ""
	}
	return family + ":" + definition[:32]
}

// reclaimAfterBuild is the reclaim a successful build runs, with its own bounded deadline. It
// speaks only when it actually reclaimed something: the image just built is good either way, a
// runtime that could not answer has simply left the disk as it was, and the next build asks again.
// A build is not the place to report Coop's own housekeeping.
func reclaimAfterBuild(ctx context.Context, rt runtime.Runtime, cfg *config.Config, keep string, out io.Writer, say func(string)) {
	ctx, cancel := context.WithTimeout(ctx, reclaimTimeout)
	defer cancel()
	removed, _ := reclaimSupersededImages(ctx, rt, cfg, keep)
	if len(removed) == 0 {
		return
	}
	line := fmt.Sprintf("Reclaimed %s no run has used in %d days: %s",
		ui.Count(len(removed), "superseded image"), int(reclaimAfter.Hours()/24), strings.Join(removed, ", "))
	switch {
	case say != nil:
		say(line)
	case out != nil:
		fmt.Fprintln(out, line)
	}
}
