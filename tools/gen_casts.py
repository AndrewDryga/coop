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


def fleet_row(glyph, agent, name, done, total, doing, log=""):
    """One coop fleet watch row: glyph · badge · name · bar · count · doing · last log."""
    line = f"{glyph} {badge(agent)} {name:<14} {bar(done, total)} {done}/{total:<5} {doing}"
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
    """The headline: a fresh agent per iteration drains the .agent/tasks/ queue unattended."""
    c = Cast("loop", cols=90, rows=27, title="coop loop — work the queue all night")
    c.command("coop loop")
    c.line(coop("starting unattended loop on .agent/tasks with claude — 0/7 done (Ctrl-C to stop)"), after=0.7)
    c.line(coop("iteration 1 · 0/7 done · now: Add retry to the API client · " + dim("profile personal")), after=0.5)
    c.line(coop("shadowed 4 secret path(s)"), after=0.5)
    c.line(dim("· model claude-opus-4-8"), after=0.7)
    c.line(ICON_LLM + " I'll add exponential backoff to the client's request path. Reading it first.", after=0.8)
    c.line("▸ Read " + dim("internal/client.go"), after=0.6)
    c.line("✎ Edit " + dim("internal/client.go"), after=0.7)
    c.line("⚙ Bash " + dim("go test ./...") + "   " + green("ok"), after=1.0)
    c.line(dim("· 3 turns · 14s · $0.07"), after=0.6)
    c.line(coop("iteration 2 · 1/7 done · now: Cache the auth token · " + dim("profile personal")), after=0.5)
    c.line(dim("· model claude-opus-4-8"), after=0.6)
    c.line(ICON_LLM + " The token is re-fetched every call. I'll memoize it with a TTL.", after=0.8)
    c.line("✎ Edit " + dim("internal/auth.go"), after=0.7)
    c.line("⚙ Bash " + dim("go test ./...") + "   " + red("✗") + " " + dim("auth_test.go: token reused after expiry"), after=0.9)
    c.line(ICON_LLM + " Good — the test caught an expiry bug. Fixing the TTL check.", after=0.8)
    c.line("✎ Edit " + dim("internal/auth.go"), after=0.7)
    c.line("⚙ Bash " + dim("go test ./...") + "   " + green("ok"), after=1.0)
    c.line(dim("· 5 turns · 31s · $0.12"), after=0.7)
    c.line(yellow("⚠ rate limited") + dim(" (five_hour) — resets Jun 20, 11:00pm"), after=0.4)
    c.line(coop("claude profile 'personal' rate limited — switching to 'work'"), after=0.9)
    c.line(coop("iteration 3 · 2/7 done · now: Document the retry policy · " + dim("profile work")), after=0.6)
    c.line(dim("· model claude-opus-4-8"), after=0.6)
    c.line(dim("       … 4 more iterations …"), after=1.1)
    c.line(coop("queue empty — running audit pass"), after=1.2)
    c.line(bold(green("✓ queue verified done — 7/7 in 7 iterations")), after=1.5)
    c.write()


def scene_doctor():
    """Prove the box contains the agent — plant a decoy secret, attack, verify."""
    c = Cast("doctor", rows=22, title="coop doctor — prove the isolation holds")
    c.command("coop doctor")
    c.line(bold("== coop doctor ==") + "  " + dim("(runtime: docker)"), after=0.6)
    c.line()
    c.line(bold("inside the sandbox"), after=0.3)
    for m in [
        ".env is shadowed (empty in the VM)",
        "*.tfvars in a subdir is shadowed",
        ".coopignore shadows a custom path",
        "secrets/ is shadowed (empty)",
        "writing the .env decoy is blocked",
        ".env.example template stays readable",
        "source files stay readable",
        "secret value appears nowhere the agent can read",
    ]:
        c.line(chk(m), after=0.22)
    c.line(after=0.2)
    c.line(bold("on the host (the clone handoff)"), after=0.3)
    for m in [
        "gitignored .env never enters a clone",
        "gitignored secrets/ never enters a clone",
        "tracked source is present in the clone",
        "no secret value anywhere in the clone",
        "clone origin is a local path — there is nowhere to push",
    ]:
        c.line(chk(m), after=0.22)
    c.line(after=0.4)
    c.line(bold(green("✓ all 13 checks passed — the box contains the agent.")), after=1.4)
    c.write()


