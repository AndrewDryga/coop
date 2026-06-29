#!/usr/bin/env python3
"""
Generate asciinema v2 .cast files for the coop website (site/casts/*.cast).

No third-party dependencies — just the stdlib. There are two kinds of scene:

  • A real capture (`capture_output`) runs an actual coop command under a PTY and
    records its true, colored output. Used for `coop help`, so that cast is genuine.

  • A scripted scene reconstructs coop's live output faithfully — every line, color,
    and glyph matches internal/ui/ui.go and internal/cli/streamjson.go. These cover
    the flows that need a container runtime and signed-in (paid) agents to run for
    real: the loop, forks, fusion, the fleet, doctor, check-secrets. To capture a
    real one instead, run e.g.  `asciinema rec -c "coop loop" site/casts/loop.cast`.

Usage:  python3 tools/gen_casts.py              # (re)write every cast
        python3 tools/gen_casts.py loop fork    # only the named ones
"""

import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CASTS = ROOT / "site" / "casts"
CWD = "~/code/acme-api"

# --- ANSI, matching internal/ui/ui.go ---------------------------------------
RESET = "\x1b[0m"


def _w(text, *codes):
    return "".join(f"\x1b[{n}m" for n in codes) + text + RESET


BOLD, DIM = 1, 2
RED, GREEN, YELLOW, MAGENTA, CYAN = 31, 32, 33, 35, 36
BGREEN = 92


def bold(s):
    return _w(s, BOLD)


def dim(s):
    return _w(s, DIM)


def green(s):
    return _w(s, GREEN)


def red(s):
    return _w(s, RED)


def yellow(s):
    return _w(s, YELLOW)


def magenta(s):
    return _w(s, MAGENTA)


def coop(rest):
    """A `coop:` Info line — prefix bold-cyan, the rest as given (ui.Info)."""
    return _w("coop:", BOLD, CYAN) + " " + rest


def chk(msg):
    """A doctor check line — green ✓ then plain text (ui.Check)."""
    return "  " + green("✓") + " " + msg


def model_line(agent="claude", model="claude-opus-4-8[1m]", profile="personal"):
    """streamjson.go's init line: dim labels (· using / model / profile), normal-bright values."""
    s = dim("· using ") + agent + dim(" model ") + model
    if profile:
        s += dim(" profile ") + profile
    return s


ICON_LLM = magenta("✦")  # streamjson.go: the agent's own voice
SPIN = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"]  # ui.SpinFrames


def cyan(s):
    return _w(s, CYAN)


def bar(done, total, w=10):
    """ui.ProgressBar — [ cyan-filled ░-empty ]."""
    filled = round(done / total * w) if total else 0
    return "[" + _w("█" * filled, CYAN) + "░" * (w - filled) + "]"


def badge(agent):
    """agentBadge — a 1-cell colored initial (c/x/g)."""
    return {"claude": _w("c", MAGENTA), "codex": _w("x", GREEN), "gemini": _w("g", YELLOW)}.get(agent, "?")


def fleet_row(glyph, agent, name, done, total, doing, countw=3, log=""):
    """One coop fleet watch row: glyph · badge · name · bar · count · doing · last log.
    The count is left-padded to countw (the frame-global max width) so counts line up one
    space past the bar, and there are two spaces before `doing` — matching fleet_watch.go."""
    count = f"{done}/{total}"
    line = f"{glyph} {badge(agent)} {name:<14} {bar(done, total)} {count:<{countw}}  {doing}"
    if log:
        line += "  " + dim(log)
    return line


