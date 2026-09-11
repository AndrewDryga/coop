package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// doctorProbe runs inside the box against the fixture and reports, check id by check id, whether
// each secret is shadowed and each non-secret stays visible, then the box's privilege posture
// (uid/caps/pids) as RESULT UID/CAPS/PIDS lines for the host to interpret. It speaks ids, not
// sentences: the wording belongs to the report (doctor_checks.go), the evidence belongs here.
const doctorProbe = `#!/bin/sh
cd /workspace 2>/dev/null || { echo "RESULT FAIL sandbox.workspace"; exit 1; }
empty() { [ -f "$1" ] && [ ! -s "$1" ]; }
check() { if "$@"; then echo "RESULT PASS $ID"; else echo "RESULT FAIL $ID"; fi; }
ID=sandbox.env               check empty .env
ID=sandbox.envrc             check empty .envrc
ID=sandbox.tfvars            check empty config/prod.tfvars
ID=sandbox.private_key       check empty deploy/id_ed25519
ID=sandbox.coopignore        check empty config/credentials.yaml
if [ -d secrets ] && [ -z "$(ls -A secrets 2>/dev/null)" ]; then echo "RESULT PASS sandbox.secret_directory"; else echo "RESULT FAIL sandbox.secret_directory"; fi
# A symlink must not read around the shadow: notes-link -> .env resolves to the emptied .env.
if [ -L notes-link ] && [ ! -s notes-link ]; then echo "RESULT PASS sandbox.secret_symlink"; else echo "RESULT FAIL sandbox.secret_symlink"; fi
if echo x >> .env 2>/dev/null; then echo "RESULT FAIL sandbox.readonly_decoy"; else echo "RESULT PASS sandbox.readonly_decoy"; fi
ID=sandbox.template          check test -s .env.example
ID=sandbox.source            check test -s src/app.js
if grep -rqs hunter2 . 2>/dev/null; then echo "RESULT FAIL sandbox.secret_value"; else echo "RESULT PASS sandbox.secret_value"; fi
# No host control plane: the agent must not be able to drive the host. The box ships only coop-entry
# (the entrypoint), never the orchestration CLI, and coop never mounts the docker socket. The ONE
# host control surface a loop box gets is the task socket (/coop/tasks/mcp.sock), asserted in its
# own section below: the eight task tools, nothing shell-, exec-, or file-shaped, and a lease it
# cannot override.
if command -v coop >/dev/null 2>&1; then echo "RESULT FAIL host.coop_cli"; else echo "RESULT PASS host.coop_cli"; fi
if [ -S /var/run/docker.sock ]; then echo "RESULT FAIL host.docker_socket"; else echo "RESULT PASS host.docker_socket"; fi
# Privilege posture (interpreted on the host — it depends on the image and runtime).
echo "RESULT UID $(id -u)"
echo "RESULT CAPS $(awk '/^CapEff/{print $2}' /proc/self/status 2>/dev/null)"
echo "RESULT PIDS $(cat /sys/fs/cgroup/pids.max 2>/dev/null || cat /sys/fs/cgroup/pids/pids.max 2>/dev/null)"
`

// doctorImage picks the image the probe runs in: the repo's own image when it is built (that is
// the box its agents actually get), then the shared base image, then a stock alpine stand-in.
// real reports whether one of coop's images was found — the stand-in lacks coop's non-root USER
// and toolchain, so the caller says so instead of printing a clean bill of health.
func doctorImage(repo string, cfg *config.Config, exists func(string) bool) (img string, real bool) {
	for _, candidate := range []string{box.ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride), cfg.BaseImage} {
		if candidate != "" && exists(candidate) {
			return candidate, true
		}
	}
	return "alpine", false
}

