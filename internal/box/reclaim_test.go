package box

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// reclaimRuntime is a Docker whose image list, ancestor query and removals the test controls: the
// shim records every call, so what the rule did is what it says it did.
func reclaimRuntime(t *testing.T, tags []string, used map[string]bool, stuck ...string) (runtime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	shim := filepath.Join(dir, "docker")
	var script strings.Builder
	script.WriteString("#!/bin/sh\necho \"$@\" >> " + strconv.Quote(calls) + "\n")
	script.WriteString("case \"$1 $2\" in\n\"image ls\")\n")
	for _, tag := range tags {
		script.WriteString("  echo " + strconv.Quote(tag) + "\n")
	}
	script.WriteString("  exit 0 ;;\nesac\n")
	script.WriteString("case \"$1\" in\nps)\n")
	for image, inUse := range used {
		if inUse {
			script.WriteString("  case \"$*\" in *" + image + "*) echo deadbeef; exit 0 ;; esac\n")
		}
	}
	script.WriteString("  exit 0 ;;\nesac\n")
	script.WriteString("case \"$1 $2\" in\n\"image rm\")\n")
	for _, image := range stuck {
		script.WriteString("  case \"$3\" in " + image + ") echo 'image is being used by another image'; exit 1 ;; esac\n")
	}
	script.WriteString("  exit 0 ;;\nesac\nexit 0\n")
	writeRepoFile(t, shim, script.String())
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}, calls
}

