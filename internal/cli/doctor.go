package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// doctorProbe runs inside the box against the fixture and reports, line by line,
// whether each secret is shadowed and each non-secret stays visible.
const doctorProbe = `#!/bin/sh
cd /workspace 2>/dev/null || { echo "RESULT FAIL workspace was not mounted"; exit 1; }
empty() { [ -f "$1" ] && [ ! -s "$1" ]; }
empty .env                   && echo "RESULT PASS .env is shadowed (empty in the VM)"     || echo "RESULT FAIL .env is READABLE in the VM"
empty .envrc                 && echo "RESULT PASS .envrc (direnv) is shadowed"            || echo "RESULT FAIL .envrc is READABLE in the VM"
empty config/prod.tfvars     && echo "RESULT PASS *.tfvars in a subdir is shadowed"       || echo "RESULT FAIL config/prod.tfvars is READABLE"
empty deploy/id_ed25519      && echo "RESULT PASS a private key in a subdir is shadowed"  || echo "RESULT FAIL deploy/id_ed25519 is READABLE"
empty config/credentials.yaml && echo "RESULT PASS .coopignore shadows a custom path"     || echo "RESULT FAIL config/credentials.yaml is READABLE"
if [ -d secrets ] && [ -z "$(ls -A secrets 2>/dev/null)" ]; then echo "RESULT PASS secrets/ is shadowed (empty)"; else echo "RESULT FAIL secrets/ exposes files"; fi
# A symlink must not read around the shadow: notes-link -> .env resolves to the emptied .env.
if [ -L notes-link ] && [ ! -s notes-link ]; then echo "RESULT PASS a symlink to a secret reads empty"; else echo "RESULT FAIL notes-link reaches the .env contents"; fi
if echo x >> .env 2>/dev/null; then echo "RESULT FAIL the .env decoy is writable"; else echo "RESULT PASS writing the .env decoy is blocked"; fi
[ -s .env.example ] && echo "RESULT PASS .env.example template stays readable" || echo "RESULT FAIL .env.example was hidden"
[ -s src/app.js ]   && echo "RESULT PASS source files stay readable"           || echo "RESULT FAIL source files were hidden"
if grep -rqs hunter2 . 2>/dev/null; then echo "RESULT FAIL secret value reachable in the tree"; else echo "RESULT PASS secret value appears nowhere the agent can read"; fi
# Privilege posture (interpreted on the host — it depends on the image and runtime).
echo "RESULT UID $(id -u)"
echo "RESULT CAPS $(awk '/^CapEff/{print $2}' /proc/self/status 2>/dev/null)"
echo "RESULT PIDS $(cat /sys/fs/cgroup/pids.max 2>/dev/null || cat /sys/fs/cgroup/pids/pids.max 2>/dev/null)"
`

type report struct{ pass, fail int }

func (r *report) ok(msg string) { r.pass++; fmt.Printf("  %s %s\n", ui.Check(), msg) }
func (r *report) no(msg string) { r.fail++; fmt.Printf("  %s %s\n", ui.Cross(), msg) }