// cmdDoctor proves isolation by attacking it: it builds a fixture repo full of secrets and runs
// the box against it, checking that secrets are shadowed inside the sandbox, the box has no path to
// the host control plane (no coop CLI, no docker socket), the box is locked down (non-root,
// capabilities dropped, pids-limited), egress fails closed, a box scoped to one agent can't see a
// peer's credentials, and nothing leaks into a clone handoff.
func (a *app) cmdDoctor(args []string) (int, error) {
	if err := rejectArgs("doctor", args); err != nil {
		return 2, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return 1, reported("Could not check the Coop box",
			fmt.Sprintf("%s is unavailable.", runtimeTitle(a.rt.Name)),
			fmt.Sprintf("Start %s, then run coop doctor again.", runtimeTitle(a.rt.Name)))
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	fixture, err := buildFixture()
	if err != nil {
		return 1, reported("Could not prepare the isolation checks", sentence(err.Error()),
			"Fix the temporary-directory permissions, then run coop doctor again.")
	}
	defer os.RemoveAll(fixture)

	// The probe lives outside the fixture: it must not appear in /workspace, or its own "hunter2"
	// grep pattern would trip the secret-value check. World-readable so the box can run it as a
	// uid that may not own it under --cap-drop ALL (see writeProbeFile).
	probe, cleanup, err := writeProbeFile(doctorProbe)
	if err != nil {
		return 1, reported("Could not prepare the isolation checks", sentence(err.Error()),
			"Fix the temporary-directory permissions, then run coop doctor again.")
	}
	defer cleanup()

	// Probe the image THIS repo's boxes run — the per-project one when its .agent/Dockerfile is
	// built (that is where a USER root or extra tooling would weaken the checks), else the shared
	// base image, else a stock alpine stand-in so doctor still works before a first `coop build`.
	img, usingReal := doctorImage(repo, a.cfg, func(image string) bool { return box.ImageExists(a.rt, image) })
	doctorHeader(a.rt.Name, img, a.cfg.BaseImage, usingReal)

	// The OCI privilege limits (cap-drop ALL, pids, no-new-privileges) are docker/podman-only
	// (box.boxLimits). On any other runtime they're simply not applied, so the uid/caps checks
	// below can't vouch for them — each says so on its own line rather than passing vacuously.
	hardened := a.rt.Name == "docker" || a.rt.Name == "podman"

	report := &doctorReport{}
	secrets, host, offline, taskSection, credentials, cloneSection := report.doctorSections()

	var out, errOut bytes.Buffer
	_, runErr := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: fixture, Workdir: "/workspace", Cmd: []string{"sh", "/probe.sh"},
		Batch: true, Quiet: true, Stdout: &out, Stderr: &errOut,
		ExtraArgs: []string{"-v", probe + ":/probe.sh:ro"},
	})
	results := parseProbeResults(out.String())
	switch {
	case runErr != nil || out.Len() == 0:
		secrets.probeFailed("Could not run the sandbox checks", probeReason(errOut.String(), runErr, "The sandbox probe produced no output."), len(doctorSecretChecks))
		host.unrun("Host access and privileges could not be checked", len(doctorHostChecks)+3)
	case results["sandbox.workspace"] == "FAIL":
		secrets.probeFailed("The test workspace is not mounted", "/workspace", len(doctorSecretChecks))
		host.unrun("Host access and privileges could not be checked", len(doctorHostChecks)+3)
	default:
		for _, def := range doctorSecretChecks {
			secrets.record(def, results[def.id] == "PASS")
		}
		for _, def := range doctorHostChecks {
			host.record(def, results[def.id] == "PASS")
		}
		doctorCheckUID(host, results["UID"], usingReal)
		doctorCheckCaps(host, results["CAPS"], hardened)
		doctorCheckPids(host, results["PIDS"], hardened, a.cfg.Pids)
	}

	// Egress fails closed: a run is asked for a network (Network:true) but with COOP_EGRESS=none;
	// the box must still come up with only loopback, proving the toggle cuts outbound regardless.
	doctorCheckEgress(offline, a, fixture, img)

	// The one host control surface a loop box gets: spoken to through the real transport from
	// inside a box, it must answer only the task tools and refuse a task another live process holds.
	if usingReal {
		doctorCheckTaskChannel(taskSection, a, fixture, img)
	} else {
		taskSection.skip("Task channel not checked", "", 4)
	}

	// A scoped agent box must preserve both the credential boundary and a writable application
	// config home under the normal generated-mount composition.
	doctorCheckCredAndHomeScope(credentials, a, fixture, img, usingReal)

	clone := fixture + "-clone"
	defer os.RemoveAll(clone)
	doctorCheckClone(cloneSection, fixture, clone)

	code := a.doctorReportOrphanBoxes(report)
	if verdict := report.print(); verdict != 0 {
		code = verdict
	}
	return code, nil
}