def scene_fork():
    """Hand off work like a PR: open a fork, list, review the diff, land it."""
    c = Cast("fork", rows=26, title="coop fork — review and land like a PR")
    c.command("coop fork payments codex --loop -d --tasks .agent/TASKS.payments.md")
    c.line(coop("forking acme-api → ../acme-api-forks/payments " + dim("(secrets stay out of the clone)")), after=0.6)
    c.line(coop("started fork payments (codex) in the background"), after=0.5)
    c.line(coop("  coop fork logs payments -f   ·   coop fork stop payments"), after=1.0)
    c.command("coop fork ls")
    c.line(bold("  NAME       AGENT   BRANCH     STATE     TASKS    CHANGES      UPDATED"), after=0.3)
    c.line("  payments   codex   payments   " + green("running") + "   2/4      +86 -12      just now", after=1.1)
    c.command("coop fork review payments")
    c.line(bold("review/payments ← payments  ·  3 commit(s), +86 -12 across 5 file(s)"), after=0.4)
    c.line(bold("commits:"), after=0.2)
    c.line(dim("  a1b2c3d  payments: verify the webhook signature against Stripe vectors"), after=0.15)
    c.line(dim("  e4f5a6b  payments: idempotency key on charge-create"), after=0.15)
    c.line(dim("  9c0d1e2  payments: dead-letter after 12 failed retries"), after=0.3)
    c.line(bold("files:"), after=0.2)
    c.line(dim("  M  internal/payments/webhook.go"), after=0.12)
    c.line(dim("  A  internal/payments/idempotency.go"), after=0.12)
    c.line(dim("  M  internal/payments/charge.go"), after=0.3)
    c.line(bold("why (latest task log):"), after=0.2)
    c.line(dim("  - signatures checked against Stripe's published test vectors"), after=0.9)
    c.command("coop fork merge payments")
    c.line(coop("rebase payments onto main — 4 commit(s), +112 -18"), after=0.5)
    c.line(coop("revalidating: make check"), after=1.1)
    c.line(coop("landing payments onto main"), after=0.5)
    c.line(bold(green("✓ landed payments")), after=0.4)
    c.line(coop("removed fork payments"), after=1.2)
    c.write()


def scene_fusion():
    """A governed council: one model leads, the others advise, the lead synthesizes."""
    c = Cast("fusion", rows=17, title="coop fusion — a council that argues before it commits")
    c.command('coop fusion claude -- -p "Design the webhook retry strategy"')
    c.line(coop("fusion: claude governs; peers codex + gemini consulted read-only"), after=0.7)
    c.line(dim("· model claude-opus-4-8"), after=0.7)
    c.line(ICON_LLM + " Mapping the failure modes before proposing a strategy.", after=0.8)
    c.line("▸ Read " + dim("internal/webhook/deliver.go"), after=0.7)
    c.line(ICON_LLM + " This is a design call — consulting the council in parallel, read-only.", after=0.8)
    c.line("⚙ Bash " + dim('codex exec -s read-only "retry strategy for webhook delivery?"'), after=0.5)
    c.line("⚙ Bash " + dim('gemini --approval-mode plan -p "retry strategy for webhook delivery?"'), after=1.1)
    c.line(dim("  codex  → exponential backoff + jitter, cap at 24h, dead-letter after 12 tries"), after=0.5)
    c.line(dim("  gemini → idempotency keys are the real fix; retries are secondary"), after=0.8)
    c.line(ICON_LLM + " Synthesis: idempotency key (gemini) + capped backoff with jitter (codex).", after=0.8)
    c.line("✎ Write " + dim("internal/webhook/idempotency.go"), after=0.6)
    c.line("✎ Edit " + dim("internal/webhook/deliver.go"), after=0.7)
    c.line("⚙ Bash " + dim("go test ./internal/webhook/...") + "   " + green("ok"), after=1.0)
    c.line(dim("· 7 turns · 48s · $0.21"), after=1.3)
    c.write()