// cmdDoctor proves isolation by attacking it: it builds a fixture repo full of
// secrets, runs the box against it, and checks that every secret is shadowed
// inside the sandbox and absent from a clone handoff.
func (a *app) cmdDoctor(args []string) (int, error) {
	if err := rejectArgs("doctor", args); err != nil {
		return 2, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	fixture, err := buildFixture()
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(fixture)

	// The probe lives outside the fixture: it must not appear in /workspace, or
	// its own "hunter2" grep pattern would trip the secret-value check.
	probeFile, err := os.CreateTemp("", "coop-probe-*.sh")
	if err != nil {
		return -1, err
	}
	probe := probeFile.Name()
	probeFile.Close()
	defer os.Remove(probe)
	if err := os.WriteFile(probe, []byte(doctorProbe), 0o644); err != nil {
		return -1, err
	}
	// CreateTemp made the file 0600 and WriteFile won't widen an existing file's mode. The box runs
	// the probe as a uid that may not own it and, under --cap-drop ALL, without CAP_DAC_OVERRIDE to
	// bypass the read check (alpine-as-root on rootful Linux Docker is exactly this) — so `sh
	// /probe.sh` would silently read nothing. Make it world-readable.
	if err := os.Chmod(probe, 0o644); err != nil {
		return -1, err
	}

	rep := &report{}
	// Prefer the real box image — it carries coop's non-root USER (node) and full toolchain, so
	// the probe tests the actual box the agent runs in, not a stand-in. Fall back to alpine when
	// it isn't built yet, so doctor still works before a first `coop build`.
	img := box.ImageForRepo(fixture, a.cfg.BaseImage, a.cfg.ImageOverride)
	usingReal := box.ImageExists(a.rt, img)
	if !usingReal {
		img = "alpine"
	}
	fmt.Printf("%s  %s\n", ui.Bold("== coop doctor =="), ui.Dim(fmt.Sprintf("(runtime: %s, image: %s)", a.rt.Name, img)))

	// The OCI privilege limits (cap-drop ALL, pids, no-new-privileges) are docker/podman-only
	// (box.boxLimits). On any other runtime they're simply not applied, so the uid/caps checks
	// below can't vouch for them — say so loudly instead of printing a falsely-clean bill.
	hardened := a.rt.Name == "docker" || a.rt.Name == "podman"
	if !hardened {
		fmt.Printf("  %s %s\n", ui.Yellow("!"), ui.Yellow(fmt.Sprintf("runtime %q applies no capability/pids limits — it relies on its own VM isolation, not coop's cap-drop", a.rt.Name)))
	}

	// --- inside the sandbox ---
	fmt.Printf("\n%s\n", ui.Bold("inside the sandbox"))
	var out, errOut bytes.Buffer
	_, runErr := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: fixture, Workdir: "/workspace", Cmd: []string{"sh", "/probe.sh"},
		Batch: true, Quiet: true, Stdout: &out, Stderr: &errOut,
		ExtraArgs: []string{"-v", probe + ":/probe.sh:ro"},
	})
	if runErr != nil || out.Len() == 0 {
		// Surface WHY: an opaque "failed to run" sent us hunting through CI logs for the cause once
		// already. Show the runtime's own complaint (its last stderr line), or the run error.
		why := strings.TrimSpace(errOut.String())
		if why == "" && runErr != nil {
			why = runErr.Error()
		}
		msg := "the sandbox produced no output (the container failed to run)"
		if why != "" {
			lines := strings.Split(why, "\n")
			msg += ": " + strings.TrimSpace(lines[len(lines)-1])
		}
		rep.no(msg)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		switch {
		case strings.HasPrefix(line, "RESULT PASS "):
			rep.ok(strings.TrimPrefix(line, "RESULT PASS "))
		case strings.HasPrefix(line, "RESULT FAIL "):
			rep.no(strings.TrimPrefix(line, "RESULT FAIL "))
		case strings.HasPrefix(line, "RESULT UID "):
			doctorCheckUID(rep, strings.TrimPrefix(line, "RESULT UID "), usingReal)
		case strings.HasPrefix(line, "RESULT CAPS "):
			doctorCheckCaps(rep, strings.TrimPrefix(line, "RESULT CAPS "), hardened)
		case strings.HasPrefix(line, "RESULT PIDS "):
			doctorCheckPids(rep, strings.TrimPrefix(line, "RESULT PIDS "), hardened, a.cfg.Pids)
		}
	}

	// --- egress fails closed ---
	// A run is asked for a network (Network:true) but with COOP_EGRESS=none; the box must still
	// come up with only loopback, proving the egress toggle cuts outbound regardless of the request.
	fmt.Printf("\n%s\n", ui.Bold("egress (fail-closed)"))
	doctorCheckEgress(rep, a, fixture, img)

	// --- on the host: the clone handoff ---
	fmt.Printf("\n%s\n", ui.Bold("on the host (the clone handoff)"))
	clone := fixture + "-clone"
	defer os.RemoveAll(clone)
	if err := exec.Command("git", "clone", "-q", fixture, clone).Run(); err != nil {
		rep.no(fmt.Sprintf("could not clone the fixture: %v", err))
	} else {
		checkAbsent(rep, filepath.Join(clone, ".env"), "gitignored .env never enters a clone", ".env leaked into the clone")
		checkAbsent(rep, filepath.Join(clone, ".envrc"), "gitignored .envrc never enters a clone", ".envrc leaked into the clone")
		checkAbsent(rep, filepath.Join(clone, "secrets"), "gitignored secrets/ never enters a clone", "secrets/ leaked into the clone")
		checkAbsent(rep, filepath.Join(clone, "deploy"), "gitignored deploy/ (private key) never enters a clone", "the deploy/ private key leaked into the clone")
		if fileExists(filepath.Join(clone, "src", "app.js")) {
			rep.ok("tracked source is present in the clone")
		} else {
			rep.no("tracked source missing")
		}
		if treeContains(clone, "hunter2") {
			rep.no("secret value leaked into the clone")
		} else {
			rep.ok("no secret value anywhere in the clone")
		}
		if origin, _ := exec.Command("git", "-C", clone, "remote", "get-url", "origin").Output(); strings.HasPrefix(strings.TrimSpace(string(origin)), "/") {
			rep.ok("clone origin is a local path — there is nowhere to push")
		} else {
			rep.no("clone origin is not a local path")
		}
	}

	fmt.Println()
	if rep.fail == 0 {
		fmt.Printf("%s — the box contains the agent.\n", ui.Bold(ui.Green(fmt.Sprintf("✓ all %d checks passed", rep.pass))))
		return 0, nil
	}
	fmt.Printf("%s\n", ui.Bold(ui.Red(fmt.Sprintf("✗ %d passed, %d failed", rep.pass, rep.fail))))
	return 1, nil
}