// doctorHeader says what is being checked, and on what. The ordinary shared image is what
// everyone gets, so naming it every run is noise; a project image, an explicit override, or the
// stand-in changes WHICH checks the report covers, so those are named. A stand-in also earns the
// notice above the report, because a partial bill of health must not read as a full one.
func doctorHeader(runtimeName, image, baseImage string, usingReal bool) {
	ui.Note("Checking the Coop box on the %s runtime", runtimeTitle(runtimeName))
	if image != baseImage {
		ui.Note("  Image:   %s", image)
	}
	if !usingReal {
		ui.Note("")
		warnBlock("No built Coop image was found",
			"Alpine is being used for the checks below.\nThe non-root user, task channel, and settings permissions cannot be checked.",
			"Run coop build, then coop doctor.")
	}
}

// probeMeasurements are the RESULT kinds that carry a MEASURED value rather than a verdict:
// `RESULT UID 1000` is read as UID → "1000", where `RESULT PASS x` is read as x → "PASS". A kind
// missing from this list is dropped silently and its check then reads as failed, which is how the
// writable-config-home check reported failure on every host from 2026-07 until this list existed
// (TestProbeKindsAreAllParsed keeps the two in step).
var probeMeasurements = []string{"UID", "CAPS", "PIDS", "HOME"}

// parseProbeResults reads the probe's RESULT lines into id → verdict. The measured kinds come back
// under those same keys with their measured value, since the host interprets them.
func parseProbeResults(stdout string) map[string]string {
	results := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		rest, ok := strings.CutPrefix(line, "RESULT ")
		if !ok {
			continue
		}
		kind, value, _ := strings.Cut(rest, " ")
		switch {
		case kind == "PASS" || kind == "FAIL":
			results[strings.TrimSpace(value)] = kind
		case slices.Contains(probeMeasurements, kind):
			results[kind] = strings.TrimSpace(value)
		}
	}
	return results
}

// probeReason is the bounded cause of a probe that produced nothing: the runtime's last stderr
// line, or the run error, or — when neither said anything — a statement of exactly that.
func probeReason(errOut string, runErr error, fallback string) string {
	why := strings.TrimSpace(errOut)
	if why == "" && runErr != nil {
		why = runErr.Error()
	}
	if why == "" {
		return fallback
	}
	return sentence(strings.TrimSpace(why[strings.LastIndex(why, "\n")+1:]))
}

// doctorReportOrphanBoxes reports this repo's boxes whose supervising coop process is provably gone
// — the count, the ids, and the label each finding rests on. It only ever LOOKS: doctor diagnoses,
// and removing a container is the sweep's job at the entry points that start work. It stays OUT of
// the pass/fail tally too — an orphan is host hygiene, not a hole in the isolation doctor attacks.
// A healthy survey prints nothing: there is no finding to report.
func (a *app) doctorReportOrphanBoxes(report *doctorReport) int {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), orphanSweepTimeout)
	defer cancel()
	survey, err := box.SurveyOrphanBoxes(ctx, a.rt, repo)
	if err != nil {
		// Fail closed and say so: an unanswerable runtime is not evidence that nothing is orphaned.
		report.section("Running boxes").add(doctorRow{outcome: doctorNote,
			label: "Could not check for abandoned boxes", reason: sentence(firstLine(err))})
		return 0
	}
	if len(survey.Orphans) > 0 {
		ids := make([]string, 0, len(survey.Orphans))
		for _, orphan := range survey.Orphans {
			ids = append(ids, orphan.ID)
		}
		report.section("Abandoned boxes").add(doctorRow{outcome: doctorNote,
			label:   fmt.Sprintf("%s no running supervisor", ui.Count(len(survey.Orphans), "box has", "boxes have")),
			details: ids,
			footer:  "They will be cleaned up when Coop next starts work or builds an image."})
	}
	if n := len(survey.Unattributed); n > 0 {
		report.section("Running boxes").add(doctorRow{outcome: doctorNote,
			label:   fmt.Sprintf("Could not identify the supervisor for %s", ui.Count(n, "box", "boxes")),
			details: survey.Unattributed,
			footer:  "Check that the box is no longer needed before removing it manually."})
	}
	return 0
}

