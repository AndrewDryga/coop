#!/usr/bin/env python3
"""
Generate the co:op website's built parts: the homepage, site/index.html, from
tools/site/index.tpl.html, and the terminal windows the docs show, in site/docs.html.

The site is plain static HTML; this script only saves hand-typing what repeats: the icons, the
provider marks (tools/site/logos/), the terminal lines and the commands you copy. Edit the template
or the scenes here, then regenerate; never edit site/index.html by hand. site/docs.html is written
by hand, except between its <!-- gen_site: NAME --> and <!-- /gen_site --> markers, which this
script fills with the scene NAME.

Usage:  python3 tools/gen_site.py           # rewrite site/index.html and the docs' terminals
        python3 tools/gen_site.py --check   # fail if either is out of date
"""

import hashlib
import html
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SOURCE = ROOT / "tools" / "site"
OUT = ROOT / "site" / "index.html"
DOCS_PAGE = ROOT / "site" / "docs.html"
MARK = "assets/img/favicon.svg"  # co:op's mark, as the nav, footer and Zed's panel show it

OK = '<span class="ok">✓</span>'


def icon(body, cls="icon"):
    return f'<svg class="{cls}" viewBox="0 0 24 24" aria-hidden="true">{body}</svg>'




COPY_ICONS = (icon('<rect x="8.5" y="8.5" width="11" height="11" rx="2.5"/><path d="M15.5 8.5v-2a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2"/>', "icon copy-idle")
              + icon('<path d="M5 12.5l4.5 4.5L19 7.5"/>', "icon copy-done"))


def command(label, text, name):
    """A command to copy, under a short label saying what it is for, with a small Copy button named
    for what it copies (it shows a tick once it has); without JavaScript the button is hidden and
    the command is plain selectable text."""
    return (f'<div class="command"><span class="command-label">{label}</span><code data-copy-source><span class="prompt" aria-hidden="true">$ </span>{text}</code>'
            f'<button class="copy" type="button" data-copy aria-label="Copy the {name} command">{COPY_ICONS}</button>'
            '<span class="copy-status" role="status" data-copy-status></span></div>')


INSTALL_CMD = 'curl -fsSL https://coop.dryga.com/<wbr><span class="nowrap">install.sh | sh</span>'


def line(text, cls=""):
    return f'<span class="{" ".join(c for c in ("line", cls) if c)}">{text}</span>'


LOCK = icon('<rect x="5" y="10.5" width="14" height="10" rx="2.5"/><path d="M8.5 10.5V7.5a3.5 3.5 0 0 1 7 0v3"/>', "icon lock")


OUTSIDE = iter(range(4))  # the order the locks outside the sandbox shut in, top to bottom


def entry(name, kind=None, children="", probe=None):
    """One entry of the home folder; folders end in a slash. Outside the sandbox an entry dims and
    locks, and a folder that only leads to the project ("via") dims without a lock. A secret file
    shows a peek of its secret until the sandbox redacts it: inside the box it reads as empty."""
    row = f'<span class="row"><span class="name">{name}</span>'
    if kind == "out":
        row += f'{LOCK}<span class="sr-only">, out of reach</span>'
        if probe:
            # an attack reaching for this entry from the sandbox, stopped at its wall: the entry's
            # key (the stylesheet says which attacks go for it), and how many rows down that wall is
            key, down = probe
            row += f'<span class="probe" data-for="{key}" style="--down: {down}" aria-hidden="true">{CUT}</span>'
    elif kind == "secret":
        row += '<span class="secret-value" aria-hidden="true">sk-••••••</span><span class="sr-only">, empty inside the box</span>'
    row += "</span>"
    if kind == "out":
        hit = f' data-hit="{probe[0]}"' if probe else ""
        return f'<li class="out" style="--i: {next(OUTSIDE)}"{hit}>{row}{children}</li>'
    if kind in ("secret", "via", "pkg"):
        return f'<li class="{kind}">{row}{children}</li>'
    return f"<li>{row}{children}</li>"


def entries(*items):
    return '<ul role="list">' + "".join(items) + "</ul>"


MARK_SLUGS = ("claude", "openai", "gemini", "grok")


def mark(slug):
    """A provider's one-colour mark (LobeHub Icons, see tools/site/logos/SOURCE.txt), drawn from the sprite."""
    return f'<svg class="logo" viewBox="0 0 24 24" aria-hidden="true"><use href="#mark-{slug}"/></svg>'


# Each mark's path, once per page; every mark() above points at it.
SPRITE = ('<svg class="sprite" aria-hidden="true">' + "".join(
    f'<symbol id="mark-{slug}" viewBox="0 0 24 24"><path fill-rule="evenodd" d="'
    + re.search(r'<path d="([^"]+)"', (SOURCE / "logos" / f"{slug}.svg").read_text()).group(1) + '"/></symbol>'
    for slug in MARK_SLUGS) + "</svg>")


# What sits on the sandbox wall where a wire meets it: the provider's open port, an x for every
# other site, a lock for the push.
CUT = icon('<path d="M7 7l10 10M17 7 7 17"/>', "icon cut")
MARKS = {"pass": '<span class="port"></span>', "stop": CUT, "git": LOCK}
BRANCH = icon('<circle cx="7" cy="6" r="2.25"/><circle cx="7" cy="18" r="2.25"/><circle cx="17" cy="8" r="2.25"/>'
              '<path d="M7 8.25v7.5M17 10.25c0 4.5-10 2.5-10 5.5"/>', "icon")
