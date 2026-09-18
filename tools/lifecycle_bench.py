#!/usr/bin/env python3
"""Measure how long a Coop box really takes to become usable, and to be gone.

This is a measurement tool, not telemetry: it runs the same commands a person runs, times the
boundaries that matter, and writes the samples it took. Nothing here is installed, nothing runs in
the background, and it never touches product code.

Two boundaries, both chosen so a UI label cannot flatter them:

  start  = the in-box command actually began. The box prints its own clock; the difference from
           the host clock taken immediately before the launch is the time a person waited before
           their work could run. A "Starting <agent>" line is not evidence of anything.
  stop   = the owned container is gone from the runtime AND the coop process has exited. A command
           that returns while its container is still being reaped has not stopped.

Both epochs come from the same machine's clock, and a container shares the host's clock, so the
subtraction is honest. Only the timezone differs, which the epoch form does not use.

Usage:
    tools/lifecycle_bench.py --samples 10 --out <dir>
    tools/lifecycle_bench.py --cases cold_start,repeat_start --samples 5 --out <dir>

Output: <dir>/samples.json (every raw sample) and <dir>/report.md (counts, p50, p95, the machine,
the versions, and the exact commands to repeat it).
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import pty
import re
import select
import shutil
import signal
import subprocess
import sys
import time
from dataclasses import dataclass, field, asdict
from pathlib import Path

# A sample that takes longer than this is a wedged environment, not a slow one. Bounded so an
# unattended run cannot hang the person who started it.
CASE_TIMEOUT_SECONDS = 300

# The box announces itself by creating a file in the mounted workspace, written relative to the
# box's workdir: the host path and the in-box path are not the same string, and only one of them
# exists on each side. The host watches for it, so the instant is the host's own.
READY_FILE = ".coop-bench-ready"
READY_PROBE = f": > ./{READY_FILE}"

HOME = str(Path.home())

# Everything a run can own. `coop=box` is the agent box; a filtered launch also creates a gateway
# guard and controller under coop.network.* (internal/box/filtered.go) plus volumes for their ipc
# and observations. A check that knows only the first reports "nothing left" while the second set
# is still there — which is precisely the failure this tool exists to catch, so it must not have it.
OWNED_CONTAINER_FILTERS = ("label=coop=box", "label=coop.network.run")
OWNED_VOLUME_FILTERS = ("label=coop=box", "label=coop.network.run")


def redact(text: str) -> str:
    """Strip this machine's identity from anything retained.

    Artifacts are read by other people: a home directory names its owner and a temp path names the
    run. Commit and version hashes are the opposite — they are the evidence — so they stay."""
    text = text.replace(HOME, "~")
    return re.sub(r"(?<![\w.])/(?:private/)?(?:var|tmp)/[\w.\-/]*", "<tmp>", text)


@dataclass
class Sample:
    case: str
    ok: bool
    seconds: float | None = None
    detail: str = ""
    extra: dict = field(default_factory=dict)


@dataclass
class CaseResult:
    name: str
    description: str
    boundary: str
    samples: list = field(default_factory=list)
    unverified: str = ""


def percentile(values: list[float], fraction: float) -> float:
    if not values:
        return float("nan")
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = fraction * (len(ordered) - 1)
    low = int(position)
    high = min(low + 1, len(ordered) - 1)
    return ordered[low] + (ordered[high] - ordered[low]) * (position - low)


def run(cmd: list[str], env: dict | None = None, timeout: int = CASE_TIMEOUT_SECONDS,
        cwd: str | None = None) -> subprocess.CompletedProcess:
    full = dict(os.environ)
    full.update(env or {})
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, env=full, cwd=cwd)


def read_ready_epoch(output: str) -> float | None:
    for line in output.splitlines():
        marker, _, value = line.partition(READY_MARKER + ":")
        if marker == line:
            continue
        try:
            return float(value.strip())
        except ValueError:
            return None
    return None


def owned_containers(runtime: str) -> set[str]:
    found: set[str] = set()
    for label in OWNED_CONTAINER_FILTERS:
        result = run([runtime, "ps", "-aq", "--filter", label], timeout=30)
        found.update("c:" + line for line in result.stdout.split() if line)
    return found


def owned_volumes(runtime: str) -> set[str]:
    found: set[str] = set()
    for label in OWNED_VOLUME_FILTERS:
        result = run([runtime, "volume", "ls", "-q", "--filter", label], timeout=30)
        found.update("v:" + line for line in result.stdout.split() if line)
    return found


def owned(runtime: str) -> set[str]:
    """Every container and volume a coop run could own, by the labels the product sets."""
    return owned_containers(runtime) | owned_volumes(runtime)


def remove_owned(runtime: str, leftovers: set[str]) -> None:
    """Take back what an interrupted sample left. The tool's own wreckage must not be mistaken for
    the product's, and must not poison the next sample's before-set."""
    for item in sorted(leftovers):
        kind, _, name = item.partition(":")
        command = ["rm", "-f", name] if kind == "c" else ["volume", "rm", "-f", name]
        run([runtime, *command], timeout=60)


def wait_until_gone(runtime: str, before: set[str], timeout: float) -> bool:
    """Poll the containers, then confirm the volumes once.

    Every runtime query costs ~20 ms, and a stop interval here is a few hundred milliseconds, so
    asking four questions per iteration would measure this loop instead of the product. Volumes are
    created and destroyed with the gateway that owns them, so the cheap poll watches containers and
    the volumes are checked when those are clear."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not (owned_containers(runtime) - before):
            break
        time.sleep(0.01)
    else:
        return False
    while time.time() < deadline:
        if not (owned_volumes(runtime) - before):
            return True
        time.sleep(0.05)
    return False


