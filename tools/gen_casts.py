#!/usr/bin/env python3
"""
Generate asciinema v2 .cast files for the coop website (site/casts/*.cast).

No third-party dependencies — just the stdlib. There are two kinds of scene:

  • A real capture (`capture_output`) runs an actual coop command under a PTY and
    records its true, colored output. Used for `coop help`, so that cast is genuine.

  • A scripted scene reconstructs coop's live output faithfully — every line, color,
    and glyph matches internal/ui/ui.go and internal/cli/streamjson.go. These cover
    the flows that need a container runtime and signed-in (paid) agents to run for
    real: the loop, forks, doctor, check-secrets. To capture a
    real one instead, run e.g.  `asciinema rec -c "coop loop" site/casts/loop.cast`.

Usage:  python3 tools/gen_casts.py              # (re)write every cast
        python3 tools/gen_casts.py loop fork    # only the named ones
"""

import json
import os
import sys
from pathlib import Path

from cast_hygiene import validate_cast

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


def model_line(agent="claude", model="claude-opus-4-8[1m]", credential="personal"):
    """streamjson.go's init line: dim labels, normal-bright agent/model/credential values."""
    s = dim("· using ") + agent + dim(" model ") + model
    if credential:
        s += dim(" credential ") + credential
    return s


ICON_LLM = magenta("✦")  # streamjson.go: the agent's own voice
SPIN = [".[  ]", ">[  ]", "[.  ]", "[ * ]", "[  .]", "[  ]>", "[  ]."]  # ui.SpinFrames


def cyan(s):
    return _w(s, CYAN)


def bar(done, total, w=10):
    """ui.ProgressBar — [ cyan-filled ░-empty ]."""
    filled = round(done / total * w) if total else 0
    return "[" + _w("█" * filled, CYAN) + "░" * (w - filled) + "]"


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
        pending = path.with_suffix(".cast.tmp")
        with pending.open("w") as f:
            f.write(json.dumps(header) + "\n")
            for e in self.ev:
                f.write(json.dumps(e, ensure_ascii=False) + "\n")
        try:
            validate_cast(pending, root=ROOT)
            os.replace(pending, path)
        finally:
            pending.unlink(missing_ok=True)
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
    env.pop("NO_COLOR", None)
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