ORIGIN = f'{BRANCH}origin'


def hop(i, label, kind, said):
    """A wire from the sandbox's floor down to something on the internet."""
    return (f'<li class="hop {kind}" style="--i: {i}"><span class="wire" aria-hidden="true">{MARKS[kind]}</span>'
            f'<span class="label">{label}</span><span class="sr-only">{said}</span></li>')


# The picture: the home folder as the agent sees it, in one card titled with the agent. The project
# sits last, so the sandbox's floor is the card's floor; below it, wires run down to the internet and
# the Git remote. The story's steps close the sandbox and cut those wires.
BUS = ('<ul class="bus" role="list">'
       + hop(0, "api.anthropic.com", "pass", ": reachable")
       + hop(1, "any other site", "stop", ": blocked")
       + hop(2, ORIGIN, "git", ", your Git remote: the agent cannot push to it") + "</ul>")
# On wide screens the remote sits off the sandbox's right wall, out of the card: a push is not a network
# rule, so it does not share the internet's wires.
REMOTE = ('<div class="remote git"><span class="wire" aria-hidden="true">' + LOCK + '</span>'
          f'<span class="label">{ORIGIN}</span><span class="sr-only">, your Git remote: the agent cannot push to it</span></div>')
WALL = ('<svg class="wall" aria-hidden="true"><rect x="0" y="0" width="100%" height="100%" rx="10" '
        'pathLength="1"/></svg>')
SANDBOX = ('<li class="zone-node"><div class="zone">' + WALL + '<span class="zone-tag">Sandbox</span>'
           '<span class="row"><span class="name">shop/</span></span>'
           + entries(entry("src/"), entry("tests/"), entry("package.json", "pkg"), entry(".env", "secret"))
           + REMOTE + "</div></li>")
CARD = ('<div class="box">'
        f'<p class="agent">{mark("claude")}Claude Code</p>'
        '<ul class="tree" role="list"><li><span class="row"><span class="name home">~</span></span>'
        + entries(entry(".ssh/", "out", probe=("ssh", 4.5)), entry(".aws/", "out", probe=("aws", 3.5)),
                  entry("Documents/", "out", probe=("docs", 2.5)),
                  entry("code/", "via", children=entries(entry("billing/", "out", probe=("billing", 0.5)), SANDBOX)))
        + "</li></ul></div>")
SCENE = CARD + BUS


# The agents co:op runs, as their marks with their names.
def provider(slug, name):
    return f"<li>{mark(slug)}{name}</li>"


PROVIDERS = ('<ul class="providers" role="list" aria-label="Agents co:op runs">' + provider("claude", "Claude Code")
             + provider("openai", "Codex") + provider("gemini", "Gemini") + provider("grok", "Grok") + "</ul>")


def NOWRAP(text):
    """Keeps a flag with its value, so a narrow screen never breaks inside --controller."""
    return f'<span class="nowrap">{text}</span>'


def snippet(*pairs):
    """Commands, each under a note saying what it does."""
    return "<pre><code>" + "".join(line(f'<span class="note"># {note}</span>') + line(f'<span class="prompt">$ </span>{command}', "cmd")
                                    for note, command in pairs) + "</code></pre>"


# Your own work in the same box (README "Services": coop up; docs/cli.md: coop shell opens in your
# project inside the box).
OWN_RUNS = snippet(("start your services", "coop up"), ("and open a shell in the same box", "coop shell"))

# Feature scenes: one small picture per feature, drawn like the story's picture. Each is complete as
# built (the state it rests in); site.js replays how it got there, once, when it first comes into view.
WARN = '<span class="warn">⚠</span>'
TICK = icon('<path d="M5 12.5l4.5 4.5L19 7.5"/>', "icon tick")
FOLDER = icon('<path d="M3.5 7.5a2 2 0 0 1 2-2h3.8l2 2h7.2a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z"/>')
DATABASE = icon('<ellipse cx="12" cy="6.5" rx="7" ry="2.5"/><path d="M5 6.5v11c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5v-11M5 12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5"/>')
LAYERS = icon('<path d="M12 4.5l8 4-8 4-8-4z"/><path d="M4 12.5l8 4 8-4M4 16.5l8 4 8-4"/>')


def term(*lines):
    return '<pre><code>' + "".join(lines) + "</code></pre>"


def at_prompt(command, where="~/code/shop"):
    return line(f'<span class="path">{where}</span> <span class="prompt">$ </span>{command}', "cmdline")


def window_bar(title):
    """A terminal window's title bar: the three window buttons, and what is running in it, centred."""
    return f'<p class="window-bar" aria-hidden="true"><span class="window-dots"><i></i><i></i><i></i></span>{title}</p>'


DOT = '<span class="dim"> · </span>'


def state_bar(done, doing, blocked, total, width):
    """internal/ui/live.go ProgressBarStates: done, in-progress and blocked cells, the rest ░; a
    task in progress or blocked always keeps at least one cell."""
    cells = lambda n: int(n / total * width + 0.5) if n > 0 else 0
    b_min = 1 if blocked > 0 and width > 0 else 0
    a_min = 1 if doing > 0 and width - b_min > 0 else 0
    b = min(max(cells(blocked), b_min), width - a_min)
    a = min(max(cells(doing), a_min), width - b)
    d = cells(done)
    if d + a + b > width:
        d = width - a - b
    return (f'[<span class="s-done">{"█" * d}</span><span class="s-doing">{"█" * a}</span>'
            f'<span class="s-blocked">{"█" * b}</span>{"░" * (width - d - a - b)}]')