# --- the cases -------------------------------------------------------------------------------


def launch(coop: str, repo: str, extra: list[str], command: str) -> subprocess.Popen:
    """Start coop the way a script does, with its own session so a stop can reach the whole tree."""
    return subprocess.Popen([coop, "run", *extra, "--", "sh", "-c", command], cwd=repo,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                            start_new_session=True)


def await_marker(repo: str, process: subprocess.Popen, timeout: float) -> float | None:
    """Return the host instant the box announced itself, or None if it never did."""
    marker = os.path.join(repo, READY_FILE)
    deadline = time.time() + timeout
    while time.time() < deadline:
        if os.path.exists(marker):
            return time.time()
        if process.poll() is not None:
            return None
        time.sleep(0.005)
    return None


def clear_marker(repo: str) -> None:
    marker = os.path.join(repo, READY_FILE)
    if os.path.exists(marker):
        os.remove(marker)


def stop_process(process: subprocess.Popen) -> None:
    """End a sample's coop the way a person's terminal would, escalating only if it refuses.

    SIGKILL first is how this tool used to lose a filtered launch's gateway containers and volumes:
    coop was killed mid-teardown and the resources it had not reached yet stayed."""
    if process.poll() is not None:
        return
    for signal_number, grace in ((signal.SIGINT, 20.0), (signal.SIGTERM, 10.0), (signal.SIGKILL, 5.0)):
        try:
            os.killpg(process.pid, signal_number)
        except (ProcessLookupError, PermissionError):
            return
        try:
            process.communicate(timeout=grace)
            return
        except subprocess.TimeoutExpired:
            continue


def sample_run(coop: str, repo: str, runtime: str, extra: list[str], command: str,
               measure: str) -> Sample:
    """One launch, measured at `measure` ("start" or "stop"), and cleaned up whatever happens."""
    clear_marker(repo)
    before = owned(runtime)
    launched = time.time()
    process = launch(coop, repo, extra, command)
    try:
        ready = await_marker(repo, process, CASE_TIMEOUT_SECONDS)
        if ready is None:
            out, err = process.communicate(timeout=CASE_TIMEOUT_SECONDS)
            return Sample("", False, detail=redact((err or out)[-400:]) or "the box never announced itself")
        if measure == "start":
            process.communicate(timeout=CASE_TIMEOUT_SECONDS)
            if not wait_until_gone(runtime, before, 60):
                return Sample("", False, detail="a finished run left a resource behind")
            return Sample("", True, seconds=ready - launched)
        process.communicate(timeout=CASE_TIMEOUT_SECONDS)
        returned = time.time()
        if not wait_until_gone(runtime, before, 60):
            return Sample("", False, detail="an owned resource outlived the command by 60s")
        gone = time.time()
        return Sample("", True, seconds=gone - ready,
                      extra={"after_process_returned_s": round(gone - returned, 4),
                             "probe_resolution_s": PROBE_RESOLUTION_NOTE})
    finally:
        stop_process(process)
        clear_marker(repo)
        leftovers = owned(runtime) - before
        if leftovers:
            remove_owned(runtime, leftovers)