// Across upgrades the images Coop built stay bounded, while a second Coop version still in use
// keeps its own: its launches record that use, and a recorded use is what spares an image.
func TestReclaimKeepsWhatAnotherCoopStillUses(t *testing.T) {
	home := t.TempDir()
	cfg := &config.Config{BoxHome: home}
	current := ManagedBaseRepository + ":" + strings.Repeat("a", 32)
	otherVersion := ManagedBaseRepository + ":" + strings.Repeat("b", 32)
	abandoned := ManagedBaseRepository + ":" + strings.Repeat("c", 32)
	running := ManagedBaseRepository + ":" + strings.Repeat("d", 32)
	// The other version launched an hour ago; the abandoned definition has not run in a month.
	markImageUsed(cfg, otherVersion)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	writeRepoFile(t, imageUsePath(cfg, abandoned), abandoned+"\n")
	if err := os.Chtimes(imageUsePath(cfg, abandoned), stale, stale); err != nil {
		t.Fatal(err)
	}
	rt, calls := reclaimRuntime(t, []string{current, otherVersion, abandoned, running,
		ManagedBaseRepository + ":latest", ManagedBaseRepository + ":<none>"}, map[string]bool{running: true})
	removed, err := reclaimSupersededImages(context.Background(), rt, cfg, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != abandoned {
		t.Fatalf("reclaimed %v, want only the abandoned definition", removed)
	}
	recorded := string(mustReadFile(t, calls))
	// The two questions it must have asked: what exists in THIS family, and whether any container —
	// running or stopped, since a stopped box still holds its image — references the one it removed.
	if !strings.Contains(recorded, "image ls --format {{.Repository}}:{{.Tag}} "+ManagedBaseRepository) {
		t.Errorf("the scan did not ask what exists in the family:\n%s", recorded)
	}
	if !strings.Contains(recorded, "ps -q -a --filter ancestor="+abandoned) {
		t.Errorf("the scan did not ask whether any container still references it:\n%s", recorded)
	}
	for _, kept := range []string{otherVersion, running, ManagedBaseRepository + ":latest"} {
		if strings.Contains(recorded, "image rm "+kept) {
			t.Errorf("the reclaim removed %s:\n%s", kept, recorded)
		}
	}
	// The one it removed is no longer recorded as ever used, and the current image now is.
	if _, err := os.Stat(imageUsePath(cfg, abandoned)); !os.IsNotExist(err) {
		t.Errorf("the removed image kept its use record: %v", err)
	}
	if _, err := os.Stat(imageUsePath(cfg, current)); err != nil {
		t.Errorf("the image just built was not recorded as used: %v", err)
	}
	// Repeated upgrades stay bounded: each new definition reclaims the last, so the family does not
	// grow — while the other version's image survives every one of them.
	// Definition hashes are hex, so the upgrades below are too.
	for i, letter := range []string{"e", "f", "0"} {
		next := ManagedBaseRepository + ":" + strings.Repeat(letter, 32)
		older := imageUsePath(cfg, current)
		if err := os.Chtimes(older, stale, stale); err != nil {
			t.Fatal(err)
		}
		rt, _ := reclaimRuntime(t, []string{next, current, otherVersion}, nil)
		removed, err := reclaimSupersededImages(context.Background(), rt, cfg, next)
		if err != nil || len(removed) != 1 || removed[0] != current {
			t.Fatalf("upgrade %d reclaimed %v, %v", i, removed, err)
		}
		current = next
	}
	if _, err := os.Stat(imageUsePath(cfg, otherVersion)); err != nil {
		t.Fatalf("the other version's image lost its use record after three upgrades: %v", err)
	}
}

// What the rule must never touch: an operator's own image, another repository, an unreadable
// runtime, and an image a container still references.
func TestReclaimNeverRemovesWhatIsNotCoopsToRemove(t *testing.T) {
	cfg := &config.Config{BoxHome: t.TempDir()}
	current := ManagedBaseRepository + ":" + strings.Repeat("a", 32)
	for name, image := range map[string]string{
		"an operator's own image": "my-company/box:latest",
		"a floating tag":          ManagedBaseRepository + ":latest",
		"another repository":      "postgres:16",
	} {
		if reclaimable(image) {
			t.Errorf("%s reads as Coop's to remove", name)
		}
		removed, err := reclaimSupersededImages(context.Background(), runtime.Runtime{Name: "false"}, cfg, image)
		if removed != nil || err != nil {
			t.Errorf("%s: reclaim ran anyway: %v, %v", name, removed, err)
		}
	}
	// A runtime that cannot answer removes nothing and says so — not knowing is not permission.
	broken, _ := reclaimRuntime(t, nil, nil)
	broken.Name = filepath.Join(t.TempDir(), "missing-docker")
	if removed, err := reclaimSupersededImages(context.Background(), broken, cfg, current); err == nil || removed != nil {
		t.Fatalf("a broken runtime reclaimed %v, %v", removed, err)
	}
	// Every family Coop tags by definition is covered, and nothing else is.
	for _, family := range reclaimFamilies {
		if !reclaimable(family + ":" + strings.Repeat("0", 32)) {
			t.Errorf("%s is not reclaimable", family)
		}
	}
}

// An image whose use was never recorded — built by a Coop older than this record, or simply never
// run — is not evidence of disuse. The scan SEEDS it and judges it at the next build.
func TestReclaimSeedsAnImageItHasNeverSeenUsed(t *testing.T) {
	cfg := &config.Config{BoxHome: t.TempDir()}
	current := ManagedBaseRepository + ":" + strings.Repeat("a", 32)
	unrecorded := ManagedBaseRepository + ":" + strings.Repeat("b", 32)
	rt, calls := reclaimRuntime(t, []string{current, unrecorded}, nil)
	removed, err := reclaimSupersededImages(context.Background(), rt, cfg, current)
	if err != nil || len(removed) != 0 {
		t.Fatalf("an unrecorded image was reclaimed on sight: %v, %v", removed, err)
	}
	if strings.Contains(string(mustReadFile(t, calls)), "image rm") {
		t.Errorf("the scan removed something:\n%s", mustReadFile(t, calls))
	}
	// Seeded now, so the clock starts here — and once that record ages out, it goes.
	info, err := os.Stat(imageUsePath(cfg, unrecorded))
	if err != nil {
		t.Fatalf("the unrecorded image was not seeded: %v", err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Errorf("the seed is dated %v, not now", info.ModTime())
	}
	stale := time.Now().Add(-reclaimAfter - time.Hour)
	if err := os.Chtimes(imageUsePath(cfg, unrecorded), stale, stale); err != nil {
		t.Fatal(err)
	}
	rt, _ = reclaimRuntime(t, []string{current, unrecorded}, nil)
	if removed, err := reclaimSupersededImages(context.Background(), rt, cfg, current); err != nil ||
		len(removed) != 1 || removed[0] != unrecorded {
		t.Fatalf("the seeded image was not reclaimed once it aged out: %v, %v", removed, err)
	}
	// A record from the future (a clock that jumped back) is re-seeded, not read as ancient.
	future := time.Now().Add(48 * time.Hour)
	third := ManagedBaseRepository + ":" + strings.Repeat("c", 32)
	markImageUsed(cfg, third)
	if err := os.Chtimes(imageUsePath(cfg, third), future, future); err != nil {
		t.Fatal(err)
	}
	rt, _ = reclaimRuntime(t, []string{current, third}, nil)
	if removed, err := reclaimSupersededImages(context.Background(), rt, cfg, current); err != nil || len(removed) != 0 {
		t.Fatalf("a record from the future was reclaimed: %v, %v", removed, err)
	}
	info, err = os.Stat(imageUsePath(cfg, third))
	if err != nil || info.ModTime().After(time.Now().Add(time.Minute)) {
		t.Errorf("the future record was not re-seeded to now: %v, %v", info.ModTime(), err)
	}
}

// One image that will not go — a child image still derived from it, a race with another build —
// must not stop the scan from reclaiming the rest, nor be reported as reclaimed.
func TestReclaimKeepsGoingWhenOneImageCannotGo(t *testing.T) {
	cfg := &config.Config{BoxHome: t.TempDir()}
	current := ManagedBaseRepository + ":" + strings.Repeat("a", 32)
	stuck := ManagedBaseRepository + ":" + strings.Repeat("b", 32)
	free := ManagedBaseRepository + ":" + strings.Repeat("c", 32)
	stale := time.Now().Add(-reclaimAfter - time.Hour)
	for _, image := range []string{stuck, free} {
		markImageUsed(cfg, image)
		if err := os.Chtimes(imageUsePath(cfg, image), stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	rt, calls := reclaimRuntime(t, []string{current, stuck, free}, nil, stuck)
	removed, err := reclaimSupersededImages(context.Background(), rt, cfg, current)
	if len(removed) != 1 || removed[0] != free {
		t.Fatalf("reclaimed %v, want the one image that could go", removed)
	}
	if err == nil {
		t.Error("the failure to remove an image was not reported to the caller")
	}
	if !strings.Contains(string(mustReadFile(t, calls)), "image rm "+free) {
		t.Errorf("the scan stopped at the stuck image:\n%s", mustReadFile(t, calls))
	}
	if _, err := os.Stat(imageUsePath(cfg, stuck)); err != nil {
		t.Errorf("an image that stayed lost its use record: %v", err)
	}
	// A build says nothing about what it could not do: only what it reclaimed reaches the human.
	var out strings.Builder
	rt, _ = reclaimRuntime(t, []string{current, stuck}, nil, stuck)
	reclaimAfterBuild(context.Background(), rt, cfg, current, &out, nil)
	if out.String() != "" {
		t.Errorf("the build reported housekeeping: %q", out.String())
	}
}

// The tags a filtered launch records are the ones its images were actually built with.
func TestBuiltImageTagsNameTheImagesCoopBuilt(t *testing.T) {
	spec, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	definition := spec.Labels["coop.clients.definition"]
	if got := clientImageTag(definition); got != spec.Tag {
		t.Errorf("a launch would record %q, but the build tags %q", got, spec.Tag)
	}
	if got, want := gatewayImageTag(gatewayimage.Fingerprint()), gatewayimage.Tag(); got != want {
		t.Errorf("gateway tag %q, want %q", got, want)
	}
	for _, tag := range []string{clientImageTag(definition), gatewayImageTag(gatewayimage.Fingerprint())} {
		if !reclaimable(tag) {
			t.Errorf("%s is not one the reclaim would ever weigh", tag)
		}
	}
	// A hash too short to tag names nothing, and nothing is what gets recorded.
	var none *filteredExecution
	if clientImageTag("abc") != "" || gatewayImageTag("") != "" || none.builtImageTags() != nil {
		t.Error("a launch without built images named one anyway")
	}
	// A launch records every image Coop built that it rests on — the box image it runs is by ID on a
	// filtered run, so the tags are what must reach the record — and nothing that is not Coop's.
	cfg := &config.Config{BoxHome: t.TempDir()}
	base := ManagedBaseRepository + ":" + strings.Repeat("a", 32)
	client := clientImageTag(definition)
	markLaunchImages(cfg, "sha256:"+strings.Repeat("d", 64), base, client, "my-company/box:latest", "")
	for _, recorded := range []string{base, client} {
		if _, err := os.Stat(imageUsePath(cfg, recorded)); err != nil {
			t.Errorf("the launch did not record %s: %v", recorded, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cfg.BoxHome, imageUseDirName))
	if err != nil || len(entries) != 2 {
		t.Errorf("the launch recorded %d images, want the two Coop built: %v", len(entries), err)
	}
}

// Every path that starts a box records the images it rests on, or the reclaim above eventually
// removes an image that is in daily use. The first version of this rule recorded only the ordinary
// launch, so every read-only and bare session — the whole session daemon — went unrecorded. Parse
// rather than grep: a launch is a function that composes the runtime's box options.
func TestEveryLaunchRecordsTheImagesItRestsOn(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	launches := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			var composes, records bool
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch name.Name {
				case "assembleOptions", "assembleArgs":
					composes = true
				case "markLaunchImages", "markImageUsed":
					records = true
				}
				return true
			})
			if composes {
				launches[function.Name.Name] = records
			}
		}
	}
	// assembleArgs only forwards to assembleOptions; its caller is the launch.
	delete(launches, "assembleArgs")
	if len(launches) < 2 {
		t.Fatalf("found %d launch paths, expected the ordinary and restricted ones: %v", len(launches), launches)
	}
	for name, records := range launches {
		if !records {
			t.Errorf("%s starts a box without recording the images it rests on — a later build will reclaim them", name)
		}
	}
}

// A project's own box image is tagged per definition like the shared families, so a build that
// supersedes one reclaims it — but only what COOP built for THAT project. Another project's
// images, and an image someone built into the same name by hand, are not Coop's to remove.
func TestReclaimTakesOnlyThisProjectsOwnDerivedImages(t *testing.T) {
	cfg := &config.Config{BoxHome: t.TempDir()}
	current := "coop-app-filtered:" + strings.Repeat("a", 16)
	superseded := "coop-app-filtered:" + strings.Repeat("b", 16)
	other := "coop-web-filtered:" + strings.Repeat("c", 16)
	stale := time.Now().Add(-reclaimAfter - time.Hour)
	for _, image := range []string{superseded, other} {
		markImageUsed(cfg, image)
		if err := os.Chtimes(imageUsePath(cfg, image), stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	// The shim answers the LABEL query only — an unlabelled lookalike never reaches the scan.
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	shim := filepath.Join(dir, "docker")
	writeRepoFile(t, shim, "#!/bin/sh\necho \"$@\" >> "+strconv.Quote(calls)+"\n"+
		"case \"$*\" in\n"+
		// A runtime that answered too widely — another project's image among the candidates — must
		// still lose it here: the scan removes only keep's own family.
		"  *\"label=coop.derived=coop-app\"*) echo "+strconv.Quote(current)+"; echo "+strconv.Quote(superseded)+
		"; echo "+strconv.Quote(other)+"; exit 0 ;;\n"+
		"  *\"image ls\"*) echo UNLABELLED-QUERY; exit 0 ;;\n"+
		"esac\nexit 0\n")
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: shim}
	removed, err := reclaimSupersededImages(context.Background(), rt, cfg, current)
	if err != nil || len(removed) != 1 || removed[0] != superseded {
		t.Fatalf("reclaimed %v, %v; want only this project's superseded image", removed, err)
	}
	recorded := string(mustReadFile(t, calls))
	if !strings.Contains(recorded, "--filter label=coop.derived=coop-app") {
		t.Errorf("the scan did not ask for Coop's own images:\n%s", recorded)
	}
	if strings.Contains(recorded, "image rm "+other) {
		t.Errorf("another project's image was removed:\n%s", recorded)
	}
	if _, err := os.Stat(imageUsePath(cfg, other)); err != nil {
		t.Errorf("another project's use record was taken with it: %v", err)
	}
	// The shape alone is never enough, and an ordinary image is not this family at all.
	for name, image := range map[string]string{
		"an operator's lookalike": "my-app-filtered:" + strings.Repeat("d", 16),
		"this project's own":      current,
	} {
		if project := derivedImageProject(image); project == "" {
			t.Errorf("%s reads as no project's image", name)
		}
	}
	for name, image := range map[string]string{
		"a plain image":       "postgres:16",
		"a floating tag":      "coop-app-filtered:latest",
		"the wrong tag width": "coop-app-filtered:" + strings.Repeat("a", 32),
		"no project at all":   "-filtered:" + strings.Repeat("a", 16),
		"a registry path":     "ghcr.io/me/app-filtered:" + strings.Repeat("a", 16),
		"the suffix inside":   "my-filtered-app:" + strings.Repeat("a", 16),
	} {
		if project := derivedImageProject(image); project != "" {
			t.Errorf("%s reads as project %q's image", name, project)
		}
	}
}

// The tag a derived build writes and the label it carries are the ones the reclaim looks for.
func TestDerivedImagesCarryTheLabelTheReclaimAsksFor(t *testing.T) {
	tag := filteredProjectTag("/src/app", "sha256:"+strings.Repeat("e", 64))
	project := derivedImageProject(tag)
	if project == "" {
		t.Fatalf("a derived build writes %q, which the reclaim does not recognize", tag)
	}
	// The build itself must apply it — not just the argument builder when asked.
	scratch := t.TempDir()
	calls := filepath.Join(scratch, "calls")
	script := filepath.Join(scratch, "docker")
	writeRepoFile(t, script, "#!/bin/sh\necho \"$@\" >> "+strconv.Quote(calls)+"\n"+
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then echo sha256:"+strings.Repeat("a", 64)+" > \"$2\"; fi\n  shift\ndone\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	writeRepoFile(t, filepath.Join(repo, ".agent", "Dockerfile"), "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n")
	entries, err := buildContextSelection(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := buildProjectOnBase(context.Background(), runtime.Runtime{Name: script}, repo, entries,
		".agent/Dockerfile", tag, "coop-clients:"+strings.Repeat("f", 32), nil); err != nil {
		t.Fatal(err)
	}
	if built := string(mustReadFile(t, calls)); !strings.Contains(built, "--label coop.derived="+project+" -t "+tag) {
		t.Errorf("the build does not mark the image as Coop's: %q", built)
	}
	// A filtered launch runs this image by ID, so the tag is what it records — and a recorded use
	// is the only thing that spares an image from the next build.
	if !reclaimable(tag) {
		t.Errorf("a launch would not record %q as used, so the next build would take it", tag)
	}
	// An ordinary project build takes no label, so `coop build` is byte-identical to before.
	if plain := strings.Join(projectBuildArgs("/ctx", ".agent/Dockerfile", "coop-app", "coop-box:x", true, false), " "); strings.Contains(plain, "--label") {
		t.Errorf("an ordinary project build gained a label: %q", plain)
	}
}