class Cast:
    """Builds one asciinema v2 cast: a typed prompt then output, with timing."""

    def __init__(self, name, cols=88, rows=26, title=None):
        self.name, self.cols, self.rows, self.title = name, cols, rows, title
        self.t = 0.0
        self.ev = []

    def _emit(self, s):
        self.ev.append([round(self.t, 3), "o", s])

    def sleep(self, dt):
        self.t += dt
        return self

    def out(self, s, after=0.0):
        if after:
            self.sleep(after)
        self._emit(s)
        return self

    def nl(self, n=1, after=0.2):
        return self.out("\r\n" * n, after)

    def prompt(self):
        self._emit(dim(CWD) + " " + _w("❯", BGREEN) + " ")
        return self

    def type(self, cmd, cps=24):
        self.sleep(0.35)
        for ch in cmd:
            self.sleep(1.0 / cps)
            self._emit(ch)
        self.sleep(0.25)
        self._emit("\r\n")
        return self

    def command(self, cmd, think=0.6):
        self.prompt()
        self.type(cmd)
        self.sleep(think)
        return self

    def line(self, s="", after=0.32):
        return self.out(s + "\r\n", after)

    def raw(self, s, after=0.0):
        return self.out(s, after)

    def write(self):
        CASTS.mkdir(parents=True, exist_ok=True)
        header = {
            "version": 2,
            "width": self.cols,
            "height": self.rows,
            "env": {"TERM": "xterm-256color", "SHELL": "/bin/zsh"},
        }
        if self.title:
            header["title"] = self.title
        path = CASTS / f"{self.name}.cast"
        with path.open("w") as f:
            f.write(json.dumps(header) + "\n")
            for e in self.ev:
                f.write(json.dumps(e, ensure_ascii=False) + "\n")
        print(f"wrote {path.relative_to(ROOT)}  ({len(self.ev)} events, {self.t:.1f}s)")


def capture_output(argv, cwd=ROOT, cols=88, rows=44):
    """Run argv under a PTY and return its real, colored output as one string."""
    import fcntl
    import struct
    import termios
    import pty
    import subprocess

    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    env = {**os.environ, "TERM": "xterm-256color", "CLICOLOR_FORCE": "1", "COLUMNS": str(cols)}
    p = subprocess.Popen(argv, stdin=slave, stdout=slave, stderr=slave, cwd=str(cwd), env=env)
    os.close(slave)
    chunks = []
    while True:
        try:
            data = os.read(master, 65536)
        except OSError:
            break
        if not data:
            break
        chunks.append(data)
    p.wait()
    os.close(master)
    return b"".join(chunks).decode("utf-8", "replace")


# ===========================================================================
# Scenes
# ===========================================================================


def scene_loop():
    """The headline: a fresh agent per iteration drains the .agent/tasks/ queue unattended.
    These lines are from a REAL `coop loop` run (claude, two tasks, captured under a PTY), then
    abridged for length — every line is genuine coop output, paths relativized. To re-capture a
    live run wholesale: asciinema rec -c "coop loop" site/casts/loop.cast"""
    c = Cast("loop", cols=92, rows=30, title="coop loop — work the queue all night")
    c.command("coop loop")
    c.line(coop("starting unattended loop on .agent/tasks with claude — 0/2 done (Ctrl-C to stop)"), after=0.7)
    c.line(coop("iteration 1 · 0/2 done · now: Add a Header method to Token"), after=0.5)
    c.line(model_line("claude", "claude-opus-4-8[1m]", "personal"), after=0.7)
    c.line("▸ Read " + dim("AGENTS.md"), after=0.5)
    c.line("⚙ Bash " + dim("find .agent/tasks -maxdepth 2 -type d | sort"), after=0.5)
    c.line("▸ Read " + dim(".agent/tasks/00_todo/2026-…-add-a-ping-method-to-client/task.md"), after=0.5)
    c.line("▸ Read " + dim("internal/client/client.go"), after=0.6)
    c.line(ICON_LLM + " Both tasks are small. I'll work them one at a time, one commit each, starting with the ping task.", after=0.9)
    c.line("⚙ Bash " + dim("go version; go build ./... && go vet ./..."), after=0.6)
    c.line("⚙ Bash " + dim("mv .agent/tasks/00_todo/2026-06-28-add-a-ping-method-to-cli…"), after=0.6)
    c.line("✎ Edit " + dim("internal/client/client.go"), after=0.7)
    c.line("⚙ Bash " + dim("gofmt -l internal/client/client.go; go build ./... && go ve…"), after=0.8)
    c.line(ICON_LLM + " Gate green. Update state.md / log.md, then commit.", after=0.7)
    c.line("⚙ Bash " + dim('git add internal/client/client.go && git commit -q -m "clie…'), after=0.7)
    c.line("⚙ Bash " + dim("mv .agent/tasks/10_in_progress/2026-06-28-add-a-ping-method…"), after=0.6)
    c.line(ICON_LLM + " Ping task shipped. Now the header task — claim, implement, gate, commit, done.", after=0.9)
    c.line("⚙ Bash " + dim("mv .agent/tasks/00_todo/2026-06-28-add-a-header-method-to-t…"), after=0.6)
    c.line("✎ Edit " + dim("internal/auth/auth.go"), after=0.7)
    c.line("⚙ Bash " + dim("gofmt -l internal/auth/auth.go; go build ./... && go vet ./…"), after=0.8)
    c.line("⚙ Bash " + dim('git add internal/auth/auth.go && git commit -q -m "auth: ad…'), after=0.7)
    c.line("⚙ Bash " + dim("mv .agent/tasks/10_in_progress/2026-06-28-add-a-header-meth…"), after=0.6)
    c.line(ICON_LLM + " Queue drained. Both tasks shipped, one commit each, gate green, working tree clean.", after=0.9)
    c.line(dim("· 35 turns · 1m40s · $0.82"), after=0.8)
    c.line(coop("queue empty — running audit pass"), after=0.9)
    c.line(model_line("claude", "claude-opus-4-8[1m]", "personal"), after=0.6)
    c.line(ICON_LLM + " Both done tasks pass the audit — gate green, implementing commit in the log. Nothing needs reopening.", after=0.9)
    c.line(dim("· 11 turns · 49s · $0.41"), after=0.8)
    c.line(bold(green("✓ queue verified done — 2/2 in 1 iterations")), after=1.5)
    c.write()