def case_start(coop: str, repo: str, runtime: str, extra: list[str]) -> Sample:
    """Time from "the person pressed enter" to "their command was running in the box"."""
    return sample_run(coop, repo, runtime, extra, READY_PROBE, "start")


def case_stop(coop: str, repo: str, runtime: str, extra: list[str]) -> Sample:
    """Time from the in-box work announcing itself to nothing this run owned remaining.

    The clock stops when the runtime lists no container OR VOLUME this run created and coop has
    exited. Returning early is exactly the failure this measurement exists to catch."""
    return sample_run(coop, repo, runtime, extra, READY_PROBE, "stop")


def case_failed_start_preflight(coop: str, repo: str, runtime: str, extra: list[str]) -> Sample:
    """A launch refused before anything is created. Cheap by construction — the interesting number
    is how fast a person learns, not how much was cleaned up, because nothing was."""
    before = owned(runtime)
    started = time.time()
    result = run([coop, "run", *extra, "--", "sh", "-c", "true"], cwd=repo,
                 env={"COOP_IMAGE": "coop-bench-no-such-image:absent"})
    elapsed = time.time() - started
    if result.returncode == 0:
        return Sample("", False, detail="a missing image did not fail the launch")
    if owned(runtime) - before:
        return Sample("", False, detail="a refused launch created a resource")
    return Sample("", True, seconds=elapsed)


def case_failed_start_after_create(coop: str, repo: str, runtime: str, extra: list[str]) -> Sample:
    """A launch that fails AFTER the box exists: the box is built, mounted and started, and the
    command inside it does not exist. This is where cleanup work actually lives, which the
    preflight refusal never touches."""
    before = owned(runtime)
    started = time.time()
    result = run([coop, "run", *extra, "--", "/no/such/binary"], cwd=repo)
    elapsed = time.time() - started
    if result.returncode == 0:
        return Sample("", False, detail="a missing in-box command did not fail the run")
    if not wait_until_gone(runtime, before, 60):
        leftovers = owned(runtime) - before
        remove_owned(runtime, leftovers)
        return Sample("", False, detail=f"a failed run left {len(leftovers)} resource(s) behind")
    return Sample("", True, seconds=elapsed)