# `coop tasks watch` (internal/tasks/watch.go): a board redrawn in place. The bar and the counts
# (todo · in_progress · blocked · done), then the queue: in progress first, with the Corner Run
# spinner, its subtasks and the lease; then todo (○); then blocked (⚑). Done tasks are the count.
def watch_frame(done, doing, todo, blocked):
    total = done + len(doing) + len(todo) + len(blocked)
    counts = DOT.join((f'<span class="s-todo">{len(todo)} todo</span>', f'<span class="s-doing">{len(doing)} in_progress</span>',
                       f'<span class="s-blocked">{len(blocked)} blocked</span>', f'<span class="s-done">{done} done</span>'))
    rows = [line(state_bar(done, len(doing), len(blocked), total, 22) + "  " + counts), line("")]
    rows += [line(f'  <span class="s-doing spin">◰</span> {title}<span class="dim"> ({sub}) · busy claude</span>') for title, sub in doing]
    rows += [line(f'  <span class="s-todo">○</span> {title}') for title in todo]
    rows += [line(f'  <span class="s-blocked">⚑</span> {title}') for title in blocked]
    return "".join(rows)


CHECKOUT, HEALTH, LIMITS, SESSIONS = ("Make POST /checkout idempotent", "Cache the /health DB probe",
                                      "Rate-limit the public API", "Choose how long sessions last")
LATER = ("Backfill tests for the parser", "Document the config file")  # the README's own examples
# The board as the loop works: subtasks tick, a task lands in done, the next one starts. It rests
# on the last frame, which is also what shows without the script.
TESTS, DOCS = LATER
WATCH_FRAMES = [watch_frame(2, [(CHECKOUT, "1/3")], [HEALTH, LIMITS, TESTS, DOCS], [SESSIONS]),
                watch_frame(2, [(CHECKOUT, "2/3")], [HEALTH, LIMITS, TESTS, DOCS], [SESSIONS]),
                watch_frame(2, [(CHECKOUT, "3/3")], [HEALTH, LIMITS, TESTS, DOCS], [SESSIONS]),
                watch_frame(3, [(HEALTH, "0/2")], [LIMITS, TESTS, DOCS], [SESSIONS]),
                watch_frame(3, [(HEALTH, "1/2")], [LIMITS, TESTS, DOCS], [SESSIONS]),
                watch_frame(3, [(HEALTH, "2/2")], [LIMITS, TESTS, DOCS], [SESSIONS]),
                watch_frame(4, [(LIMITS, "0/2")], [TESTS, DOCS], [SESSIONS]),
                watch_frame(4, [(LIMITS, "1/2")], [TESTS, DOCS], [SESSIONS]),
                watch_frame(4, [(LIMITS, "2/2")], [TESTS, DOCS], [SESSIONS]),
                watch_frame(5, [(TESTS, "0/1")], [DOCS], [SESSIONS])]
QUEUE = ('<figure class="window scene-watch" data-scene="watch" aria-label="coop tasks watch: the queue\'s progress, '
         'the task in progress with its subtasks, the ones up next, and one waiting for your decision">'
         + window_bar("coop tasks watch") + "<pre><code>" + WATCH_FRAMES[4] + "</code></pre>"
         + "".join(f"<template>{frame}</template>" for frame in WATCH_FRAMES) + "</figure>")


# `coop loop frontier` as it really scrolls (internal/loop/report.go task headers and final review,
# iteration.go "Starting", streamjson.go activity: ✦ the agent's note, ✎ edits, ⚙ commands). site.js
# plays it a beat at a time, slow enough to read; a beat can move the live bar (done, active, what it
# is on). The window shows the newest rows, as a terminal does; without the script it shows how the
# run ended.
AGENT = "claude:claude-opus-5-5/xhigh@work"
RULE, THIN = "━" * 64, "─" * 64


def task_header(n, attempt, title, queue):
    return ["", ("rule", RULE), ("dim", f" Task {n} - Attempt {attempt}"), "", f" {title}", "",
            f" Agent  {AGENT}", f" Queue  {queue}", ("rule", RULE), "", ("strong", f"Starting {AGENT}")]


def review(round_, tasks_):
    return ["", ("rule", THIN), f" Final review · Round {round_} of 3", "", f" Agent  {AGENT}", f" Tasks  {tasks_} completed", ("rule", THIN)]


