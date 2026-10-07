#!/usr/bin/env python3
"""Measure how long a warm box start takes in a project with Compose services (Postgres, Redis).

The boundary is the one tools/lifecycle_bench.py uses for its start cases: from the launch to the
box's own command running. The box writes a marker file into the mounted project and the host
watches for it, so the instant is the host's own. "Warm" means the images are already pulled and the
project's volumes exist.

It never removes anything itself: before each sample it runs `coop down` (volumes kept) or `coop up`
in its own project, so it is safe while other coop runs use the same Docker. Measure on an idle
machine anyway: other runs make every start slower.

Two cases, taken in turns so both see the same load, each after one throwaway sample:
  stopped   the services are stopped (`coop down`); the launch starts them
  running   the services are already up (`coop up`), as after a first launch

Usage:
    tools/services_bench.py --repo <project with approved services> --samples 7 --out <dir>

Output: <dir>/samples.json (every sample, with the load average when it started) and a summary on
stdout: per case the count, failures, median, fastest and slowest.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import signal
import statistics
import subprocess
import tempfile
import time

MARK = ".coop-services-ready"
CASES = ("stopped", "running")
TIMEOUT = 300
STOP_GRACE = 10.0  # how long a launch's session gets to leave after each signal


def coop_env(repo: str) -> dict:
    """Every coop call works on the project the bench was given, whatever COOP_REPO the shell has, and
    lets the launch start the services, whatever the project says: that start is what it times."""
    return dict(os.environ, COOP_REPO=os.path.abspath(repo), COOP_AUTO_UP="1")


def coop_step(coop: str, repo: str, *args: str, strict: bool = True) -> None:
    """Put the services in the state a case starts from. If that fails, every later sample would be
    filed under the wrong case, so the bench stops instead."""
    done = subprocess.run([coop, *args], cwd=repo, env=coop_env(repo), capture_output=True, text=True,
                          timeout=TIMEOUT, check=False)
    if strict and done.returncode != 0:
        raise SystemExit(f"services_bench: coop {' '.join(args)} exited {done.returncode}: "
                         f"{(done.stderr or done.stdout).strip()[-300:]}")


def group_alive(pgid: int) -> bool:
    try:
        os.killpg(pgid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        pass
    return True


def stop(process: subprocess.Popen) -> None:
    """End a launch and everything in its session, escalating while anything is left, and reap it.
    The launch itself leaving is not enough: a child of it that ignores TERM would outlive it."""
    for sig in (signal.SIGTERM, signal.SIGKILL):
        process.poll()  # reap an exited leader, which would otherwise keep the group alive as a zombie
        if not group_alive(process.pid):
            break
        try:
            os.killpg(process.pid, sig)
        except (ProcessLookupError, PermissionError):
            break
        deadline = time.time() + STOP_GRACE
        while time.time() < deadline:
            process.poll()
            if not group_alive(process.pid):
                break
            time.sleep(0.05)
    process.wait()


def launch(coop: str, repo: str) -> dict:
    """One box start: the seconds until the box's command wrote its marker, or None if it never did."""
    marker = os.path.join(repo, MARK)
    if os.path.exists(marker):
        os.remove(marker)
    load = os.getloadavg()[0]
    # its output goes to a file: an unread pipe would fill and stall a chatty launch before its marker
    with tempfile.TemporaryFile("w+") as spool:
        started = time.time()
        process = subprocess.Popen([coop, "run", "--", "sh", "-c", f": > ./{MARK}"], cwd=repo, env=coop_env(repo),
                                   stdout=spool, stderr=subprocess.STDOUT, text=True, start_new_session=True)
        ready = None
        try:
            while time.time() < started + TIMEOUT:
                if os.path.exists(marker):
                    ready = time.time()
                    break
                if process.poll() is not None:
                    break
                time.sleep(0.005)
            try:
                process.wait(timeout=TIMEOUT)
            except subprocess.TimeoutExpired:
                pass
        finally:
            stop(process)  # whatever happened: a launch that hangs after its marker, Ctrl-C
            if os.path.exists(marker):
                os.remove(marker)
        spool.seek(0)
        output = spool.read()
    return {"seconds": round(ready - started, 3) if ready else None, "exit": process.returncode, "load": round(load, 1),
            "tail": "" if ready else output[-300:]}


def sample(coop: str, repo: str, case: str) -> dict:
    coop_step(coop, repo, "down" if case == "stopped" else "up")
    return launch(coop, repo)


def summarize(rows: list[dict]) -> dict:
    seconds = [row["seconds"] for row in rows if row["seconds"] is not None]
    return {"n": len(seconds), "failed": len(rows) - len(seconds),
            "p50": round(statistics.median(seconds), 2) if seconds else None,
            "min": min(seconds) if seconds else None, "max": max(seconds) if seconds else None}


def main(argv: list[str] | None = None) -> dict:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--repo", required=True, help="a project whose Compose services are approved")
    parser.add_argument("--samples", type=int, default=7, help="samples per case (default 7)")
    parser.add_argument("--out", required=True, help="directory for samples.json")
    parser.add_argument("--coop", default=shutil.which("coop") or "coop", help="the coop binary to measure")
    args = parser.parse_args(argv)
    samples: dict[str, list[dict]] = {case: [] for case in CASES}
    try:
        for case in CASES:
            sample(args.coop, args.repo, case)  # warm-up, not counted
        for _ in range(args.samples):
            for case in CASES:
                samples[case].append(sample(args.coop, args.repo, case))
    finally:
        coop_step(args.coop, args.repo, "down", strict=False)  # leave the services stopped, however it ended
    summary = {case: summarize(rows) for case, rows in samples.items()}
    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "samples.json"), "w") as out:
        json.dump({"repo": args.repo, "when": time.strftime("%Y-%m-%d %H:%M %Z"), "summary": summary, "samples": samples}, out, indent=2)
    print(json.dumps(summary, indent=2))
    return summary


if __name__ == "__main__":
    main()