def case_cancelled_stop(coop: str, repo: str, runtime: str, extra: list[str]) -> Sample:
    """Ctrl-C at a box that is doing work: how long until nothing of it is left.

    On a REAL terminal, because that is the only place this is measurable. Coop runs the runtime
    client in coop's OWN foreground process group precisely so the terminal delivers ^C to the
    client — and through it to the container. Signalling a process group from a script instead
    reaches coop and not the workload: measured that way this case reported that cancellation never
    happened at all, while a person pressing ^C gets an exit in a fraction of a second."""
    clear_marker(repo)
    before = owned(runtime)
    marker = os.path.join(repo, READY_FILE)
    pid, fd = pty.fork()
    if pid == 0:  # child: become coop on the far side of the terminal
        try:
            os.chdir(repo)
            os.execv(coop, [coop, "run", *extra, "--", "sh", "-c", f"{READY_PROBE}; sleep 120"])
        finally:
            os._exit(127)

    def drain(timeout: float = 0.01) -> None:
        readable, _, _ = select.select([fd], [], [], timeout)
        if readable:
            try:
                os.read(fd, 4096)
            except OSError:
                pass

    try:
        deadline = time.time() + CASE_TIMEOUT_SECONDS
        while time.time() < deadline and not os.path.exists(marker):
            drain()
        if not os.path.exists(marker):
            return Sample("", False, detail="the box never announced itself")
        signalled = time.time()
        os.write(fd, b"\x03")  # ^C on the terminal, exactly as a person types it
        exited = False
        while time.time() < signalled + 90:
            drain()
            if os.waitpid(pid, os.WNOHANG)[0]:
                exited = True
                break
        if not exited:
            return Sample("", False, detail="coop did not exit within 90s of ^C")
        if not wait_until_gone(runtime, before, 60):
            return Sample("", False, detail="a cancelled run left a resource behind")
        return Sample("", True, seconds=time.time() - signalled)
    finally:
        try:
            os.killpg(pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
        os.close(fd)  # hangs up the session; leaving it open leaked a descriptor per sample
        clear_marker(repo)
        leftovers = owned(runtime) - before
        if leftovers:
            remove_owned(runtime, leftovers)


def case_acp_initialize(coop: str, repo: str, runtime: str, extra: list[str]) -> Sample:
    """How long an editor waits before Coop's ACP agent answers it.

    This is the editor-facing half of "start": the editor speaks ACP over stdio and cannot do
    anything until `initialize` comes back. It is measured with the real protocol rather than by
    watching for a process, because a process that exists has not answered anybody. Note what is
    inside the number: the proxy spawns the lead's box immediately, so this includes a real box and
    its ACP adapter, and the warm pool may be fanning other providers out concurrently."""
    before = owned(runtime)
    request = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize",
                          "params": {"protocolVersion": 1,
                                     "clientCapabilities": {"fs": {"readTextFile": False,
                                                                   "writeTextFile": False}}}})
    started = time.time()
    process = subprocess.Popen([coop, "acp", *extra], cwd=repo, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                               start_new_session=True)
    try:
        process.stdin.write(request + "\n")
        process.stdin.flush()
        deadline = time.time() + CASE_TIMEOUT_SECONDS
        while time.time() < deadline:
            line = process.stdout.readline()
            if not line:
                break
            answer = initialize_result(line)
            if answer is not None:
                return Sample("", True, seconds=time.time() - started,
                              extra={"agent": answer.get("agentInfo", answer.get("agent", {}))})
        return Sample("", False, detail="the ACP agent never answered initialize")
    finally:
        stop_process(process)
        leftovers = owned(runtime) - before
        if leftovers:
            remove_owned(runtime, leftovers)


def initialize_result(line: str) -> dict | None:
    """The JSON-RPC answer to request 1, or None. Parsed, not substring-matched: a notification or a
    log line that merely contains the right characters is not an answer."""
    try:
        message = json.loads(line)
    except (ValueError, TypeError):
        return None
    if not isinstance(message, dict) or message.get("id") != 1:
        return None
    result = message.get("result")
    return result if isinstance(result, dict) else None


CASES = {
    "repeat_start": ("start with everything already warm on the host", "start", case_start, []),
    "filtered_start": ("start with --egress filtered (the qualified gateway)", "start", case_start,
                       ["--egress", "filtered"]),
    "acp_initialize": ("editor start: from launching coop acp to its answer to initialize", "start",
                       case_acp_initialize, []),
    "normal_stop": ("from the box announcing itself to nothing owned remaining", "stop", case_stop, []),
    "filtered_stop": ("the same, for a filtered run and its gateway", "stop", case_stop,
                      ["--egress", "filtered"]),
    "cancelled_stop": ("from Ctrl-C to nothing owned remaining", "stop", case_cancelled_stop, []),
    "failed_start_preflight": ("a launch refused before anything is created", "start",
                               case_failed_start_preflight, []),
    "failed_start_after_create": ("a launch that fails after the box exists, and its cleanup",
                                  "start", case_failed_start_after_create, []),
}

# A cold case is deliberately absent. "Cold" would have to mean a precondition this tool does not
# control — a daemon that has not seen the image, caches discarded — and taking one sample and
# calling it cold measures the warm path under another name. Record it as not measured instead of
# measuring something else.