def scene_fleet():
    """Run several models at once; `coop fleet watch` is the live board (alt-screen)."""
    c = Cast("fleet", cols=92, rows=12, title="coop fleet — many agents, one live board")
    c.command("coop fleet up")
    c.line(coop("started fork perf (codex) in the background"), after=0.35)
    c.line(coop("started fork deps (gemini) in the background"), after=0.35)
    c.line(coop("started fork docs (claude) in the background"), after=0.35)
    c.line(coop("fleet up: 3 fork(s) detached — coop fleet watch · coop fork logs -f"), after=0.9)
    c.command("coop fleet watch", think=0.4)
    # The watch renders on the alternate screen (top/htop style), repainting in place.
    # Each frame: home + clear, then the dashboard — forks make progress and finish.
    frames = [
        (0, [("perf", "codex", 1, 3, "Cache the fragment", "⚙ go test ./..."),
             ("deps", "gemini", 0, 2, "Bump axios to 1.x", "▸ Read package.json"),
             ("docs", "claude", 2, 4, "Document the fleet", "✎ Edit README.md")], 1.3),
        (3, [("perf", "codex", 2, 3, "Add backoff jitter", "✎ Edit retry.go"),
             ("deps", "gemini", 1, 2, "Fix the breakage", "⚙ npm test"),
             ("docs", "claude", 3, 4, "Document the fleet", "✦ the loop section")], 1.3),
        (6, [("perf", "codex", 2, 3, "Add backoff jitter", "⚙ go test ./..."),
             ("deps", "gemini", 2, 2, None, None),
             ("docs", "claude", 4, 4, None, None)], 1.4),
        (0, [("perf", "codex", 3, 3, None, None),
             ("deps", "gemini", 2, 2, None, None),
             ("docs", "claude", 4, 4, None, None)], 1.8),
    ]
    for spin, forks, hold in frames:
        done = sum(f[2] for f in forks)
        total = sum(f[3] for f in forks)
        running = sum(1 for f in forks if f[2] < f[3])
        rows = [bold("acme-api fleet") + f" — {running} running, 0 blocked", ""]
        for name, agent, fd, ft, doing, log in forks:
            if fd >= ft:
                rows.append(fleet_row(green("✓"), agent, name, fd, ft, green("✓ done")))
            else:
                rows.append(fleet_row(cyan(SPIN[spin % len(SPIN)]), agent, name, fd, ft, doing, log))
        gl = green("✓") if running == 0 else cyan(SPIN[spin % len(SPIN)])
        rows += ["", f"{gl} {bar(done, total, 27)} {done}/{total} tasks · {running} running · 0 blocked"]
        c.raw("\x1b[H\x1b[2J")            # home + clear (alt-screen repaint)
        c.raw("\r\n".join(rows) + "\r\n")
        c.sleep(hold)
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
    c.line("  possible secret in " + bold("config/legacy.rb:42") + dim(" (high-entropy string)"), after=0.6)
    c.line(coop("check-secrets: 1 finding(s) — remove it, or hide the file in .coopignore"), after=0.7)
    c.command("echo $?")
    c.line("1", after=1.0)
    c.write()


def scene_claude():
    """One sandboxed agent — its autonomous flags, your secrets shadowed."""
    c = Cast("claude", rows=10, title="coop claude — a sandboxed agent, brakes off")
    c.command('coop claude -- -p "Make the JSON logger redact auth headers"')
    c.line(coop("shadowed 4 secret path(s)"), after=0.6)
    c.line(dim("· model claude-opus-4-8"), after=0.7)
    c.line(ICON_LLM + " I'll redact Authorization and Cookie headers before they reach the sink.", after=0.8)
    c.line("▸ Read " + dim("internal/log/json.go"), after=0.6)
    c.line("✎ Edit " + dim("internal/log/json.go"), after=0.7)
    c.line("⚙ Bash " + dim("go test ./internal/log/...") + "   " + green("ok"), after=1.0)
    c.line(dim("· 4 turns · 22s · $0.09"), after=1.2)
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