// doctorCheckUID interprets the box's uid. Only the real box image carries coop's non-root USER
// (node); the alpine fallback is root by default, so there a root uid is expected, not a finding.
func doctorCheckUID(rep *report, uid string, usingReal bool) {
	uid = strings.TrimSpace(uid)
	switch {
	case !usingReal:
		fmt.Printf("  %s the box uid is %s (alpine fallback runs as root; build the box image to check its USER)\n", ui.Dim("·"), uid)
	case uid == "0":
		rep.no("the box runs as ROOT (uid 0) — give Dockerfile.agent a non-root USER")
	default:
		rep.ok(fmt.Sprintf("the box runs as non-root (uid %s)", uid))
	}
}

// doctorCheckCaps interprets the box's effective capabilities. --cap-drop ALL is applied only on
// docker/podman (hardened); on any other runtime the warning up top already flagged it, so a
// non-zero set there is a note, not a double-counted failure.
func doctorCheckCaps(rep *report, caps string, hardened bool) {
	caps = strings.TrimSpace(caps)
	switch {
	case !hardened:
		fmt.Printf("  %s the box CapEff is %s (no cap-drop on this runtime)\n", ui.Dim("·"), caps)
	case caps == "":
		rep.no("could not read the box's CapEff from /proc")
	case strings.Trim(caps, "0") == "":
		rep.ok("all Linux capabilities dropped (CapEff=0)")
	default:
		rep.no(fmt.Sprintf("capabilities NOT dropped (CapEff=%s) — --cap-drop ALL didn't take effect", caps))
	}
}

// doctorCheckPids interprets the box's pids cgroup limit. Like caps it's docker/podman-only; an
// unreadable value (an unusual cgroup layout) is a note rather than a failure, but a live "max"
// when config asked for a cap is a real finding (the --pids-limit didn't take).
func doctorCheckPids(rep *report, pids string, hardened bool, configured string) {
	pids = strings.TrimSpace(pids)
	switch {
	case !hardened:
		fmt.Printf("  %s pids-limit is not applied on this runtime\n", ui.Dim("·"))
	case configured == "" || configured == "0" || configured == "-1" || configured == "unlimited":
		fmt.Printf("  %s pids-limit disabled by config (COOP_PIDS=%q)\n", ui.Dim("·"), configured)
	case pids == "":
		fmt.Printf("  %s could not read the box's pids-limit (unusual cgroup layout)\n", ui.Dim("·"))
	case pids == "max":
		rep.no("pids-limit not enforced (cgroup pids.max=max) — the --pids-limit didn't take effect")
	default:
		rep.ok(fmt.Sprintf("pids-limit enforced (%s)", pids))
	}
}