LOOP_BEATS = [
    (None, [("cmdline", f'<span class="path">~/code/shop</span> <span class="prompt">$ </span>coop loop frontier')]),
    ((0, 1, CHECKOUT), task_header(1, 1, CHECKOUT, "0 completed · 1 active · 2 pending · 0 blocked")),
    (None, ["✦ A retried checkout charges twice. I’ll key each order on its Idempotency-Key, so a replay returns the first charge."]),
    (None, ["✎ Edit src/payments/checkout.ts"]),
    (None, ["⚙ Bash npm test"]),
    ((1, 1, HEALTH), ["", f"{OK} Task completed: {CHECKOUT}"]),
    (None, task_header(2, 1, HEALTH, "1 completed · 1 active · 1 pending · 0 blocked")),
    (None, ["✦ The probe counts orders on every call. Caching it for five seconds takes that load off the database."]),
    (None, ["✎ Edit src/health.ts"]),
    ((2, 1, LIMITS), ["", f"{OK} Task completed: {HEALTH}"]),
    (None, task_header(3, 1, LIMITS, "2 completed · 1 active · 0 pending · 0 blocked")),
    (None, ["✎ Edit src/middleware/rate-limit.ts"]),
    (None, ["⚙ Bash npm test"]),
    ((3, 0, "signoff: make-checkout-idempotent +2"), ["", f"{OK} Task completed: {LIMITS}"]),
    (None, review(1, 3)),
    ((2, 1, LIMITS), ["", ("rule", THIN), ' Final review · <span class="warn">1 task needs more work</span>', "", f"  {LIMITS}", "",
                      ("dim", " Continuing the task queue"), ("rule", THIN)]),
    (None, task_header(3, 2, LIMITS, "2 completed · 1 active · 0 pending · 0 blocked")),
    (None, ["✦ Internal callers were throttled too. The limit now applies only to public routes."]),
    (None, ["✎ Edit src/middleware/rate-limit.ts"]),
    ((3, 0, "signoff: rate-limit-the-public-api"), ["", f"{OK} Task completed: {LIMITS}"]),
    (None, review(2, 1)),
    (None, ["", f'{OK} All tasks passed <span class="nowrap">final review · 3/3 done</span>']),
]


def loop_line(beat, entry, bar=None):
    cls, text = entry if isinstance(entry, tuple) else ("", entry)
    data = f' data-beat="{beat}"' + (f' data-bar="{bar[0]},{bar[1]},{bar[2]}"' if bar else "")
    return f'<span class="{" ".join(c for c in ("line", cls) if c)}"{data}>{text}</span>'


LOOP = ('<figure class="window scene-replay scene-loop" data-scene="loop" aria-label="coop loop working through three tasks: '
        'a fresh agent on each, a final review that reopens one, and the run passing review">'
        + window_bar("coop loop frontier") + "<pre><code>"
        + "".join(loop_line(beat, entry, bar if i == 0 else None) for beat, (bar, entries) in enumerate(LOOP_BEATS)
                  for i, entry in enumerate(entries))
        + '<span class="line live-bar" aria-hidden="true"></span></code></pre></figure>')


# Forks, open to merge (internal/cli/fork_cmd.go, internal/forkctl/{supervise,ls,status,review,merge}.go):
# a fork working in the background, listed ready to merge, its review summary, and the merge. The
# merge's delete prompt that follows is left off the end.
def at_shop(command):
    return ("cmdline", f'<span class="path">~/code/shop</span> <span class="prompt">$ </span>{command}')


FORK_BEATS = [
    (None, [at_shop("coop fork api codex --loop -d")]),
    (None, ["Creating fork: api", "", "  /Users/you/code/shop-forks/api"]),
    (None, [f"{OK} Started fork api in the background", "", "  Agent: codex", "  Logs:  coop fork logs api --follow",
            "  Stop:  coop fork stop api"]),
    (None, ["", at_shop("coop fork ls")]),
    (None, ["Forks", "", '  <span class="strong">api</span> · ready to merge · updated 4 minutes ago', "    Agent: codex",
            "    Branch: api", "    Tasks: 1 ready to merge", "    Changes: +88 −6", "", "Review a fork: coop fork review &lt;name&gt;"]),
    (None, ["", at_shop("coop fork review api --stat")]),
    (None, ["Changes in fork api", "  Into: main", "  2 commits · 3 files · +88 −6", "", "Commits",
            "  9c41e2a webhook: verify Stripe signatures", "  3a7f0db webhook: dedupe replayed events by id", "", "Files",
            "  code:", "    M  src/webhook/handler.ts  +40 -6", "    A  src/webhook/verify.ts  +24 -0", "  tests:",
            "    A  tests/webhook.test.ts  +24 -0", "", "Project checks will run when you merge."]),
    (None, ["", at_shop("coop fork merge api")]),
    (None, ["Merge fork api into main", "  2 commits · +88 −6", "", "Merge these commits? [Y/n] y"]),
    (None, ["Rebasing onto main", "Running project checks", "  Running: npm test"]),
    (None, ["", f"{OK} Merged fork api into main"]),
]
FORK = ('<figure class="window scene-replay scene-fork" data-scene="fork" aria-label="coop fork: a fork works in the '
        'background, shows as ready to merge, is reviewed like a pull request and merged after the project checks">'
        + window_bar("coop fork") + "<pre><code>"
        + "".join(loop_line(beat, entry) for beat, (bar, entries) in enumerate(FORK_BEATS) for entry in entries)
        + "</code></pre></figure>")