// doctorCheckUID interprets the box's uid. Only the real box image carries coop's non-root USER
// (node); the alpine fallback is root by default, so there a root uid proves nothing about the
// image a person will actually run.
func doctorCheckUID(s *doctorSection, uid string, usingReal bool) {
	switch uid = strings.TrimSpace(uid); {
	case !usingReal:
		s.skip("Non-root user not checked", "", 1)
	case uid == "0":
		s.fail("The box runs as root", "The image uses user ID 0.",
			"Set a non-root USER in .agent/Dockerfile, then run coop build and coop doctor.")
	default:
		s.pass("The box runs as a non-root user")
	}
}

// doctorCheckCaps interprets the box's effective capabilities. --cap-drop ALL is applied only on
// docker/podman; on any other runtime the limit is simply not applied, which is a different
// statement from a limit that failed to take effect.
func doctorCheckCaps(s *doctorSection, caps string, hardened bool) {
	switch caps = strings.TrimSpace(caps); {
	case !hardened:
		s.add(doctorRow{outcome: doctorNotApplied, label: "Linux capability limits are not applied by this runtime"})
	case caps == "":
		s.skip("Linux capabilities could not be checked", "/proc did not provide CapEff.", 1)
	case strings.Trim(caps, "0") == "":
		s.pass("Linux capabilities are removed")
	default:
		s.fail("Linux capabilities were not removed", "The box reports CapEff="+caps+".", "")
	}
}

// doctorCheckPids interprets the box's pids cgroup limit. Like capabilities it is docker/podman
// only, and it can be turned off deliberately — a disabled limit and an unreadable one are
// different answers, and neither is a pass.
func doctorCheckPids(s *doctorSection, pids string, hardened bool, configured string) {
	switch pids = strings.TrimSpace(pids); {
	case !hardened:
		s.add(doctorRow{outcome: doctorNotApplied, label: "Process limits are not applied by this runtime"})
	case configured == "" || configured == "0" || configured == "-1" || configured == "unlimited":
		s.add(doctorRow{outcome: doctorDisabled, label: fmt.Sprintf("Process limit disabled by COOP_PIDS=%q", configured)})
	case pids == "":
		s.skip("Process limit could not be checked", "The runtime did not expose a readable process limit.", 1)
	case pids == "max":
		s.fail("The process limit is not enforced", "The box reports no process limit.", "")
	default:
		s.pass(fmt.Sprintf("The process limit is enforced (%s)", pids))
	}
}

// doctorCheckEgress proves COOP_EGRESS=none cuts the box off the network even when a run asks for
// one. It runs a box with Egress forced to none and Network requested, and checks only loopback
// came up — reliable offline, since --network none leaves just `lo` with no host connectivity.
func doctorCheckEgress(s *doctorSection, a *app, fixture, img string) {
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
		s.fail("Offline mode still has network access", "Interfaces present: "+strings.Join(external, " ")+".", "")
	case len(ifaces) == 1 && ifaces[0] == "lo":
		s.pass("Offline mode leaves only the loopback interface")
	default:
		s.skip("Offline mode could not be checked", probeReason(errOut.String(), err, "The offline box produced no interface list."), 1)
	}
}