def scene_doctor():
    """Prove the box contains the agent — plant a decoy secret, attack, verify."""
    c = Cast("doctor", rows=32, title="coop doctor — prove the isolation holds")
    c.command("coop doctor")
    c.line(bold("== coop doctor ==") + "  " + dim("(runtime: docker, image: coop-box)"), after=0.6)
    c.line()
    c.line(bold("inside the sandbox"), after=0.3)
    for m in [
        ".env is shadowed (empty in the VM)",
        ".envrc (direnv) is shadowed",
        "*.tfvars in a subdir is shadowed",
        "a private key in a subdir is shadowed",
        ".coopignore shadows a custom path",
        "secrets/ is shadowed (empty)",
        "a symlink to a secret reads empty",
        "writing the .env decoy is blocked",
        ".env.example template stays readable",
        "source files stay readable",
        "secret value appears nowhere the agent can read",
        "no coop CLI in the box (ships coop-entry only)",
        "no docker socket in the box (can't drive the host daemon)",
        "the box runs as non-root (uid 1000)",
        "all Linux capabilities dropped (CapEff=0)",
        "pids-limit enforced (4096)",
    ]:
        c.line(chk(m), after=0.14)
    c.line(after=0.2)
    c.line(bold("egress (fail-closed)"), after=0.3)
    c.line(chk("COOP_EGRESS=none cuts the box off the network (loopback only)"), after=0.14)
    c.line(after=0.2)
    c.line(bold("credential scope"), after=0.3)
    for m in [
        "the scoped agent's own credential home is mounted",
        "a peer agent's credential home is NOT mounted",
        "a second peer's credential home is NOT mounted",
        "the scoped agent's API key is in the env",
        "a peer's API key is stripped from the env",
        "a peer's alias key (bare) is stripped",
    ]:
        c.line(chk(m), after=0.14)
    c.line(after=0.2)
    c.line(bold("on the host (the clone handoff)"), after=0.3)
    for m in [
        "gitignored .env never enters a clone",
        "gitignored .envrc never enters a clone",
        "gitignored secrets/ never enters a clone",
        "gitignored deploy/ (private key) never enters a clone",
        "tracked source is present in the clone",
        "no secret value anywhere in the clone",
        "clone origin is a local path — there is nowhere to push",
    ]:
        c.line(chk(m), after=0.14)
    c.line(after=0.4)
    c.line(bold(green("✓ all 30 checks passed")) + " — the box contains the agent.", after=1.4)
    c.write()