# The docs' terminals, from the CLI's approved transcripts (internal/cli/testdata/approved/).
# `coop doctor` (18a-doctor-all-passed.txt): each section of checks arrives as a beat of the replay.
DOCTOR_SECTIONS = [
    ("Protecting secrets", [".env is hidden", ".envrc is hidden", "Terraform variable files are hidden", "Private keys are hidden",
                            "Custom .coopignore paths are hidden", "Secret directories are empty", "Links cannot reveal hidden secrets",
                            "Hidden secret files cannot be written", "Environment templates stay readable", "Source files stay readable",
                            "The test secret cannot be read"]),
    ("Host access and privileges", ["The host Coop CLI is absent", "The host Docker socket is absent", "The box runs as a non-root user",
                                    "Linux capabilities are removed", "The process limit is enforced (4096)"]),
    ("Offline access", ["Offline mode leaves only the loopback interface"]),
    ("Task access", ["The task channel exposes only its 8 task tools", "Calls outside the task tools are refused",
                     "Changes to a task held by another process are refused", "The assigned task can be updated"]),
    ("Credentials and settings", ["Claude's credential home is available", "Codex's credential home is hidden from Claude",
                                  "Gemini's credential home is hidden from Claude", "Claude's saved login takes priority over its environment key",
                                  "Codex's environment key is hidden from Claude", "Gemini's environment key is hidden from Claude",
                                  "The box can write its settings directory"]),
    ("Fork handoff", [".env is absent from the clone", ".envrc is absent from the clone", "Secret directories are absent from the clone",
                      "Private keys are absent from the clone", "Tracked source files are present in the clone",
                      "The test secret is absent from the clone", "The clone's origin is a local path"]),
]
DOCTOR_BEATS = ([(None, [at_shop("coop doctor")]), (None, ["Checking the Coop box on the Docker runtime"])]
                + [(None, ["", ("strong", title)] + [f"  {OK} {check}" for check in checks]) for title, checks in DOCTOR_SECTIONS]
                + [(None, ["", f"{OK} All {sum(len(checks) for _, checks in DOCTOR_SECTIONS)} checks passed"])])
DOCTOR = ('<figure class="window scene-replay scene-doctor" data-scene="doctor" aria-label="coop doctor tries to break out of its own '
          'box: secrets, host access, the network, tasks, credentials and the fork handoff, and all 35 checks pass">'
          + window_bar("coop doctor") + "<pre><code>"
          + "".join(loop_line(beat, entry) for beat, (bar, entries) in enumerate(DOCTOR_BEATS) for entry in entries)
          + "</code></pre></figure>")

# `coop check-secrets` with one finding (approved/19e-finding-and-ignored-blind-spot.txt, its first block).
FAIL = '<span class="fail">✗</span>'
CHECK_SECRETS = ('<figure class="window" aria-label="coop check-secrets finds an OpenAI API key in a source file, says how to '
                 'remove it, and how to mark a false positive">'
                 + window_bar("coop check-secrets")
                 + term(at_prompt("coop check-secrets"), line(f"{FAIL} 1 possible secret found"), line(""),
                        line("  config/client.go:12"), line("    OpenAI API key"), line(""),
                        line("Remove real secrets from files you intend to commit."),
                        line("If this is a false positive, add this entry to .coopsecretsignore"),
                        line("and replace &lt;reason&gt; with an explanation:"),
                        line(""), line('  <span class="dim"># config/client.go — OpenAI API key</span>'),
                        line("  fp-v1:b2758b3a796f81888b6f896f9c420ee855608fdcb828dfc2b83f736768171a6c "
                             '<span class="dim"># &lt;reason&gt;</span>', "hash"))
                 + "</figure>")

# What `coop claude` prints before the agent starts, with networking filtered (internal/box/launch_sections.go).
CLAUDE_RUN = ('<figure class="window" aria-label="coop claude hides two secret paths, connects your account, allows only '
              'Anthropic\'s endpoints, then starts Claude Code">'
              + window_bar("coop claude")
              + term(at_prompt("coop claude"), line("Protecting secrets", "strong"), line(f"  {OK} 2 secret paths hidden from the box"),
                     line(""), line("Connecting account", "strong"), line(f"  {OK} Claude (default) · Signed in"),
                     line(""), line("Configuring network access", "strong"), line(f"  {OK} Anthropic endpoints allowed"),
                     line(f"  {OK} Everything else blocked"), line(""), line("Starting Claude Code", "strong"))
              + "</figure>")
DOCS_SCENES = {"doctor": DOCTOR, "check-secrets": CHECK_SECRETS, "claude": CLAUDE_RUN, "loop": LOOP, "fork": FORK}


# Where each terminal's lines come from, so a CLI change can't leave the site showing output the CLI
# no longer prints (terminal_drift, run by --check and tools/test_site_content.py). A transcript is
# approved output, and every line of the block must be one of its lines. Otherwise the block cites
# the Go files that print it and the shape of each line it shows, {} standing for a value; the
# shape's literal parts, its own text between the values unless listed, must still be in one of
# those files. A line that is only an example value, such as a task title, is listed as one.
def shape(text, *parts):
    return text, parts or tuple(part for part in text.split("{}") if part.strip())