// doctorTaskProbe runs inside a box given the task channel and speaks JSON-RPC to it over the
// exact command the agents' MCP clients use. One connection carries every request; the replies
// come back in order and the host matches them by id (doctorCheckTaskChannel).
const doctorTaskProbe = `#!/bin/sh
[ -S /coop/tasks/mcp.sock ] || { echo "RESULT FAIL task.socket"; exit 0; }
{
  echo '{"jsonrpc":"2.0","id":"list","method":"tools/list"}'
  echo '{"jsonrpc":"2.0","id":"exec","method":"tools/call","params":{"name":"exec","arguments":{"command":"id"}}}'
  echo '{"jsonrpc":"2.0","id":"shell","method":"shell","params":{"command":"id"}}'
  echo '{"jsonrpc":"2.0","id":"held","method":"tools/call","params":{"name":"tasks_append_log","arguments":{"id":"theirs","entry":"doctor must not land here"}}}'
  echo '{"jsonrpc":"2.0","id":"mine","method":"tools/call","params":{"name":"tasks_append_log","arguments":{"id":"mine","entry":"doctor reached its own task"}}}'
} | socat -t 3 STDIO UNIX-CONNECT:/coop/tasks/mcp.sock | sed 's/^/REPLY /'
`

// doctorTaskChecks is how many ordinary checks the task-channel probe carries, so a probe that
// never answers reports the right number of uncompleted checks rather than silently shrinking
// the total.
const doctorTaskChecks = 4