def scene_fork():
    """Hand off work like a PR: a fork loops in the background, you read the brief + diff, land it.
    From a REAL fork run (claude added request retry with exponential backoff to the acme-api
    client — a 2-commit, 3-file change with its test). The fork path is relativized and the diff
    abridged for length; the brief, commits, files, task log, and colored diff are genuine coop
    output (re-captured with `coop fork review`)."""
    c = Cast("fork", cols=92, rows=32, title="coop fork — review and land like a PR")
    c.command("coop fork feat claude --loop -d --tasks .agent/tasks.feat")
    c.line(coop("forking acme-api → ../acme-api-forks/feat (secrets are gitignored, so they don't come along)"), after=0.6)
    c.line(coop("started fork feat (claude) in the background"), after=0.5)
    c.line(coop("  coop fork logs feat -f   ·   coop fork stop feat"), after=1.0)
    c.command("coop fork ls")
    c.line(bold("  NAME AGENT    BRANCH       STATE     TASKS    CHANGES         UPDATED"), after=0.3)
    c.line("  feat claude   feat         idle      2/2      +65 -4          1 minute ago", after=1.1)
    c.command("coop fork review feat")
    c.line("review/feat ← feat  ·  2 commit(s), +65 -4 across 3 file(s)", after=0.4)
    c.line(bold("commits:"), after=0.15)
    c.line("  52019f6 client: add request retry with exponential backoff", after=0.18)
    c.line("  5f2289d client: add Name field to Client", after=0.3)
    c.line(bold("files:"), after=0.15)
    c.line("  M\tinternal/client/client.go", after=0.12)
    c.line("  A\tinternal/client/retry.go", after=0.12)
    c.line("  A\tinternal/client/retry_test.go", after=0.3)
    c.line(bold("why (latest task log):"), after=0.15)
    c.line("  # Log — Add request retry with exponential backoff", after=0.12)
    c.line("  - Added internal/client/retry.go: backoff(n) (100ms→5s cap) + withRetry(attempts, fn).", after=0.12)
    c.line("  - Client gained a MaxRetries field (default 3, set in New).", after=0.12)
    c.line("  - Tested the backoff schedule and that withRetry stops on the first success.", after=0.12)
    c.line("  - Gate green: gofmt -l clean, go build ./..., go vet ./..., go test ./... pass.", after=0.5)
    c.line(bold("diff:"), after=0.2)
    c.line(bold("diff --git a/internal/client/client.go b/internal/client/client.go"), after=0.1)
    c.line(dim("--- a/internal/client/client.go"), after=0.05)
    c.line(dim("+++ b/internal/client/client.go"), after=0.1)
    c.line(cyan("@@ -9,14 +9,16 @@") + " // Client talks to the acme API over HTTP.", after=0.08)
    c.line(" type Client struct {", after=0.05)
    c.line(red("-\tBaseURL string"), after=0.04)
    c.line(red("-\tHTTP    *http.Client"), after=0.04)
    c.line(red("-\tTimeout time.Duration"), after=0.04)
    c.line(green("+\tName       string"), after=0.04)
    c.line(green("+\tBaseURL    string"), after=0.04)
    c.line(green("+\tHTTP       *http.Client"), after=0.04)
    c.line(green("+\tTimeout    time.Duration"), after=0.04)
    c.line(green("+\tMaxRetries int"), after=0.04)
    c.line(" }", after=0.2)
    c.line(bold("diff --git a/internal/client/retry.go b/internal/client/retry.go") + dim("   (new file, 27 lines)"), after=0.1)
    c.line(green("+func backoff(attempt int) time.Duration {  // 100ms, 200ms, 400ms, …, capped at 5s"), after=0.05)
    c.line(green("+func withRetry(attempts int, fn func() error) error {"), after=0.1)
    c.line(dim("       … and internal/client/retry_test.go (new file, 32 lines)"), after=0.8)
    c.command("coop fork merge feat --yes")
    c.line(coop("rebase review/feat onto main — 2 commit(s), +65 -4"), after=0.5)
    c.line(coop("landing feat onto main"), after=0.6)
    c.line("Successfully rebased and updated refs/heads/feat.", after=0.6)
    c.line(green("✓") + " landed feat", after=0.4)
    c.line(green("✓") + " removed fork feat", after=1.2)
    c.write()