TERMINAL_SOURCES = {
    "doctor": {"transcript": "internal/cli/testdata/approved/18a-doctor-all-passed.txt"},
    "check-secrets": {"transcript": "internal/cli/testdata/approved/19e-finding-and-ignored-blind-spot.txt"},
    "claude": {"go": ["internal/box/launch_sections.go"], "examples": ["Claude Code"], "shapes": [
        shape("Protecting secrets"), shape("{} hidden from the box", "hidden from the box", '"secret path"'),
        shape("Connecting account"), shape("{} · Signed in", '"Signed in"'), shape("Configuring network access"),
        shape("{} endpoints allowed"), shape("Everything else blocked"), shape("Starting {}", '"Starting "')]},
    "loop": {"go": ["internal/loop/report.go", "internal/loop/iteration.go", "internal/loop/banners.go",
                    "internal/loop/streamjson.go", "internal/loop/streamjson_providers.go"],
             "examples": [CHECKOUT, HEALTH, LIMITS], "shapes": [
        shape("━", 'strings.Repeat("━"'), shape("─", 'strings.Repeat("─"'), shape("Task {} - Attempt {}"),
        shape("Agent  {}"), shape("Queue  {}"), shape("Starting {}", '"Starting "'), shape("✦ {}", 'llmIcon = "✦"'),
        shape("✎ Edit {}", '"✎"'), shape("⚙ Bash {}", '"⚙", "Bash"'), shape("Task completed: {}"),
        shape("Final review · Round {} of {}"), shape("Tasks  {} completed", 'label: "Tasks"', '"%d completed"'),
        shape("Final review · {} needs more work", '"Final review', '"needs"', '" more work"'),
        shape("Continuing the task queue"), shape("All tasks passed final review · {}/{} done")]},
    "fork": {"go": ["internal/cli/fork_cmd.go", "internal/forkctl/supervise.go", "internal/forkctl/ls.go",
                    "internal/forkctl/status.go", "internal/forkctl/review.go", "internal/forkctl/dossier.go",
                    "internal/forkctl/merge.go", "internal/ui/confirm.go"],
             "examples": ["/Users/you/code/shop-forks/api", "9c41e2a webhook: verify Stripe signatures",
                          "3a7f0db webhook: dedupe replayed events by id"], "shapes": [
        shape("Creating fork: {}", '"Creating fork"', '"%s: %s"'), shape("Started fork {} in the background"),
        shape("Agent: {}"), shape("Logs:  coop fork logs {} --follow"), shape("Stop:  coop fork stop {}"),
        shape("Forks", 'listing("Forks")'), shape("{} · ready to merge · updated {}", '" · updated "', '"ready to merge"'),
        shape("Branch: {}"), shape("Tasks: {} ready to merge", '"    Tasks: %s"', '"ready to merge"'), shape("Changes: {}"),
        shape("Review a fork: coop fork review <name>"), shape("Changes in fork {}"), shape("Into: {}"),
        shape("{} · {} · +{} −{}", '"  %s · %s · +%d −%d"'), shape("Commits", 'say("Commits")'), shape("Files", 'say("Files")'),
        shape("code:", 'dossierCode   = "code"'), shape("tests:", 'dossierTests  = "tests"'),
        shape("{}  {}  +{} -{}", '" + f.path', '"  +%d -%d"'), shape("Project checks will run when you merge."),
        shape("Merge fork {} into {}"), shape("{} · +{} −{}", '"  %s · +%d −%d"'),
        shape("Merge these commits? [Y/n] {}", '"Merge these commits?"', '"Y/n"'), shape("Rebasing onto {}"),
        shape("Running project checks"), shape("Running: {}"), shape("Merged fork {} into {}")]},
    "worker": {"go": ["internal/cli/session_connect.go"], "examples": [], "shapes": [
        shape("Local session service ready"), shape("Connecting to {}…"), shape("Worker identity ready: {}")]},
    "watch": {"go": ["internal/tasks/watch.go", "internal/tasks/lease.go", "internal/taskstate/taskstate.go", "internal/ui/live.go"],
              "examples": [], "shapes": [
        shape("[{}]  {} todo · {} in_progress · {} blocked · {} done", '"%d %s"', '"10_in_progress"', '"░"'),
        shape("◰ {} ({}/{}) · busy {}", '"◰"', '" (%d/%d)"', '"busy "'), shape("○ {}", '"○"'), shape("⚑ {}", '"⚑"')]},
}

STATUS_MARKS = ("✓ ", "✗ ", "⚠ ")  # ui.OK/Fail/Warn print these before the message the source holds


def terminal_blocks(page):
    """Each terminal on a page by name (its data-scene, else the gen_site region holding it), with
    the lines a visitor sees: replay frames included, the reader's typed commands and blank or
    ellipsis-only lines left out."""
    blocks = {}
    for figure in re.finditer(r'<figure class="window[^"]*"[^>]*>.*?</figure>', page, re.S):
        named = re.search(r'data-scene="([^"]+)"', figure.group(0)[:300])
        region = re.findall(r"<!-- gen_site: ([\w-]+) -->", page[:figure.start()])
        name = named.group(1) if named else (region[-1] if region else None)
        lines = []
        for cls, inner in re.findall(r'<span class="line([^"]*)"[^>]*>(.*?)(?=<span class="line|</code>|</template>)', figure.group(0), re.S):
            text = html.unescape(re.sub(r"<[^>]+>", "", inner)).rstrip()
            if "cmdline" not in cls and text.strip() not in ("", "…", "...") and text not in lines:
                lines.append(text)
        if name and lines:
            blocks[name] = lines
    return blocks