class LoopBar:
    """Simulates internal/ui Region: a bottom-pinned status line (spinner · progress bar · done/total
    · now: <task> · elapsed) that the loop's activity scrolls above, repainted in place — what an
    interactive `coop loop` shows. scroll() funnels one activity line into the history above the bar;
    tick() animates the spinner/clock without new output; set() advances the progress."""

    def __init__(self, cast, total):
        self.c, self.total = cast, total
        self.done, self.active, self.spin, self.t, self.shown = 0, "", 0, 0.0, False

    def _line(self):
        el = "%d:%02d" % (int(self.t) // 60, int(self.t) % 60)
        prog = green(str(self.done)) + "/%d done" % self.total
        if self.active:
            prog += " · now: " + self.active
        return "%s %s %s %s" % (SPIN[self.spin % len(SPIN)], bar(self.done, self.total, 20), prog, dim(el))

    def _paint(self, history=""):
        if self.shown:
            self.c.raw("\r\x1b[J")  # Region.eraseLocked: CR + erase to end, wiping the current bar
        for ln in (history.split("\n") if history else []):
            self.c.raw("\x1b[K" + ln + "\r\n")  # scroll one line into the history above the bar
        self.c.raw("\x1b[K" + self._line())  # redraw the pinned bar; cursor stays on it
        self.shown = True

    def show(self):
        self._paint()

    def scroll(self, line, after=0.5):
        self.c.sleep(after)
        self.spin += 1
        self.t += after
        self._paint(line)

    def tick(self, n=1, after=0.13):
        for _ in range(n):
            self.c.sleep(after)
            self.spin += 1
            self.t += after
            self._paint()

    def set(self, done=None, active=None):
        if done is not None:
            self.done = done
        if active is not None:
            self.active = active

    def clear(self, after=0.4):
        self.c.sleep(after)
        if self.shown:
            self.c.raw("\r\x1b[J")
            self.shown = False


def scene_loop():
    """The headline: a fresh agent per iteration drains the .agent/tasks/ queue unattended, with the
    live bottom bar (spinner · progress · done/total · now: <task> · elapsed) pinned below the
    scrolling activity — exactly what an interactive `coop loop` shows. Three small, real tasks ship
    one commit each, then an audit pass verifies the work. Scripted to mirror ui.go's Region +
    streamjson.go; to record a live run instead: asciinema rec -c "coop loop" site/casts/loop.cast"""
    c = Cast("loop", cols=92, rows=26, title="coop loop — ship the backlog overnight")
    c.command("coop loop")
    c.line(coop("starting unattended loop on .agent/tasks with claude — 0/3 done (Ctrl-C to stop)"), after=0.5)
    lb = LoopBar(c, total=3)
    lb.set(active="Make POST /checkout idempotent")
    lb.show()
    lb.tick(1)

    lb.scroll(coop("iteration 1 · 0/3 done · now: Make POST /checkout idempotent"))
    lb.scroll(model_line("claude", "claude-opus-4-8[1m]", "personal"))
    lb.scroll(ICON_LLM + " A retried checkout double-charges — I'll key each order on the Idempotency-Key + a unique index, so a replay returns the first charge. Added a double-submit test; gate green.", after=0.9)
    lb.scroll("✎ Edit " + dim("internal/payments/checkout.go"))
    lb.scroll("⚙ Bash " + dim('git commit -q -m "payments: make checkout idempotent"'))
    lb.scroll(dim("· 14 turns · 1m08s · $0.42 · 190k input / 9.4k output"))
    lb.set(done=1, active="Cache the /health DB probe")
    lb.tick(2)

    lb.scroll(coop("iteration 2 · 1/3 done · now: Cache the /health DB probe"))
    lb.scroll(ICON_LLM + " The liveness probe COUNTs orders on every call — caching it 5s behind a singleflight drops ~99% of that load off the primary.", after=0.9)
    lb.scroll("✎ Edit " + dim("internal/health/health.go"))
    lb.scroll("⚙ Bash " + dim('git commit -q -m "health: cache the liveness probe (singleflight)"'))
    lb.scroll(dim("· 9 turns · 47s · $0.31 · 140k input / 6.2k output"))
    lb.set(done=2, active="Rate-limit the public API")
    lb.tick(2)

    lb.scroll(coop("iteration 3 · 2/3 done · now: Rate-limit the public API"))
    lb.scroll(ICON_LLM + " A per-token sliding window (Redis) returns 429 + Retry-After, opt-in per route so internal callers stay unthrottled.", after=0.9)
    lb.scroll("✎ Edit " + dim("internal/middleware/ratelimit.go"))
    lb.scroll("⚙ Bash " + dim('git commit -q -m "api: per-token rate limiting"'))
    lb.scroll(dim("· 11 turns · 58s · $0.38 · 165k input / 7.9k output"))
    lb.set(done=3, active="")
    lb.tick(2)

    lb.set(active="signoff: make-checkout-idempotent +2")
    lb.scroll(coop("queue empty — running signoff"), after=0.6)
    lb.scroll(ICON_LLM + " All three hold up — gate green, each with a commit and a regression test. Nothing to reopen.", after=0.9)
    lb.tick(2)
    lb.clear()
    # The closing digest: what shipped, cost per task, the run total, and the by-model split — work
    # ran on claude, the audit/review stages on codex — the "super-helpful ending" the loop now prints.
    c.line(bold("Shipped this run:"))
    for tid, subj, sub, cost in [
        ("make-checkout-idempotent", "payments: idempotent checkout", "payments", "$0.42"),
        ("cache-health-db-probe", "health: cache liveness probe", "health", "$0.31"),
        ("rate-limit-public-api", "api: per-token rate limiting", "middleware", "$0.38"),
    ]:
        c.line("  • %-26s %-31s %s" % (tid, subj, dim("(%s)  %s" % (sub, cost))))
    c.line("  Touched: internal/payments, internal/health, internal/middleware")
    c.line("  " + bold("Cost:") + " $1.47 · 620k in / 31k out")
    c.line("  by model: claude:claude-opus-4-8 $1.11 · codex:gpt-5.6-terra $0.36", after=1.0)
    c.line(bold(green("✓ queue verified done — 3/3 in 3 iterations")), after=1.4)
    c.write()


def scene_doctor():
    """Prove the box contains the agent — plant a decoy secret, attack, verify."""
    c = Cast("doctor", rows=22, title="coop doctor — prove the isolation holds")
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
    """Hand off work like a PR: a fork loops in the background, you read the brief + diff, then land it.
    A realistic 2-commit change — verifying Stripe webhook signatures and deduping replayed events —
    scripted to mirror `coop fork review`'s real output (brief, commits, files, task log, colored diff)."""
    c = Cast("fork", cols=92, rows=32, title="coop fork — review and land like a PR")
    c.command("coop fork hook claude --loop -d --tasks .agent/tasks.hook")
    c.line(coop("forking acme-api → ../acme-api-forks/hook (secrets are gitignored, so they don't come along)"), after=0.6)
    c.line(coop("started fork hook (claude) in the background"), after=0.5)
    c.line(coop("  coop fork logs hook -f   ·   coop fork stop hook"), after=1.0)
    c.command("coop fork ls")
    c.line(bold("  NAME AGENT    BRANCH       STATE     TASKS    CHANGES         COST     UPDATED"), after=0.3)
    c.line("  hook claude   hook         idle      2/2      +88 -6          $0.63    2 minutes ago", after=1.1)
    c.command("coop fork review hook")
    c.line("review/hook ← hook  ·  2 commit(s), +88 -6 across 3 file(s)", after=0.4)
    c.line(bold("commits:"), after=0.15)
    c.line("  9c41e2a webhook: verify Stripe signatures, reject unsigned events", after=0.18)
    c.line("  3a7f0db webhook: dedupe replayed events by id (at-least-once → exactly-once)", after=0.3)
    c.line(bold("files:"), after=0.15)
    c.line("  M\tinternal/webhook/handler.go", after=0.12)
    c.line("  A\tinternal/webhook/verify.go", after=0.12)
    c.line("  A\tinternal/webhook/verify_test.go", after=0.3)
    c.line(bold("why (latest task log):"), after=0.15)
    c.line("  # Log — Verify Stripe webhook signatures + dedupe events", after=0.12)
    c.line("  - verify.go: constant-time HMAC-SHA256 over the raw body with the signing secret; a", after=0.12)
    c.line("    missing / stale / mismatched Stripe-Signature is rejected 400 before any handler runs.", after=0.12)
    c.line("  - Dedupe by Stripe event id (seen-events table + unique index) — a redelivery is a no-op.", after=0.12)
    c.line("  - Gate green: gofmt clean, go build/vet/test ./... pass; added a replay + bad-signature test.", after=0.5)
    c.line(coop("cost: $0.63 · 210k in / 11k out"), after=0.5)
    c.line(bold("diff:"), after=0.2)
    c.line(bold("diff --git a/internal/webhook/handler.go b/internal/webhook/handler.go"), after=0.1)
    c.line(dim("--- a/internal/webhook/handler.go"), after=0.05)
    c.line(dim("+++ b/internal/webhook/handler.go"), after=0.1)
    c.line(cyan("@@ -18,6 +18,14 @@") + " func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {", after=0.08)
    c.line(" \tbody, _ := io.ReadAll(r.Body)", after=0.05)
    c.line(green('+\tif err := verifySignature(body, r.Header.Get("Stripe-Signature"), h.secret); err != nil {'), after=0.04)
    c.line(green('+\t\thttp.Error(w, "bad signature", http.StatusBadRequest)'), after=0.04)
    c.line(green("+\t\treturn"), after=0.04)
    c.line(green("+\t}"), after=0.04)
    c.line(green("+\tif h.seen(event.ID) {  // Stripe delivers at-least-once — a replay is a safe no-op"), after=0.04)
    c.line(green("+\t\tw.WriteHeader(http.StatusOK)"), after=0.04)
    c.line(green("+\t\treturn"), after=0.04)
    c.line(green("+\t}"), after=0.2)
    c.line(bold("diff --git a/internal/webhook/verify.go b/internal/webhook/verify.go") + dim("   (new file, 24 lines)"), after=0.1)
    c.line(green("+func verifySignature(body []byte, header, secret string) error {  // constant-time HMAC-SHA256"), after=0.05)
    c.line(dim("       … and internal/webhook/verify_test.go (new file, 41 lines)"), after=0.8)
    c.command("coop fork merge hook --yes")
    c.line(coop("rebase review/hook onto main — 2 commit(s), +88 -6"), after=0.4)
    c.line(coop("fork cost: $0.63 · 210k in / 11k out"), after=0.5)
    c.line(coop("landing hook onto main"), after=0.6)
    c.line("Successfully rebased and updated refs/heads/hook.", after=0.6)
    c.line(green("✓") + " landed hook", after=0.4)
    c.line(green("✓") + " removed fork hook", after=1.2)
    c.write()


def scene_secrets():
    """Secrets never enter the box — shadowed by name, scanned by content before any agent runs."""
    c = Cast("secrets", rows=12, title="coop check-secrets — secrets stay out of the box")
    c.command("cat .coopignore")
    c.line(dim("# repo-specific paths to hide from the agent, on top of the built-in defaults"), after=0.2)
    c.line("prod.env" + dim("                 # basename — matched at any depth"), after=0.2)
    c.line("config/stripe.live.json" + dim("  # a slash makes it a repo-relative path"), after=0.2)
    c.line("vault/" + dim("                   # a directory — its contents are hidden whole"), after=0.9)
    c.command("coop check-secrets")
    c.line("  possible secret in config/legacy_seed.rb:7 (high-entropy value assigned to 'STRIPE_SECRET')", after=0.6)
    c.line(red("✗ 1 secret found in commit-candidate files + coop's .agent/ state (other gitignored paths excluded) — remove them, or hide an intended file with a .coopignore entry"), after=0.7)
    c.command("echo $?")
    c.line("1", after=1.0)
    c.write()


def scene_claude():
    """One sandboxed agent, brakes off — your secrets shadowed. Scripted to mirror a real
    `coop claude -p` run: print mode does the task and prints the result (the streamed tool view is
    the loop's; -p prints the final answer)."""
    c = Cast("claude", cols=92, rows=11, title="coop claude — a sandboxed agent, brakes off")
    c.command('coop claude -p "Redact card numbers from the request logger; keep the gate green"')
    c.line(coop("shadowed 2 secret path(s)"), after=0.9)
    c.line(after=0.7)
    c.line("Gate is green (gofmt clean, vet/build/tests pass).", after=1.1)
    c.line(after=0.4)
    c.line("Card-like digit runs in logged request bodies are now masked to the last 4 (a Luhn check keeps", after=0.5)
    c.line("order ids untouched); added a test with a sample PAN asserting it never reaches the logs.", after=1.3)
    c.write()


def _coop_version(coop_bin):
    """The version string `./coop version` reports (e.g. 'coop v3.0.0'), or '' if it won't run."""
    import subprocess

    try:
        r = subprocess.run([str(coop_bin), "version"], cwd=str(ROOT),
                           capture_output=True, text=True, timeout=10)
        return (r.stdout or "").strip()
    except Exception:
        return ""


def _require_clean_coop():
    """Refuse to capture help.cast from an untagged/dirty ./coop — the recording embeds the version
    string, and a dev/+dirty binary shipped a `coop v0.0.0-...+dirty` line to the site once. A missing
    binary is fine (scene_help just skips; scripted scenes carry no version); only a present-but-dirty
    one is fatal, so `make casts` off a clean release tag just works."""
    coop_bin = ROOT / "coop"
    if not coop_bin.exists():
        return
    ver = _coop_version(coop_bin)
    if (not ver) or ("dirty" in ver) or ("v0.0.0" in ver) or ver.split()[-1] in ("dev", "(devel)"):
        sys.exit(
            f"refusing to capture help.cast from an untagged/dirty coop ({ver or 'no version'}).\n"
            "The cast records this version string, so the site would ship it. Regenerate from a\n"
            "clean release tag (e.g. `git checkout v3.0.0 && make casts`)."
        )


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
    "secrets": scene_secrets,
    "claude": scene_claude,
    "help": scene_help,
}


def main():
    want = sys.argv[1:] or list(SCENES)
    unknown = [w for w in want if w not in SCENES]
    if unknown:
        sys.exit(f"unknown scene(s): {', '.join(unknown)}  (have: {', '.join(SCENES)})")
    if "help" in want:
        _require_clean_coop()  # fail fast, before any cast is written
    for name in want:
        SCENES[name]()


if __name__ == "__main__":
    main()