def scene_fusion():
    """A governed council: one model leads, the others advise read-only, the lead decides.
    From a REAL `coop fusion claude -p` run — claude consulted codex + gemini, then decided
    (note the honest peer outcome: gemini concurred, codex flaked — coop reports it as-is)."""
    c = Cast("fusion", cols=96, rows=15, title="coop fusion — a council that argues before it commits")
    c.command('coop fusion claude -p "Design Client.Ping\'s retry policy — consult your peers, decide, document it"')
    c.line(coop("fusion: claude governs; peers codex + gemini consulted read-only"), after=0.8)
    c.line(coop("shadowed 2 secret path(s)"), after=0.7)
    c.line(after=0.6)
    c.line("Gate is green (gofmt clean, `go vet`/`go build`/`go test ./...` all pass).", after=1.1)
    c.line(after=0.4)
    c.line("I gave `Client.Ping` **no internal retries** and documented why on the function — a liveness probe", after=0.5)
    c.line("must report the API's immediate, unmasked state, and its callers (readiness checks, monitors, CLI", after=0.5)
    c.line("health commands) already own their own polling cadence (Gemini concurred; Codex was unavailable", after=0.5)
    c.line("after three attempts).", after=1.3)
    c.write()


def scene_fleet():
    """Run several agents at once; `coop fleet watch` is the live board (alt-screen) that animates
    each fork's progress and auto-exits with a final summary once the fleet is done. The board
    format and the 'fleet idle' exit line are real coop output (captured from a real watch); the
    three-fork scenario — each agent draining its own small queue — is a realistic run."""
    c = Cast("fleet", cols=96, rows=12, title="coop fleet — many agents, one live board")
    c.command("coop fleet up")
    for name, agent in [("api", "claude"), ("web", "gemini"), ("deps", "codex")]:
        c.line(coop(f"forking acme-api → ../acme-api-forks/{name} (secrets are gitignored, so they don't come along)"), after=0.3)
        c.line(coop(f"started fork {name} ({agent}) in the background"), after=0.25)
    c.line(green("✓") + " 3 forks detached — coop fork ls · coop fork logs -f", after=0.9)
    c.command("coop fleet watch", think=0.4)

    # api (claude, 3 tasks), web (gemini, 3 tasks), deps (codex, 2 tasks). Each frame advances the
    # board as the agents ship tasks; the spinner cycles as it repaints in place. When the last fork
    # finishes (0 running), watch auto-exits, leaves the alt-screen, and prints the final frame plus
    # the 'fleet idle' line on the normal screen.
    doing = {
        "api": ["rate-limit the client", "add request retry", "cache /health probes"],
        "web": ["fix hydration mismatch", "lazy-load the dashboard", "tighten CSP headers"],
        "deps": ["bump axios to 1.7.x", "drop the left-pad dep"],
    }
    logs = {"api": "⚙ Bash go test ./...", "web": "✎ Edit src/app.tsx", "deps": "⚙ Bash npm audit fix"}
    forks = [("claude", "api", 3), ("gemini", "web", 3), ("codex", "deps", 2)]

    def render(done, spin, final=False):
        running = sum(1 for _, n, total in forks if done[n] < total)
        head_glyph = green("✓") if running == 0 else cyan(SPIN[spin % len(SPIN)])
        rows = [bold("acme-api fleet") + f" — {running} running, 0 blocked", ""]
        for agent, n, total in forks:
            d = done[n]
            if d >= total:
                glyph, what, log = green("✓"), green("✓ done"), "✓ queue verified done — %d/%d" % (total, total)
            else:
                glyph, what, log = cyan(SPIN[spin % len(SPIN)]), doing[n][d], logs[n]
            rows.append(fleet_row(glyph, agent, n, d, total, what, countw=3, log=log))
        tot_done = sum(done.values())
        rows += ["", f"{head_glyph} {bar(tot_done, 8, 27)} {tot_done}/8 tasks · {running} running · 0 blocked"]
        return rows

    steps = [
        {"api": 1, "web": 0, "deps": 0},
        {"api": 1, "web": 1, "deps": 0},
        {"api": 2, "web": 1, "deps": 1},
        {"api": 2, "web": 2, "deps": 1},
        {"api": 3, "web": 2, "deps": 2},  # api done, deps done
        {"api": 3, "web": 3, "deps": 2},  # all done → auto-exit
    ]
    for spin, done in enumerate(steps):
        c.raw("\x1b[H\x1b[2J")  # home + clear (alt-screen repaint)
        c.raw("\r\n".join(render(done, spin)) + "\r\n")
        c.sleep(0.6)
    c.sleep(0.6)
    # Auto-exit: leave the alt-screen, then the final frame + the exit line persist on the normal screen.
    c.raw("\x1b[?25h\x1b[?1049l")
    c.line("", after=0.0)
    for row in render(steps[-1], 0):
        c.line(row, after=0.05)
    c.line("fleet idle — every fork is done, stopped, or blocked; watch exited", after=1.3)
    c.write()