// doctorCheckTaskChannel proves the task socket by attacking it from inside a box: it must list
// exactly the eight task tools and nothing shell-, exec-, or file-shaped; refuse a call outside
// that set; and refuse a mutation on a task another live process holds — here a task the doctor's
// own process leases through the same host authority a concurrent loop would — while the box's own
// assigned task stays reachable, so the refusals cannot pass vacuously on a dead channel.
func doctorCheckTaskChannel(s *doctorSection, a *app, fixture, img string) {
	fail := func(label, reason string) {
		s.probeFailed(label, sentence(reason), doctorTaskChecks)
	}
	queue := filepath.Join(fixture, ".agent", "tasks")
	for _, id := range []string{"mine", "theirs"} {
		dir := filepath.Join(queue, tasks.StateInProgress, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("Could not prepare the task-channel checks", err.Error())
			return
		}
		body := "---\nid: " + id + "\ntitle: " + id + "\n---\n\n# " + id + "\n\n**Context:** doctor\n\n**Acceptance criteria:** doctor\n\n**Approach:** doctor\n\n## Subtasks\n- [ ] probe\n"
		if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(body), 0o644); err != nil {
			fail("Could not prepare the task-channel checks", err.Error())
			return
		}
	}
	if err := tasks.ScaffoldStateDirs(queue); err != nil {
		fail("Could not prepare the task-channel checks", err.Error())
		return
	}
	theirs, ok, err := tasks.CurrentTask(queue, "theirs")
	if err != nil || !ok {
		fail("Could not prepare the task-channel checks", fmt.Sprintf("The fixture task could not be read: %v.", err))
		return
	}
	lease, observed, err := tasks.TryTaskLease(queue, theirs, tasks.TaskLeaseOwner{RunID: "doctor", PID: os.Getpid(), Provider: "doctor", Target: "doctor"})
	if err != nil || lease == nil {
		fail("Could not reserve the test task", fmt.Sprintf("%v %v", err, observed))
		return
	}
	defer func() { _ = lease.Release() }()
	server, err := taskmcp.New(taskmcp.Authority{QueueRoots: []string{queue}, Assigned: "mine"})
	if err != nil {
		fail("Could not start the test task server", err.Error())
		return
	}
	probe, cleanup, err := writeProbeFile(doctorTaskProbe)
	if err != nil {
		fail("Could not prepare the task-channel probe", err.Error())
		return
	}
	defer cleanup()
	var out, errOut bytes.Buffer
	_, runErr := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: fixture, Workdir: "/workspace", Cmd: []string{"sh", "/probe-tasks.sh"},
		Batch: true, Quiet: true, Stdout: &out, Stderr: &errOut,
		ExtraArgs: []string{"-v", probe + ":/probe-tasks.sh:ro"},
		TaskTools: server,
	})
	replies := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.HasPrefix(line, "RESULT FAIL task.socket") {
			s.probeFailed("The task channel is not mounted", "/coop/tasks/mcp.sock", doctorTaskChecks)
			return
		}
		raw, ok := strings.CutPrefix(line, "REPLY ")
		if !ok {
			continue
		}
		var reply map[string]any
		if json.Unmarshal([]byte(raw), &reply) != nil {
			continue
		}
		if id, _ := reply["id"].(string); id != "" {
			replies[id] = reply
		}
	}
	if len(replies) == 0 {
		s.probeFailed("Could not run the task-channel checks",
			probeReason(errOut.String(), runErr, "The task channel did not respond."), doctorTaskChecks)
		return
	}
	// 1. Exactly the task tools — by the fixed set AND by shape, so a renamed tool cannot slip a
	//    shell in under a task-sounding name.
	var names []string
	if result, _ := replies["list"]["result"].(map[string]any); result != nil {
		if list, _ := result["tools"].([]any); list != nil {
			for _, tool := range list {
				if m, _ := tool.(map[string]any); m != nil {
					names = append(names, fmt.Sprint(m["name"]))
				}
			}
		}
	}
	shaped := ""
	for _, name := range names {
		for _, verb := range []string{"shell", "exec", "file", "bash", "read", "write", "run"} {
			if strings.Contains(strings.ToLower(name), verb) {
				shaped = name
			}
		}
	}
	switch {
	case shaped != "":
		s.fail("The task channel exposes unexpected tools", "It offers a shell-shaped tool: "+shaped+".", "")
	case !slices.Equal(names, taskmcp.ToolNames()):
		s.fail("The task channel exposes unexpected tools",
			fmt.Sprintf("It offers %v, not %v.", names, taskmcp.ToolNames()), "")
	default:
		s.pass(fmt.Sprintf("The task channel exposes only its %d task tools", len(names)))
	}
	// 2. A call outside the tool set — an unknown tool and an unknown method — is refused.
	refusedExec := replies["exec"]["error"] != nil && replies["exec"]["result"] == nil
	refusedShell := replies["shell"]["error"] != nil && replies["shell"]["result"] == nil
	switch {
	case refusedExec && refusedShell:
		s.pass("Calls outside the task tools are refused")
	default:
		s.fail("The task channel did not refuse an unsupported call",
			strings.Join(answered(refusedExec, refusedShell), " and ")+" answered.", "")
	}
	// 3. The lease: a task another live process holds is refused at the call; the box's own task
	//    is reachable (the positive control).
	if text, isError := toolReply(replies["held"]); isError && strings.Contains(text, "held by another live process") {
		s.pass("Changes to a task held by another process are refused")
	} else {
		s.fail("The task channel did not refuse a change to another process's task", sentence(text), "")
	}
	if log, _ := os.ReadFile(filepath.Join(queue, tasks.StateInProgress, "theirs", "log.md")); strings.Contains(string(log), "doctor must not land here") {
		s.fail("A refused change still reached the held task's log", "log.md", "")
	}
	if text, isError := toolReply(replies["mine"]); !isError && strings.Contains(text, "appended") {
		s.pass("The assigned task can be updated")
	} else {
		s.fail("The assigned task could not be updated", sentence(text), "")
	}
}

// answered names which unsupported calls came back with a result instead of a refusal, without
// echoing any part of the reply payload.
func answered(refusedExec, refusedShell bool) []string {
	var out []string
	if !refusedExec {
		out = append(out, "exec")
	}
	if !refusedShell {
		out = append(out, "shell")
	}
	return out
}

// doctorCheckClone proves the fork handoff: a clone of the fixture carries the tracked source and
// none of the shadowed secrets, and has nowhere to push to.
func doctorCheckClone(s *doctorSection, fixture, clone string) {
	if err := exec.Command("git", "clone", "-q", fixture, clone).Run(); err != nil {
		s.probeFailed("Could not check the fork handoff", sentence(err.Error()), len(doctorCloneChecks))
		return
	}
	present := map[string]bool{
		"clone.env":     pathExists(filepath.Join(clone, ".env")),
		"clone.envrc":   pathExists(filepath.Join(clone, ".envrc")),
		"clone.secrets": pathExists(filepath.Join(clone, "secrets")),
		"clone.keys":    pathExists(filepath.Join(clone, "deploy")),
	}
	for _, def := range doctorCloneChecks {
		switch def.id {
		case "clone.source":
			s.record(def, fileExists(filepath.Join(clone, "src", "app.js")))
		case "clone.secret_value":
			s.record(def, !treeContains(clone, "hunter2"))
		case "clone.origin":
			origin, _ := exec.Command("git", "-C", clone, "remote", "get-url", "origin").Output()
			s.record(def, strings.HasPrefix(strings.TrimSpace(string(origin)), "/"))
		default:
			s.record(def, !present[def.id])
		}
	}
}