# One `docker ps` round trip is the floor on any "is it gone yet" answer, so a stop number smaller
# than this is at the probe's resolution, not the product's.
PROBE_RESOLUTION_NOTE = "two runtime queries (~0.04s) are the floor on any gone-yet answer"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--samples", type=int, default=5, help="samples per case (default 5)")
    parser.add_argument("--out", required=True, help="directory for samples.json and report.md")
    parser.add_argument("--cases", default=",".join(CASES), help="comma-separated case names")
    parser.add_argument("--coop", default=shutil.which("coop") or "./coop", help="the coop binary to measure")
    parser.add_argument("--repo", default=os.getcwd(), help="the repository to launch in")
    args = parser.parse_args()

    # Absolute, always: every case runs with cwd set to the workspace, so a relative name — even one
    # `which` happily confirms here — resolves against the wrong directory there and fails as "no
    # such file" on the first sample.
    coop = str(Path(shutil.which(args.coop) or args.coop).resolve())
    if not os.access(coop, os.X_OK):
        print(f"lifecycle_bench: {args.coop} is not executable", file=sys.stderr)
        return 2
    runtime = os.environ.get("COOP_RUNTIME") or "docker"

    selected = [name.strip() for name in args.cases.split(",") if name.strip()]
    unknown = [name for name in selected if name not in CASES]
    if unknown:
        print(f"lifecycle_bench: unknown case(s): {', '.join(unknown)}", file=sys.stderr)
        return 2

    results: list[CaseResult] = []
    interrupted = ""
    try:
        for name in selected:
            description, boundary, function, extra = CASES[name]
            case = CaseResult(name=name, description=description, boundary=boundary)
            results.append(case)
            for _ in range(args.samples):
                try:
                    sample = function(coop, args.repo, runtime, extra)
                except subprocess.TimeoutExpired:
                    sample = Sample("", False, detail=f"case exceeded {CASE_TIMEOUT_SECONDS}s")
                sample.case = name
                case.samples.append(sample)
    except KeyboardInterrupt:
        # Samples already taken are the expensive part; losing them to a traceback is the one
        # failure mode this tool must not have. Each case cleans up after itself in its own finally.
        interrupted = "interrupted before every case finished — the samples below are what was taken"

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    environment = collect_environment(coop, runtime)
    workspace = describe_workspace(args.repo)
    if interrupted and results:
        results[-1].unverified = interrupted
    (out / "samples.json").write_text(json.dumps(
        {"environment": environment, "workspace": workspace,
         "cases": [{**asdict(case), "samples": [asdict(s) for s in case.samples]} for case in results]},
        indent=2, sort_keys=True) + "\n")
    (out / "report.md").write_text(render_report(results, environment, args, workspace))
    print(f"lifecycle_bench: wrote {out / 'samples.json'} and {out / 'report.md'}")
    return 1 if interrupted else 0


def describe_workspace(repo: str) -> dict:
    """What was measured IN. A near-empty directory and a real project are different products: a
    project policy can make every run filtered, and secret shadowing, image selection and mount
    assembly all scale with the tree. A number without this is not reproducible."""
    tracked = "unknown"
    result = run(["git", "-C", repo, "ls-files"], timeout=60)
    if result.returncode == 0:
        tracked = str(len(result.stdout.split("\n")) - 1)
    head = run(["git", "-C", repo, "rev-parse", "--short", "HEAD"], timeout=30)
    absolute = os.path.abspath(repo)
    return {
        # The basename survives redaction on purpose: WHICH workspace is the whole point, and
        # "<tmp>" for every one of them makes two different products look like one measurement.
        "path": os.path.join(redact(os.path.dirname(absolute)), os.path.basename(absolute)),
        "tracked_files": tracked,
        "head": head.stdout.strip() if head.returncode == 0 else "none",
        "has_agent_dir": os.path.isdir(os.path.join(repo, ".agent")),
        "has_project_policy": os.path.exists(os.path.join(repo, ".agent", "project.yaml")),
        "has_box_dockerfile": os.path.exists(os.path.join(repo, ".agent", "Dockerfile")),
    }