def terminal_drift(read=lambda rel: (ROOT / rel).read_text()):
    """Every problem with the terminals' sources, as 'page, block: what' lines; empty when none."""
    problems = []
    for page_path in (OUT, DOCS_PAGE):
        page = page_path.name
        for name, lines in terminal_blocks(read(page_path.relative_to(ROOT))).items():
            source = TERMINAL_SOURCES.get(name)
            if source is None:
                problems.append(f"{page}, block {name}: no source is listed in TERMINAL_SOURCES")
                continue
            if "transcript" in source:
                known = {line.rstrip() for line in read(source["transcript"]).splitlines()}
                problems += [f"{page}, block {name}: line {line!r} is not in {source['transcript']}"
                             for line in lines if line not in known]
                continue
            texts = [read(path) for path in source["go"]]
            for text, parts in source["shapes"]:
                problems += [f"{page}, block {name}: shape {text!r} needs {part!r}, which none of {', '.join(source['go'])} has"
                             for part in parts if not any(part in go for go in texts)]
            patterns = [re.compile("^" + ".+?".join(map(re.escape, text.split("{}"))) + "$") for text, _ in source["shapes"]]
            for line in lines:
                said = line.strip()
                for mark in STATUS_MARKS:
                    said = said.removeprefix(mark)
                if set(said) in ({"━"}, {"─"}):
                    said = said[0]
                if said not in source["examples"] and not any(p.match(said) for p in patterns):
                    problems.append(f"{page}, block {name}: line {line!r} matches no shape listed for it")
    return sorted(set(problems))


# Account rotation: each target with its usage meter, drawn as `coop usage` draws it (10 cells:
# cyan, yellow from 80%, red at 100%). The work account is spent; the loop runs on the home account.
def meter(used):
    full = round(used / 10)
    tone = "full" if used >= 100 else "high" if used >= 80 else "ok"
    return (f'<span class="meter" aria-hidden="true"><span class="meter-used {tone}">{"█" * full}</span>{"░" * (10 - full)}</span>'
            f'<span class="pct">{used}%</span>')


def slot(slug, target, used, state=""):
    said = {"spent": "limit reached", "live": "running"}.get(state, "")
    cls = f"slot {state}".strip()
    return (f'<li class="{cls}" data-used="{used}">{mark(slug)}<span class="target">{target}</span>'
            f'<span class="usage">{meter(used)}</span><span class="state">{said}</span></li>')


ROTATE = ('<figure class="scene-card rotate" data-scene="rotate" aria-label="The loop\'s targets with their usage: '
          'the work account reached its limit, so the same model runs on the home account; Codex on two accounts '
          'waits its turn"><ol class="slots" role="list">'
          + slot("claude", "claude:opus@work", 100, "spent") + slot("claude", "claude:opus@home", 40, "live")
          + slot("openai", "codex:gpt-6-astra@work", 0) + slot("openai", "codex:gpt-6-astra@home", 0)
          + "</ol></figure>")


# The starter team `coop presets init` writes (internal/preset/template.go): a lead, two read-only
# advisers and one editor ("2 advisers, 1 editor", as `coop presets` lists it).
def seat(slug, role, model, kind="", cls=""):
    k = f'<span class="kind">{kind}</span>' if kind else ""
    return (f'<{"div" if cls else "li"} class="seat{(" " + cls) if cls else ""}" data-role="{role}">'
            f'<span class="seat-head">{mark(slug)}<span class="role">{role}</span></span>'
            f'<span class="model">{model}</span>{k}</{"div" if cls else "li"}>')


TEAM = ('<figure class="scene-card team" data-scene="team" aria-label="The starter preset: a lead model and '
        'the three it hands work to, two advisers and an editor, from three providers">'
        '<p class="card-title"><span class="prompt">$ </span>coop frontier</p><div class="org">'
        + seat("claude", "lead", "Opus 5.5", cls="lead") + '<ul class="org-roles" role="list">'
        + seat("claude", "thinker", "Fable 5.1", "adviser") + seat("openai", "critic", "GPT-6 Astra", "adviser")
        + seat("gemini", "fast", "Gemini 3.8 Flash", "editor") + "</ul></div></figure>")


# Project services: the sandbox and, beside it, the containers `coop up` starts, all inside the
# private network they share; with filtered networking that network has no direct internet route
# (docs/networking.md, "A service: grant").
def service(name, glyph):
    return f'<li class="svc" data-svc="{name}">{glyph}<span class="svc-name">{name}</span><span class="svc-dot"></span></li>'


SERVICES = ('<figure class="scene-card services" data-scene="services" aria-label="coop up starts a database and Redis '
            'beside the sandbox, each in its own container, on a private network with no direct internet">'
            '<p class="card-title"><span class="prompt">$ </span>coop up</p><div class="svc-map">'
            '<span class="net-tag">Private network</span>'
            '<div class="svc-box">' + WALL + '<span class="zone-tag">Sandbox</span>'
            f'<p class="svc-agent">{mark("claude")}Claude Code</p><p class="svc-project">shop/</p></div>'
            '<ul class="svc-list" role="list">' + service("db", DATABASE) + service("redis", LAYERS) + "</ul></div></figure>")

# Integrations: Zed's agent panel driving the box over ACP, with co:op's preset and account
# selectors (README "Drive it from Zed"), and a worker connecting to your own platform, with what
# `coop sessions connect` prints (internal/cli/session_connect.go).
INTEGRATIONS = ('<div class="integrations">'
                '<figure class="scene-card zed" data-scene="zed" aria-label="Zed\'s agent panel with co:op as the agent: '
                'you pick the preset and account there, and the agent works in the box">'
                + window_bar("Zed") +
                '<div class="zed-body"><ul class="zed-files" role="list" aria-hidden="true"><li>shop/</li><li>src/</li><li>tests/</li></ul>'
                f'<div class="zed-agent"><p class="zed-head"><img src="{MARK}" width="18" height="18" alt="">co:op</p>'
                '<p class="zed-pills"><span>frontier</span><span>claude@work</span></p>'
                '<p class="zed-ask">Add a /health endpoint</p>'
                '<p class="zed-busy" aria-hidden="true"><i></i><i></i><i></i></p></div></div></figure>'
                '<figure class="window scene-worker" data-scene="worker" aria-label="What coop sessions connect prints as a worker joins your platform">'
                + window_bar("coop sessions connect") + term(at_prompt("coop sessions connect " + NOWRAP("--controller") + " https://jobs.example.com", "~"),
                       line(f"  {OK} Local session service ready"),
                       line("Connecting to https://jobs.example.com…"),
                       line(f"  {OK} Worker identity ready: " + NOWRAP("w-7c1e")))
                + "</figure></div>")


