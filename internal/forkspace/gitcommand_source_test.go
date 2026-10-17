package forkspace

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The exact expression is pinned, not a file or function exemption: adding a
// second call (even in the same wrapper), or changing an allowed call, must be
// reviewed. TrustedGitCommand additionally needs caller-owned metadata; ordinary
// new call sites use GitCommand, which constructs the trusted view itself.
var reviewedGitConstructors = map[string]string{
	"internal/sessionsvc/acp.go:startChildWithRunID:exec.Command(executable, \"fork\", bound.ForkName, \"acp\", bound.Target)": "Direct Coop child launch; optional test injection has no production constructor alias.",

	"tools/releasenotes/main.go:remoteTags:forkspace.TrustedGitCommand(ctx, dir, env, \"init\", \"--bare\", \"--quiet\", \"--template=\")":               "Initializes empty private metadata with isolated configuration.",
	"tools/releasenotes/main.go:remoteTags:forkspace.TrustedGitCommand(ctx, dir, env, \"ls-remote\", \"--exit-code\", \"--\", remote, ref, ref+\"^{}\")": "Explicit workflow remote and private metadata; no checkout config or transport settings.",

	"internal/cli/acp_cmd.go:spawnBox:exec.Command(self, inner...)":                                             "Reexecutes os.Executable for the editor child; argv invokes Coop.",
	"internal/cli/eval_loop.go:runLoopTrial:exec.Command(self, args...)":                                        "Reexecutes os.Executable for a Coop loop trial.",
	"internal/cli/modelscache.go:runModelCLI:exec.CommandContext(ctx, name, args...)":                           "Provider catalog spec selects the fixed codex debug models command.",
	"internal/forkctl/review.go:openInEditor:exec.Command(parts[0], parts[1:]...)":                              "Explicit editor launch selected from host settings/global config, never repository config.",
	"internal/forkctl/supervise.go:DetachForkLoop:exec.Command(self, reExec...)":                                "Reexecutes os.Executable as the detached Coop fork worker.",
	"internal/loop/keepawake.go:armKeepAwake:exec.Command(argv[0], argv[1:]...)":                                "sleepInhibitorCmd supplies only the platform caffeinate executable.",
	"internal/runtime/compose.go:RunCompose:exec.Command(docker.Binary(), argv...)":                             "Bound Docker client and explicit compose argv, not Git workspace inspection.",
	"internal/runtime/docker.go:runWorkload:exec.Command(d.binary, argv...)":                                    "Bound Docker client with validated run argv.",
	"internal/runtime/docker_lifecycle.go:StartAttached:exec.Command(d.binary, append(args, ref.ID)...)":        "Bound Docker client with fixed container start argv.",
	"internal/runtime/project_build.go:RunProjectBuild:exec.Command(docker.binary, argv...)":                    "Bound Docker client with validated build argv.",
	"internal/runtime/runtime.go:Run:exec.Command(r.Name, args...)":                                             "Configured host container runtime CLI, not Git workspace inspection.",
	"internal/runtime/runtime.go:contextCommand:exec.CommandContext(ctx, name, args...)":                        "Runtime and bound Docker callers supply the host container runtime CLI.",
	"internal/runtime/runtime.go:RunInterruptible:exec.Command(r.Name, args...)":                                "Configured host container runtime CLI, not Git workspace inspection.",
	"internal/runtime/runtime.go:StartHelper:exec.Command(r.Name, args...)":                                     "Configured host container runtime CLI starts a helper container.",
	"internal/runtime/runtime.go:Silent:exec.Command(r.Name, args...)":                                          "Configured host container runtime CLI performs existence probes.",
	"internal/tasks/lease_cmd.go:holdTaskLease:exec.CommandContext(ctx, req.command[0], req.command[1:]...)":    "Explicit user-requested lease command; arbitrary execution is the user-facing operation.",
	"tools/lifecyclebench/cases.go:coopCommand:exec.Command(b.coop, args...)":                                   "User-selected Coop benchmark binary; not a Git runner.",
	"tools/lifecyclebench/main.go:runCommand:exec.CommandContext(ctx, name, args...)":                           "Benchmark runtime/system queries; Git callers only list files or read HEAD metadata.",
	"tools/servicesbench/main.go:step:exec.CommandContext(bounded, b.coop, verb)":                               "User-selected Coop benchmark binary runs up/down.",
	"tools/servicesbench/main.go:launch:exec.Command(b.coop, \"run\", \"--\", \"sh\", \"-c\", \": > ./\"+mark)": "User-selected Coop benchmark binary launches the benchmark container.",

	"internal/box/gitenv.go:hostGitGlobal:exec.Command(\"git\", append([]string{\"config\", \"--global\"}, args...)...)":                                                                                                                                                             "Reads trusted global config only.",
	"internal/box/image.go:ignoredBuildPaths:exec.Command(\"git\", \"-C\", repo,\n\t\"-c\", \"core.fsmonitor=\", \"-c\", \"core.hooksPath=/dev/null\",\n\t\"ls-files\", \"--others\", \"--ignored\", \"--exclude-standard\", \"--directory\", \"-z\", \"--\")":                       "Lists ignored paths; no recursive submodule option or content conversion.",
	"internal/box/image.go:fileUntracked:exec.Command(\"git\", \"-C\", repo, \"rev-parse\", \"--git-dir\")":                                                                                                                                                                          "Metadata-only repository/path lookup.",
	"internal/box/image.go:fileUntracked:exec.Command(\"git\", \"-C\", repo, \"-c\", \"core.fsmonitor=\", \"-c\", \"core.hooksPath=/dev/null\",\n\t\"ls-files\", \"--error-unmatch\", \"--\", name)":                                                                                 "Metadata-only repository/path lookup.",
	"internal/cli/doctor.go:fixtureGit:exec.Command(\"git\", args...)":                                                                                                                                                                                                               "Fresh generated doctor fixture only, before box execution, with the person's git config kept out; remote URL is data.",
	"internal/cli/init_cmd.go:askGitInit:exec.Command(\"git\", \"-C\", repo, \"init\", \"-q\")":                                                                                                                                                                                      "Initializes metadata, no content inspection.",
	"internal/eval/workspace.go:runGit:exec.CommandContext(ctx, \"git\", args...)":                                                                                                                                                                                                   "Fresh eval fixture with all nested .git metadata excluded before initialization.",
	"internal/forkctl/git.go:gitGlobalOut:exec.Command(\"git\", append([]string{\"config\", \"--global\"}, args...)...)":                                                                                                                                                             "Reads trusted global config only.",
	"internal/forkspace/git.go:GitCloneContext:TrustedGitCommand(ctx, \"\", os.Environ(), \"clone\", \"--quiet\", src, dst)":                                                                                                                                                         "New destination; shared policy disables recursive clone.",
	"internal/forkspace/git.go:GitClonePinnedContext:TrustedGitCommand(ctx, \"\", env, \"clone\", \"--quiet\", \"--template=\", \"--no-local\", \"--no-checkout\", \"--\", view.dir, dst)":                                                                                           "Private source view and new destination; no checkout.",
	"internal/forkspace/git.go:GitCopyPinnedHistoryContext:TrustedGitCommand(ctx, \"\", env, args...)":                                                                                                                                                                               "Fresh destination metadata; explicit object transfer only.",
	"internal/forkspace/git.go:gitGlobalOutContext:exec.CommandContext(ctx, \"git\", append([]string{\"config\", \"--global\"}, args...)...)":                                                                                                                                        "Reads trusted global config only.",
	"internal/forkspace/gitcommand.go:TrustedGitCommand:exec.CommandContext(ctx, \"git\", full...)":                                                                                                                                                                                  "The sole guarded subprocess constructor.",
	"internal/forkspace/gitfsck.go:RunGitFsck:TrustedGitCommand(ctx, dir, view.envFrom(env), append([]string{\"fsck\", \"--no-reflogs\"}, args...)...)":                                                                                                                              "Fresh trusted reference snapshot.",
	"internal/forkspace/gitobserve.go:observationGitCommandWithCheckout:TrustedGitCommand(ctx, repository, view.envFrom(IndependentGitEnv()), args...)":                                                                                                                              "Fresh trusted metadata snapshot and copied index.",
	"internal/forkspace/gitobserve.go:ImportIsolatedObjects:TrustedGitCommand(ctx, repository, dst.envFrom(pinnedTransferEnv()),\n\t\"fetch\", \"--quiet\", \"--no-write-fetch-head\", \"--no-tags\", \"--recurse-submodules=no\", \"--\", src.dir, commit)":                         "Trusted source and destination snapshots; recursion explicitly off.",
	"internal/forkspace/gitobserve.go:CheckoutIsolatedTree:TrustedGitCommand(ctx, repository, view.envFrom(IndependentGitEnv()),\n\t\"update-index\", \"-q\", \"--refresh\", \"--ignore-submodules\")":                                                                               "Trusted metadata snapshot; refresh and checkout do not descend into children.",
	"internal/forkspace/gitobserve.go:CheckoutIsolatedTree:TrustedGitCommand(ctx, repository, view.envFrom(IndependentGitEnv()),\n\t\"read-tree\", \"--no-sparse-checkout\", \"--no-recurse-submodules\", \"-u\", \"-m\", before, publication)":                                      "Trusted metadata snapshot; refresh and checkout do not descend into children.",
	"internal/forkspace/gitview.go:GitCommandWithEnv:TrustedGitCommand(ctx, dir, withoutGitEnv(env), args...)":                                                                                                                                                                       "Constructs the trusted view, or verifies no repository is present.",
	"internal/forkspace/gitview.go:GitCommandWithEnv:TrustedGitCommand(ctx, dir, viewEnv, args...)":                                                                                                                                                                                  "Constructs the trusted view, or verifies no repository is present.",
	"internal/forkspace/gitview.go:GitRefCommand:TrustedGitCommand(ctx, dir, withoutGitEnv(os.Environ()), args...)":                                                                                                                                                                  "Runtime ref-only argument policy; never content conversion.",
	"internal/forkspace/gitview_config.go:listGitConfig:exec.CommandContext(ctx, \"git\", \"config\", \"--file\", path, \"-z\", \"--list\")":                                                                                                                                         "Reads a bounded private config copy as data.",
	"internal/forkspace/gitview_config.go:includeTrustedCheckoutConfig:exec.CommandContext(ctx, \"git\", \"config\", \"--\"+scope,\n\t\"--includes\", \"--null\", \"--type=bool-or-str\", \"--get-regexp\", `^core\\.(autocrlf|eol|safecrlf)$`)":                                     "Reads checkout config values as data only.",
	"internal/forkspace/lfs.go:startLFSReaderAt:TrustedGitCommand(ctx, repository, command.Env, \"read-tree\", commit)":                                                                                                                                                              "Read-tree uses the observation snapshot private index.",
	"internal/forkspace/lfs_storage.go:QualifyDefaultLFSStorage:exec.CommandContext(ctx, \"git\", \"--no-pager\", \"--git-dir=\"+roots[0], \"--work-tree=\"+canonical,\n\t\"config\", \"--includes\", \"--null\", \"--get\", \"lfs.storage\")":                                       "Reads lfs.storage as data; no filter execution.",
	"internal/scaffold/scaffold.go:gitRepo:exec.Command(\"git\", \"-C\", repo, \"rev-parse\", \"--git-dir\")":                                                                                                                                                                        "Metadata-only repository lookup.",
	"internal/scaffold/scaffold.go:gitConfigGet:exec.Command(\"git\", \"-C\", repo, \"config\", \"--get\", key)":                                                                                                                                                                     "Reads config values as data.",
	"internal/scaffold/scaffold.go:gitConfigSet:exec.Command(\"git\", \"-C\", repo, \"config\", \"--local\", key, value)":                                                                                                                                                            "Writes config values; no content processing.",
	"internal/sessionsvc/checkpoint_repository.go:writeCheckpointRepository:forkspace.TrustedGitCommand(ctx, workspace, env, args...)":                                                                                                                                               "Caller owns private snapshot metadata/index.",
	"internal/sessionsvc/checkpoint_restore.go:initializeCheckpointRepository:exec.CommandContext(ctx, \"git\", append(append([]string{}, forkspace.GitHardening...),\n\t\"init\", \"--quiet\", \"--template=\", \"--object-format=\"+format, repository)...)":                       "Initializes fresh private metadata only.",
	"internal/sessionsvc/companion.go:sessionPinnedStatusContext:forkspace.TrustedGitCommand(ctx, binding.Workspace, env, []string{\n\t\"status\", \"--porcelain=v2\", \"--untracked-files=all\", \"--no-renames\",\n\t\"--ignore-submodules=all\", \"-z\",\n}...)":                  "Caller owns private pinned metadata/index.",
	"internal/sessionsvc/companion.go:withSessionPrivateIndex:exec.CommandContext(ctx,\n\t\"git\", \"init\", \"--bare\", \"--quiet\", \"--object-format=\"+objectFormat,\n\t\"--template=\"+emptyTemplate, gitDir,\n)":                                                               "Initializes fresh private metadata only.",
	"internal/sessionsvc/review_candidate.go:retainReviewCandidate:exec.CommandContext(ctx, \"git\", args...)":                                                                                                                                                                       "Initializes fresh private retained-candidate metadata only.",
	"internal/sessionsvc/tracked_snapshot.go:streamSessionTrackedPatch:forkspace.TrustedGitCommand(ctx, workspace, env, []string{\n\t\"diff\", \"--no-ext-diff\", \"--no-textconv\", \"--ignore-submodules=dirty\", \"--submodule=short\", \"--binary\", base, tree, \"--\",\n}...)": "Private snapshot metadata; compares two trees.",
	"internal/sessionsvc/tracked_snapshot.go:withSessionTrackedTree:forkspace.TrustedGitCommand(ctx, workspace, env, args...)":                                                                                                                                                       "Private snapshot metadata/index; literal paths staged with update-index.",
	"internal/sessionsvc/workspace.go:runSessionPrivateGitContext:forkspace.TrustedGitCommand(ctx, workspace, env, args...)":                                                                                                                                                         "Caller owns private snapshot metadata/index.",
	"internal/workerconnector/job_sources.go:sourceGitCommand:forkspace.TrustedGitCommand(ctx, directory, nil, args...)":                                                                                                                                                             "Worker owns fresh source staging metadata; shared policy prevents recursion.",
}