// doctorCheckEgress proves COOP_EGRESS=none cuts the box off the network even when a run asks for
// one. It runs a box with Egress forced to none and Network requested, and checks only loopback
// came up — reliable offline, since --network none leaves just `lo` with no host connectivity.
func doctorCheckEgress(rep *report, a *app, fixture, img string) {
	offlineCfg := *a.cfg
	offlineCfg.Egress = "none"
	var out, errOut bytes.Buffer
	_, err := box.Run(&offlineCfg, a.rt, box.RunSpec{
		Image: img, Repo: fixture, Workdir: "/workspace", Network: true,
		Cmd:   []string{"sh", "-c", "ls /sys/class/net 2>/dev/null | tr '\\n' ' '"},
		Batch: true, Quiet: true, Stdout: &out, Stderr: &errOut,
	})
	ifaces := strings.Fields(out.String())
	var external []string
	for _, n := range ifaces {
		if n != "lo" {
			external = append(external, n)
		}
	}
	switch {
	case len(external) > 0:
		rep.no(fmt.Sprintf("COOP_EGRESS=none still left a network interface (%s) — egress is not fully closed", strings.Join(external, " ")))
	case len(ifaces) == 1 && ifaces[0] == "lo":
		rep.ok("COOP_EGRESS=none cuts the box off the network (loopback only)")
	default:
		why := strings.TrimSpace(errOut.String())
		if why == "" && err != nil {
			why = err.Error()
		}
		msg := "could not verify the offline box's network"
		if why != "" {
			msg += ": " + strings.TrimSpace(why[strings.LastIndex(why, "\n")+1:])
		}
		rep.no(msg)
	}
}

// buildFixture creates a throwaway git repo seeded with secrets and decoys.
func buildFixture() (string, error) {
	dir, err := os.MkdirTemp("", "coop-doctor-")
	if err != nil {
		return "", err
	}
	// MkdirTemp makes the root 0700; the box mounts it at /workspace and the probe must cd into and
	// stat it as a uid that may not own it (and, under --cap-drop ALL, can't bypass the check). Make
	// the root world-traversable — the seeded files are already 0644 / subdirs 0755.
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", err
	}
	files := map[string]string{
		".env":         "SECRET=hunter2\n",
		".env.example": "KEY=put-your-key-here\n",
		// direnv config (a common AWS_SECRET_ACCESS_KEY home) and a private key in a subdir —
		// both must be shadowed by name regardless of depth.
		".envrc":             "export AWS_SECRET_ACCESS_KEY=hunter2\n",
		"deploy/id_ed25519":  "-----BEGIN OPENSSH PRIVATE KEY-----\nhunter2\n",
		"config/prod.tfvars": "x = \"hunter2\"\n",
		// A repo-specific secret the default denylist can't know about, hidden via
		// .coopignore — proves the user-extensible path, not just the built-ins.
		".coopignore":             "config/credentials.yaml\n",
		"config/credentials.yaml": "token: hunter2\n",
		"secrets/api-token":       "tok-hunter2\n",
		"src/app.js":              "console.log(1)\n",
		".gitignore":              ".env\n.envrc\n*.tfvars\nsecrets/\ndeploy/\nconfig/credentials.yaml\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	// A symlink to a secret: shadowing must cover what it points at, so following it reads empty.
	if err := os.Symlink(".env", filepath.Join(dir, "notes-link")); err != nil {
		return "", err
	}
	cmds := [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=d@d", "-c", "user.name=d", "commit", "-qm", "init"},
	}
	for _, c := range cmds {
		cmd := exec.Command("git", append([]string{"-C", dir}, c...)...)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %v: %w", c, err)
		}
	}
	return dir, nil
}

func checkAbsent(r *report, path, okMsg, noMsg string) {
	if !pathExists(path) {
		r.ok(okMsg)
	} else {
		r.no(noMsg)
	}
}

func treeContains(root, needle string) bool {
	found := false
	n := []byte(needle)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, n) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