def proof_count():
    """Commits on the published main branch that carry a Coop-Task trailer, minus 'none'."""
    log = subprocess.run(["git", "-C", str(ROOT), "log", "origin/main", "--format=%B"],
                         capture_output=True, text=True, check=True).stdout
    lines = [l for l in log.splitlines() if l.startswith("Coop-Task:")]
    return f"{sum(1 for l in lines if l.split(':', 1)[1].strip() != 'none'):,}"


def published_count():
    """The count the committed page shows, so --check compares the page and not the git history."""
    found = re.search(r'<strong>([\d,]+)</strong> <span>commits to co:op', OUT.read_text())
    if not found:
        sys.exit("site/index.html has no commit count; regenerate it: python3 tools/gen_site.py")
    return found.group(1)


def typographic(page):
    """Curly apostrophes in the prose (don’t, team’s); code, commands, scripts and attribute values
    keep straight ones, since people copy those."""
    out, inside = [], 0
    for part in re.split(r"(<[^>]+>)", page):
        tag = re.match(r"</?(pre|code|script|style)\b", part)
        if tag:
            inside += -1 if part.startswith("</") else 1
        out.append(part if part.startswith("<") or inside else re.sub(r"(?<=\w)'(?=\w)", "’", part))
    return "".join(out)


def render(count):
    blocks = {
        "sprite": SPRITE,
        "scene": SCENE,
        "own_runs": OWN_RUNS,
        "queue": QUEUE,
        "loop": LOOP,
        "fork": FORK,
        "rotate": ROTATE,
        "team": TEAM,
        "services": SERVICES,
        "integrations": INTEGRATIONS,
        "check": icon('<path d="M5 12.5l4.5 4.5L19 7.5"/>', "icon"),
        "providers": PROVIDERS,
        "proof_count": count,
        "install": command("Install", INSTALL_CMD, "install"),
        "cmd_login": command("Sign in", '<span data-cmd="login">coop login claude</span>', "sign-in"),
        "cmd_init": command("Set up", 'cd <span class="nowrap">your-project</span> &amp;&amp; coop init', "project setup"),
        "cmd_run": command("Start", '<span data-cmd="run">coop claude</span>', "start"),
        "mark": MARK,
    }
    page = (SOURCE / "index.tpl.html").read_text()
    for name, value in blocks.items():
        page = page.replace("{{" + name + "}}", value)
    left = re.findall(r"\{\{\w+\}\}", page)
    assert not left, left
    return stamp_assets(typographic(page))


ASSETS = ("assets/css/site.css", "assets/js/site.js", "assets/js/analytics.js")


def asset_version():
    """A short hash of the stylesheet and scripts, put on their links (?v=) so a browser holding an
    older copy fetches the new one the moment either file changes."""
    digest = hashlib.sha256()
    for name in ASSETS:
        digest.update((ROOT / "site" / name).read_bytes())
    return digest.hexdigest()[:10]


def stamp_assets(page):
    """The page with every CSS and JS link carrying the current asset version."""
    return re.sub(r'(assets/(?:css|js)/[\w.-]+\.(?:css|js))\?v=[\w-]+', rf"\1?v={asset_version()}", page)


def fill_docs(docs):
    """The docs with each marked region holding its scene, and their asset links stamped."""
    def scene(found):
        return f"{found.group(1)}{DOCS_SCENES[found.group(2)]}{found.group(3)}"
    filled, regions = re.subn(r"(<!-- gen_site: ([\w-]+) -->)(?s:.*?)(<!-- /gen_site -->)", scene, docs)
    assert regions == len(DOCS_SCENES), f"site/docs.html marks {regions} scenes, expected {len(DOCS_SCENES)}"
    return stamp_assets(filled)


def main():
    if sys.argv[1:] == ["--check"]:
        stale = [path.relative_to(ROOT) for path, page in ((OUT, render(published_count())), (DOCS_PAGE, fill_docs(DOCS_PAGE.read_text())))
                 if path.read_text() != page]
        if stale:
            sys.exit(f"out of date: {', '.join(map(str, stale))}; edit tools/site/ or tools/gen_site.py and run python3 tools/gen_site.py")
        if drift := terminal_drift():
            sys.exit("terminal output no longer matches its source:\n  " + "\n  ".join(drift))
        return
    if sys.argv[1:]:
        sys.exit(__doc__)
    count = proof_count()
    OUT.write_text(render(count))
    DOCS_PAGE.write_text(fill_docs(DOCS_PAGE.read_text()))
    print(f"wrote {OUT.relative_to(ROOT)} ({count} task commits) and the terminals in {DOCS_PAGE.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