func TestHostGitCommandsUseSharedBoundary(t *testing.T) {
	found := map[string]int{}
	root := filepath.Join("..", "..")
	if override := os.Getenv("COOP_TEST_GIT_BOUNDARY_ROOT"); override != "" {
		root = override
	}
	forbidden, err := forbiddenConstructorValues(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range forbidden {
		t.Error(violation)
	}
	err = walkGitSources(root, func(path string, data []byte) error {
		sites, err := gitConstructorSites(path, data)
		if err != nil {
			return err
		}
		for _, site := range sites {
			found[site]++
			if reviewedGitConstructors[site] == "" {
				t.Errorf("unreviewed Git constructor: %q", site)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for site := range reviewedGitConstructors {
		if found[site] != 1 {
			t.Errorf("reviewed Git constructor occurs %d times, want exactly once: %s", found[site], site)
		}
	}
}

func gitConstructorSites(path string, source []byte) ([]string, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, path, source, 0)
	if err != nil {
		return nil, err
	}
	aliases := map[string]string{}
	dotExec := false
	for _, imp := range file.Imports {
		value, _ := strconv.Unquote(imp.Path.Value)
		name := filepath.Base(value)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		aliases[name] = value
		if name == "." && value == "os/exec" {
			dotExec = true
		}
	}
	// Unknown executable expressions require review, regardless of their spelling.
	// Constructor values are separately forbidden before the reviewed-call check.
	constructor := func(expr ast.Expr) string {
		switch fun := expr.(type) {
		case *ast.Ident:
			if fun.Name == "TrustedGitCommand" || dotExec && (fun.Name == "Command" || fun.Name == "CommandContext") {
				return fun.Name
			}
		case *ast.SelectorExpr:
			pkg, ok := fun.X.(*ast.Ident)
			if !ok {
				return ""
			}
			if aliases[pkg.Name] == "github.com/AndrewDryga/coop/internal/forkspace" && fun.Sel.Name == "TrustedGitCommand" ||
				aliases[pkg.Name] == "os/exec" && (fun.Sel.Name == "Command" || fun.Sel.Name == "CommandContext") {
				return fun.Sel.Name
			}
		}
		return ""
	}
	var sites []string
	for _, decl := range file.Decls {
		name := "package-level"
		if function, ok := decl.(*ast.FuncDecl); ok {
			name = function.Name.Name
		}
		record := func(node ast.Node) {
			var expression bytes.Buffer
			_ = format.Node(&expression, set, node)
			sites = append(sites, path+":"+name+":"+expression.String())
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			kind := constructor(call.Fun)
			if kind == "" {
				return true
			}
			if kind != "TrustedGitCommand" {
				index := 0
				if kind == "CommandContext" {
					index = 1
				}
				if len(call.Args) > index {
					if literal, ok := call.Args[index].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						executable, err := strconv.Unquote(literal.Value)
						base := strings.ToLower(filepath.Base(executable))
						if err == nil && base != "git" && base != "git.exe" {
							return true // only a literal proves this executable is not Git
						}
					}
				}
			}
			record(call)
			return true
		})

	}
	return sites, nil
}

func TestHostGitBoundaryScannerRejectsNewCalls(t *testing.T) {
	for _, body := range []string{
		`exec.Command("git", "status")`,
		`exec.CommandContext(ctx, "git", args...)`,
		`exec.Command(gitBinary, args...)`,
		`binary := "git"; exec.Command(binary, "status")`,
		`binary := "git"; exec.CommandContext(ctx, binary, "status")`,
		`forkspace.TrustedGitCommand(ctx, repo, os.Environ(), "status")`,
	} {
		source := `package p; import "os/exec"; import "github.com/AndrewDryga/coop/internal/forkspace"; func newCall() { ` + body + ` }`
		sites, err := gitConstructorSites("new.go", []byte(source))
		if err != nil || len(sites) != 1 {
			t.Fatalf("missed %s: %v, %v", body, sites, err)
		}
		if reviewedGitConstructors[sites[0]] != "" {
			t.Fatal("new constructor was exempted")
		}
	}
}

func TestHostGitBoundaryScannerIncludesPackageInitializers(t *testing.T) {
	for _, source := range []string{
		`package p; import "os/exec"; var cmd = exec.Command("git", "status")`,
		`package p; import "github.com/AndrewDryga/coop/internal/forkspace"; var cmd = forkspace.TrustedGitCommand(ctx, repo, env, "status")`,
		`package p; import . "os/exec"; func f() { Command("git", "status") }`,
		`package p; import . "os/exec"; import . "fmt"; func f() { Command("git", "status"); Println() }`,
	} {
		sites, err := gitConstructorSites("new.go", []byte(source))
		if err != nil || len(sites) != 1 || reviewedGitConstructors[sites[0]] != "" {
			t.Fatalf("unreviewed constructor escaped: %v, %v", sites, err)
		}
	}
}

func TestHostGitBoundaryScannerRecognizesLiteralNonGit(t *testing.T) {
	source := `package p; import "os/exec"; func f() { exec.Command("docker", "ps"); exec.CommandContext(ctx, "/usr/bin/ssh", "host") }`
	sites, err := gitConstructorSites("new.go", []byte(source))
	if err != nil || len(sites) != 0 {
		t.Fatalf("literal non-Git constructors: %v, %v", sites, err)
	}
}

func TestHostGitBoundaryScannerRejectsConstructorValues(t *testing.T) {
	for _, source := range []string{
		`package p; import e "os/exec"; var run = e.Command`,
		`package p; import . "os/exec"; func f() { run := CommandContext; run(ctx, "git", "status") }`,
		`package p; import "os/exec"; func f() { use(exec.Command) }`,
		`package p; import "os/exec"; func f() { (exec.Command)("git", "status") }`,
	} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "new.go"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		violations, err := forbiddenConstructorValues(root)
		if err != nil || len(violations) == 0 {
			t.Fatalf("forbidden constructor value escaped: %v, %v", violations, err)
		}
	}
}