// toolReply extracts a tools/call reply's text and error flag; a protocol error reads as an error.
func toolReply(reply map[string]any) (string, bool) {
	if reply == nil {
		return "no reply", true
	}
	if e, ok := reply["error"].(map[string]any); ok {
		return fmt.Sprint(e["message"]), true
	}
	result, _ := reply["result"].(map[string]any)
	if result == nil {
		return "no result", true
	}
	isError, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return "", isError
	}
	first, _ := content[0].(map[string]any)
	return fmt.Sprint(first["text"]), isError
}

// writeProbeFile writes a probe script to a world-readable temp file, so the box can run it as a
// uid that may not own it (and, under --cap-drop ALL, can't bypass the read check). Returns the
// path and a cleanup func.
func writeProbeFile(content string) (string, func(), error) {
	f, err := os.CreateTemp("", "coop-probe-*.sh")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	_ = f.Close()
	cleanup := func() { _ = os.Remove(path) }
	// CreateTemp made it 0600 and WriteFile won't widen an existing file's mode, so chmod after.
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// doctorCredAndHomeProbe checks, inside a box scoped to claude, that only claude's credentials are
// visible and the normal mount composition leaves its application config home writable.
func doctorCredAndHomeProbe(home string) string {
	return fmt.Sprintf(`#!/bin/sh
check() { if "$@"; then echo "RESULT PASS $ID"; else echo "RESULT FAIL $ID"; fi; }
ID=credential.own_home    check test -f "%[1]s/.claude/.credentials.json"
ID=credential.codex_home  check test ! -e "%[1]s/.codex/auth.json"
ID=credential.gemini_home check test ! -e "%[1]s/.gemini/gemini-credentials.json"
ID=credential.own_env     check test -z "$ANTHROPIC_API_KEY"
ID=credential.peer_env    check test -z "$OPENAI_API_KEY"
ID=credential.peer_alias  check test -z "$GOOGLE_API_KEY"
if mkdir -p "%[1]s/.config/coop-browser-probe" && : > "%[1]s/.config/coop-browser-probe/write"; then
	rm -rf "%[1]s/.config/coop-browser-probe"
	echo "RESULT HOME writable"
else
	echo "RESULT HOME blocked"
fi
`, home)
}

// doctorCheckCredAndHomeScope proves the credential boundary and writable config-home contract in
// one normally composed box. It seeds a throwaway credential for every agent and an env file
// holding every agent's key, then runs a claude-scoped probe that also creates state under
// ~/.config — exercising credentialScope, generated home mounts, and writeFilteredEnvFile.
func doctorCheckCredAndHomeScope(s *doctorSection, a *app, fixture, img string, usingReal bool) {
	covers := len(doctorCredentialChecks) + 1
	cfgDir, err := os.MkdirTemp("", "coop-doctor-cred-")
	if err != nil {
		s.probeFailed("Could not prepare the credential checks", sentence(err.Error()), covers)
		return
	}
	defer os.RemoveAll(cfgDir)
	if err := os.Chmod(cfgDir, 0o755); err != nil { // box reads it as a non-owner uid
		s.probeFailed("Could not prepare the credential checks", sentence(err.Error()), covers)
		return
	}
	credCfg := *a.cfg
	credCfg.ConfigDir = cfgDir
	credCfg.MCPFile = filepath.Join(cfgDir, "mcp.json") // absent → no MCP wiring to stand up
	// Seed a fake credential per agent at its real mount source (cfg.AgentDir), so a claude-scoped
	// run mounts claude's and leaves the peers' behind. Each credential filename comes from the
	// agent's own AuthMarker — the single place a cred filename lives (agents-are-one-file), so a
	// new agent is exercised here automatically and this can't drift from the real mount.
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		credFile, _ := ag.AuthMarker()
		dir := credCfg.AgentDir(name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.probeFailed("Could not prepare the credential checks", sentence(err.Error()), covers)
			return
		}
		_ = os.WriteFile(filepath.Join(dir, credFile), []byte(`{"token":"hunter2"}`), 0o644)
	}
	// ANTHROPIC is claude's own, but claude also has a mounted login (the marker seeded above), so
	// its env token yields to that login and is dropped — a marker-backed account is never shadowed
	// by an env token. OPENAI is codex's; GOOGLE is one of gemini's keys, given bare so the filter
	// must drop a peer's alias AND a bare (env-imported) line.
	_ = os.WriteFile(credCfg.EnvFile(), []byte("ANTHROPIC_API_KEY=hunter2\nOPENAI_API_KEY=hunter2\nGOOGLE_API_KEY\n"), 0o644)

	probe, cleanup, err := writeProbeFile(doctorCredAndHomeProbe(credCfg.HomeInBox))
	if err != nil {
		s.probeFailed("Could not prepare the credential checks", sentence(err.Error()), covers)
		return
	}
	defer cleanup()

	var out, errOut bytes.Buffer
	_, runErr := box.Run(&credCfg, a.rt, box.RunSpec{
		Image: img, Repo: fixture, Agent: "claude", Homes: true,
		Cmd: []string{"sh", "/credprobe.sh"}, Batch: true, Quiet: true, Stdout: &out, Stderr: &errOut,
		ExtraArgs: []string{"-v", probe + ":/credprobe.sh:ro"},
	})
	if runErr != nil || out.Len() == 0 {
		s.probeFailed("Could not run the credential checks", probeReason(errOut.String(), runErr, "The credential-scope box produced no output."), covers)
		return
	}
	results := parseProbeResults(out.String())
	for _, def := range doctorCredentialChecks {
		s.record(def, results[def.id] == "PASS")
	}
	doctorCheckHome(s, results["HOME"], usingReal)
}