def collect_environment(coop: str, runtime: str) -> dict:
    def first_line(cmd: list[str]) -> str:
        try:
            result = run(cmd, timeout=30)
        except (OSError, subprocess.TimeoutExpired):
            return "unavailable"
        text = (result.stdout or result.stderr).strip().splitlines()
        return redact(text[0]) if text else "unavailable"

    # The binary's OWN stamp, not the checkout's HEAD: they are routinely different, and a report
    # that names a commit the measured binary was not built from is worse than one that admits it.
    version = first_line([coop, "version"])
    memory = "unknown"
    try:
        if platform.system() == "Darwin":
            memory = str(int(run(["sysctl", "-n", "hw.memsize"], timeout=10).stdout.strip()) >> 30) + " GiB"
        else:
            with open("/proc/meminfo") as handle:
                memory = str(int(handle.readline().split()[1]) >> 20) + " GiB"
    except (OSError, ValueError, IndexError):
        pass
    daemon = "unknown"
    info = run([runtime, "info", "--format",
                "{{.OperatingSystem}} / {{.ServerVersion}} / {{.NCPU}} cpus / {{.MemTotal}} bytes"],
               timeout=60)
    if info.returncode == 0 and info.stdout.strip():
        daemon = redact(info.stdout.strip().splitlines()[0])
    return {
        "coop_version": version,
        "binary_provenance": ("built from a tree with uncommitted changes — cite the commit it became"
                              if "dirty" in version else "built from a committed tree"),
        "runtime": runtime,
        "runtime_version": first_line([runtime, "--version"]),
        # The daemon, not the host: a Linux container on macOS runs inside the runtime's VM, which
        # has its own CPU and memory budget and its own clock.
        "runtime_daemon": daemon,
        "host_os": f"{platform.system()} {platform.release()} {platform.machine()}",
        "host_cpus": os.cpu_count(),
        "host_memory": memory,
        "measured_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }


def render_report(results: list[CaseResult], environment: dict, args, workspace: dict) -> str:
    lines = ["# Agent lifecycle baseline", "",
             "Start is when the host saw the box announce itself; stop is when nothing the run owned",
             "is left in the runtime. Both instants are the host's own clock, never a UI line and",
             "never the container's clock.", "", "## Machine", ""]
    for key in ("coop_version", "binary_provenance", "runtime", "runtime_version", "runtime_daemon",
                "host_os", "host_cpus", "host_memory", "measured_at_utc"):
        lines.append(f"- **{key}:** {environment.get(key, 'unknown')}")
    lines += ["", "## Workspace", ""]
    for key in ("path", "head", "tracked_files", "has_agent_dir", "has_project_policy",
                "has_box_dockerfile"):
        lines.append(f"- **{key}:** {workspace.get(key, 'unknown')}")
    lines += ["", "## Results", "",
              "| case | boundary | ok | n | p50 (s) | p95 (s) | min (s) | max (s) |",
              "|---|---|---|---|---|---|---|---|"]
    for case in results:
        ok = [s.seconds for s in case.samples if s.ok and s.seconds is not None]
        if ok:
            lines.append(f"| {case.name} | {case.boundary} | {len(ok)}/{len(case.samples)} | "
                         f"{len(case.samples)} | {percentile(ok, 0.5):.3f} | {percentile(ok, 0.95):.3f} | "
                         f"{min(ok):.3f} | {max(ok):.3f} |")
        else:
            lines.append(f"| {case.name} | {case.boundary} | 0/{len(case.samples)} | "
                         f"{len(case.samples)} | — | — | — | — |")
    lines += ["", "## Notes", ""]
    for case in results:
        lines.append(f"- **{case.name}** — {case.description}")
        if case.unverified:
            lines.append(f"  - unverified: {case.unverified}")
        for sample in case.samples:
            if not sample.ok:
                lines.append(f"  - failed sample: {sample.detail}")
            elif sample.extra:
                lines.append(f"  - {', '.join(f'{k}={v}' for k, v in sample.extra.items())}")
    lines += ["", "## Repeat it", "", "```",
              f"tools/lifecycle_bench.py --samples {args.samples} --cases {args.cases} \\",
              f"  --coop {redact(str(Path(args.coop).resolve()))} --repo {workspace.get('path', '<workspace>')} --out <dir>",
              "```",
              "",
              "The workspace is part of the measurement, not a detail: a project whose policy makes",
              "every run filtered pays a different start than a bare directory, and this command",
              "names the one these numbers came from.", ""]
    return "\n".join(lines)


if __name__ == "__main__":
    sys.exit(main())