def scene_secrets():
    """Secrets never enter the box — shadowed by name, scanned by content."""
    c = Cast("secrets", rows=12, title="coop check-secrets — secrets stay out of the box")
    c.command("cat .coopignore")
    c.line(dim("# repo-specific paths to hide from the agent, on top of the built-in defaults"), after=0.2)
    c.line("prod.yml" + dim("                 # basename — matched at any depth"), after=0.2)
    c.line("config/credentials.yaml" + dim("  # a slash makes it a repo-relative path"), after=0.2)
    c.line("vault/" + dim("                   # a directory — its contents are hidden whole"), after=0.9)
    c.command("coop check-secrets")
    c.line("  possible secret in config/legacy.rb:5 (high-entropy value assigned to 'api_key')", after=0.6)
    c.line(red("✗ 1 secret found in commit-candidate files (tracked + untracked; gitignored excluded) — remove them, or hide an intended file with a .coopignore entry"), after=0.7)
    c.command("echo $?")
    c.line("1", after=1.0)
    c.write()


def scene_claude():
    """One sandboxed agent, brakes off — your secrets shadowed. From a REAL `coop claude -p` run:
    print mode runs the task and prints the result (no streamed tool view — that's the loop only)."""
    c = Cast("claude", cols=92, rows=11, title="coop claude — a sandboxed agent, brakes off")
    c.command('coop claude -p "Add a Timeout field to Client (30s default), keep the gate green"')
    c.line(coop("shadowed 2 secret path(s)"), after=0.9)
    c.line(after=0.7)
    c.line("Gate is green (gofmt clean, vet/build/tests pass).", after=1.1)
    c.line(after=0.4)
    c.line("I added a `Timeout time.Duration` field to `Client` and set it to `30 * time.Second` in `New()`.", after=1.3)
    c.write()


def scene_help():
    """Real, colored `coop help`, captured under a PTY."""
    coop_bin = ROOT / "coop"
    if not coop_bin.exists():
        print("skip help: build ./coop first (make build)")
        return
    out = capture_output([str(coop_bin), "help"], cols=104)
    if "\r\n" not in out:
        out = out.replace("\n", "\r\n")
    c = Cast("help", cols=104, rows=50, title="coop help")
    c.command("coop help")
    c.raw(out, after=0.2)
    c.sleep(1.2)
    c.write()


SCENES = {
    "loop": scene_loop,
    "doctor": scene_doctor,
    "fork": scene_fork,
    "fusion": scene_fusion,
    "fleet": scene_fleet,
    "secrets": scene_secrets,
    "claude": scene_claude,
    "help": scene_help,
}


def main():
    want = sys.argv[1:] or list(SCENES)
    unknown = [w for w in want if w not in SCENES]
    if unknown:
        sys.exit(f"unknown scene(s): {', '.join(unknown)}  (have: {', '.join(SCENES)})")
    for name in want:
        SCENES[name]()


if __name__ == "__main__":
    main()