// doctorCheckHome interprets the config-home write probe. The Alpine fallback runs as root, so it
// cannot expose the ownership bug this check guards and must not report a false pass.
func doctorCheckHome(s *doctorSection, result string, usingReal bool) {
	switch {
	case !usingReal:
		s.skip("Settings permissions not checked", "", 1)
	case strings.TrimSpace(result) == "writable":
		s.pass("The box can write its settings directory")
	default:
		s.fail("The box cannot write its settings directory", "", "")
	}
}

// buildFixture creates a throwaway git repo seeded with secrets and decoys.
func buildFixture() (string, error) {
	dir, err := os.MkdirTemp("", "coop-doctor-")
	if err != nil {
		return "", fmt.Errorf("could not create a temporary project: %s", osCause(err))
	}
	// MkdirTemp makes the root 0700; the box mounts it at /workspace and the probe must cd into and
	// stat it as a uid that may not own it (and, under --cap-drop ALL, can't bypass the check). Make
	// the root world-traversable — the seeded files are already 0644 / subdirs 0755.
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", fmt.Errorf("could not create a temporary project: %s", osCause(err))
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
			return "", fmt.Errorf("could not create a temporary project: %s", osCause(err))
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return "", fmt.Errorf("could not create a temporary project: %s", osCause(err))
		}
	}
	// A symlink to a secret: shadowing must cover what it points at, so following it reads empty.
	if err := os.Symlink(".env", filepath.Join(dir, "notes-link")); err != nil {
		return "", fmt.Errorf("could not create a temporary project: %s", osCause(err))
	}
	cmds := [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=d@d", "-c", "user.name=d", "commit", "-qm", "init"},
	}
	for _, c := range cmds {
		cmd := exec.Command("git", append([]string{"-C", dir}, c...)...)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("could not prepare the temporary project with git %v: %v", c, err)
		}
	}
	return dir, nil
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
