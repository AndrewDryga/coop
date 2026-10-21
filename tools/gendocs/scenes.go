package main

// The pieces the site's generated pages repeat: icons, provider marks, the commands you copy, the
// pictures and the terminal scenes. Each is drawn in the state it rests in, so a page reads whole
// without JavaScript; site.js and orchestrator.js replay how it got there.

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"slices"
	"strconv"
	"strings"
)

const (
	okMark   = `<span class="ok">✓</span>`
	failMark = `<span class="fail">✗</span>`
	dot      = `<span class="dim"> · </span>`
)

func icon(cls, body string) string {
	return `<svg class="` + cls + `" viewBox="0 0 24 24" aria-hidden="true">` + body + `</svg>`
}

var (
	copyIcons = icon("icon copy-idle", `<rect x="8.5" y="8.5" width="11" height="11" rx="2.5"/><path d="M15.5 8.5v-2a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2"/>`) +
		icon("icon copy-done", `<path d="M5 12.5l4.5 4.5L19 7.5"/>`)
	checkIcon = icon("icon", `<path d="M5 12.5l4.5 4.5L19 7.5"/>`)
	lock      = icon("icon lock", `<rect x="5" y="10.5" width="14" height="10" rx="2.5"/><path d="M8.5 10.5V7.5a3.5 3.5 0 0 1 7 0v3"/>`)
	cut       = icon("icon cut", `<path d="M7 7l10 10M17 7 7 17"/>`)
	branch    = icon("icon", `<circle cx="7" cy="6" r="2.25"/><circle cx="7" cy="18" r="2.25"/><circle cx="17" cy="8" r="2.25"/>`+
		`<path d="M7 8.25v7.5M17 10.25c0 4.5-10 2.5-10 5.5"/>`)
	database = icon("icon", `<ellipse cx="12" cy="6.5" rx="7" ry="2.5"/><path d="M5 6.5v11c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5v-11M5 12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5"/>`)
	layers   = icon("icon", `<path d="M12 4.5l8 4-8 4-8-4z"/><path d="M4 12.5l8 4 8-4M4 16.5l8 4 8-4"/>`)
)

// command is a command to copy, under a short label saying what it is for, with a small Copy button
// named for what it copies (it shows a tick once it has); without JavaScript the button is hidden
// and the command is plain selectable text.
func command(label, text, name string) string {
	kind := ""
	if name == "install" {
		kind = ` data-copy-kind="install"`
	}
	return `<div class="command"><span class="command-label">` + label + `</span><code data-copy-source` + kind + `>` +
		`<span class="prompt" aria-hidden="true">$ </span>` + text + `</code>` +
		`<button class="copy" type="button" data-copy aria-label="Copy the ` + name + ` command">` + copyIcons + `</button>` +
		`<span class="copy-status" role="status" data-copy-status></span></div>`
}

// The start commands every page offers; the agent picker in site.js rewrites the data-cmd parts.
var (
	installCommand = command("Install", `curl -fsSL https://coop.dryga.com/<wbr><span class="nowrap">install.sh | sh</span>`, "install")
	loginCommand   = command("Sign in", `<span data-cmd="login">coop login claude</span>`, "sign-in")
	initCommand    = command("Set up", `cd <span class="nowrap">your-project</span> &amp;&amp; coop init`, "project setup")
)

func lineClass(cls string) string {
	if cls == "" {
		return "line"
	}
	return "line " + cls
}

func line(cls, text string) string {
	return `<span class="` + lineClass(cls) + `">` + text + `</span>`
}

// mark is a provider's one-colour mark (LobeHub Icons, see tools/site/logos/SOURCE.txt), drawn from
// the page's sprite.
func mark(slug string) string {
	return `<svg class="logo" viewBox="0 0 24 24" aria-hidden="true"><use href="#mark-` + slug + `"/></svg>`
}

func entries(items ...string) string {
	return `<ul role="list">` + strings.Join(items, "") + "</ul>"
}

func entryRow(name, extra string) string {
	return `<span class="row"><span class="name">` + name + `</span>` + extra + "</span>"
}

// entry is one entry of the home folder; folders end in a slash. kind is "", "pkg", "via" (a
// folder that only leads to the project: dimmed without a lock) or "secret" (a peek of its secret
// shows until the sandbox redacts it: inside the box it reads as empty).
func entry(name, kind string, children ...string) string {
	extra := ""
	if kind == "secret" {
		extra = `<span class="secret-value" aria-hidden="true">sk-••••••</span><span class="sr-only">, empty inside the box</span>`
	}
	open := "<li>"
	if kind != "" {
		open = `<li class="` + kind + `">`
	}
	return open + entryRow(name, extra) + strings.Join(children, "") + "</li>"
}

// outside is an entry outside the sandbox: dimmed and locked, with the attack reaching for it from
// the sandbox stopped at its wall. key says which attacks go for it (the stylesheet knows), down how
// many rows down that wall is, and i the order the locks shut in, top to bottom.
func outside(i int, name, key, down string) string {
	return `<li class="out" style="--i: ` + strconv.Itoa(i) + `" data-hit="` + key + `">` + entryRow(name, lock+
		`<span class="sr-only">, out of reach</span><span class="probe" data-for="`+key+`" style="--down: `+down+`" aria-hidden="true">`+cut+`</span>`) +
		"</li>"
}

// What sits on the sandbox wall where a wire meets it: the provider's open port, an x for every
// other site, a lock for the push.
var wireMarks = map[string]string{"pass": `<span class="port"></span>`, "stop": cut, "git": lock}

var origin = branch + "origin"

// hop is a wire from the sandbox's floor down to something on the internet.
func hop(i int, label, kind, said string) string {
	return `<li class="hop ` + kind + `" style="--i: ` + strconv.Itoa(i) + `"><span class="wire" aria-hidden="true">` + wireMarks[kind] + `</span>` +
		`<span class="label">` + label + `</span><span class="sr-only">` + said + `</span></li>`
}

// The picture: the home folder as the agent sees it, in one card titled with the agent. The project
// sits last, so the sandbox's floor is the card's floor; below it, wires run down to the internet and
// the Git remote. The story's steps close the sandbox and cut those wires.
var (
	bus = `<ul class="bus" role="list">` + hop(0, "api.anthropic.com", "pass", ": reachable") +
		hop(1, "any other site", "stop", ": blocked") + hop(2, origin, "git", ", your Git remote: the agent cannot push to it") + "</ul>"
	// On wide screens the remote sits off the sandbox's right wall, out of the card: a push is not a
	// network rule, so it does not share the internet's wires.
	remote = `<div class="remote git"><span class="wire" aria-hidden="true">` + lock + `</span>` +
		`<span class="label">` + origin + `</span><span class="sr-only">, your Git remote: the agent cannot push to it</span></div>`
	wall    = `<svg class="wall" aria-hidden="true"><rect x="0" y="0" width="100%" height="100%" rx="10" pathLength="1"/></svg>`
	sandbox = `<li class="zone-node"><div class="zone">` + wall + `<span class="zone-tag">Sandbox</span>` +
		`<span class="row"><span class="name">shop/</span></span>` +
		entries(entry("src/", ""), entry("tests/", ""), entry("package.json", "pkg"), entry(".env", "secret")) + remote + "</div></li>"
	card = `<div class="box"><p class="agent">` + mark("claude") + `Claude Code</p>` +
		`<ul class="tree" role="list"><li><span class="row"><span class="name home">~</span></span>` +
		entries(outside(0, ".ssh/", "ssh", "4.5"), outside(1, ".aws/", "aws", "3.5"), outside(2, "Documents/", "docs", "2.5"),
			entry("code/", "via", entries(outside(3, "billing/", "billing", "0.5"), sandbox))) +
		"</li></ul></div>"
	scene = card + bus
)

// The agents co:op runs, as their marks with their names.
func provider(slug, name string) string {
	return "<li>" + mark(slug) + name + "</li>"
}

var providers = `<ul class="providers" role="list" aria-label="Agents co:op runs">` + provider("claude", "Claude Code") +
	provider("openai", "Codex") + provider("gemini", "Gemini") + provider("grok", "Grok") + "</ul>"

// Your own work in the same box (docs/box.md "Project services": coop up; docs/cli.md: coop shell
// opens in your project inside the box): each command under a note saying what it does.
var ownRuns = "<pre><code>" +
	line("", `<span class="note"># start your services</span>`) + line("cmd", `<span class="prompt">$ </span>coop up`) +
	line("", `<span class="note"># and open a shell in the same box</span>`) + line("cmd", `<span class="prompt">$ </span>coop shell`) +
	"</code></pre>"

// windowBar is a terminal window's title bar: the three window buttons, and what is running in it,
// centred.
func windowBar(title string) string {
	return `<p class="window-bar" aria-hidden="true"><span class="window-dots"><i></i><i></i><i></i></span>` + title + "</p>"
}

// stateBar is internal/ui/live.go ProgressBarStates: done, in-progress and blocked cells, the rest
// ░; a task in progress or blocked always keeps at least one cell.
func stateBar(done, doing, blocked, total, width int) string {
	cells := func(n int) int {
		if n <= 0 {
			return 0
		}
		return int(float64(n)/float64(total)*float64(width) + 0.5)
	}
	bMin, aMin := 0, 0
	if blocked > 0 && width > 0 {
		bMin = 1
	}
	if doing > 0 && width-bMin > 0 {
		aMin = 1
	}
	b := min(max(cells(blocked), bMin), width-aMin)
	a := min(max(cells(doing), aMin), width-b)
	d := cells(done)
	if d+a+b > width {
		d = width - a - b
	}
	return `[<span class="s-done">` + strings.Repeat("█", d) + `</span><span class="s-doing">` + strings.Repeat("█", a) + `</span>` +
		`<span class="s-blocked">` + strings.Repeat("█", b) + `</span>` + strings.Repeat("░", width-d-a-b) + "]"
}

// Example task titles.
const (
	taskCheckout = "Make POST /checkout idempotent"
	taskHealth   = "Cache the /health DB probe"
	taskLimits   = "Rate-limit the public API"
	taskSessions = "Choose how long sessions last"
	taskTests    = "Backfill tests for the parser"
	taskDocs     = "Document the config file"
)

// watchFrame is one frame of `coop tasks watch` (internal/tasks/watch.go), a board redrawn in place:
// the bar and the counts (todo · in_progress · blocked · done), then the queue: the task in progress
// first, with the Corner Run spinner, its subtasks and the lease; then todo (○); then blocked (⚑).
// Done tasks are the count.
func watchFrame(done int, doing, subtasks string, todo, blocked []string) string {
	counts := strings.Join([]string{
		`<span class="s-todo">` + strconv.Itoa(len(todo)) + ` todo</span>`, `<span class="s-doing">1 in_progress</span>`,
		`<span class="s-blocked">` + strconv.Itoa(len(blocked)) + ` blocked</span>`, `<span class="s-done">` + strconv.Itoa(done) + ` done</span>`,
	}, dot)
	frame := line("", stateBar(done, 1, len(blocked), done+1+len(todo)+len(blocked), 22)+"  "+counts) + line("", "") +
		line("", `  <span class="s-doing spin">◰</span> `+doing+`<span class="dim"> (`+subtasks+`) · busy claude</span>`)
	for _, title := range todo {
		frame += line("", `  <span class="s-todo">○</span> `+title)
	}
	for _, title := range blocked {
		frame += line("", `  <span class="s-blocked">⚑</span> `+title)
	}
	return frame
}

// queueScene is the board as the loop works: subtasks tick, a task lands in done, the next one
// starts. It rests on the fifth frame, which is also what shows without the script.
var queueScene = func() string {
	blocked := []string{taskSessions}
	frames := []string{
		watchFrame(2, taskCheckout, "1/3", []string{taskHealth, taskLimits, taskTests, taskDocs}, blocked),
		watchFrame(2, taskCheckout, "2/3", []string{taskHealth, taskLimits, taskTests, taskDocs}, blocked),
		watchFrame(2, taskCheckout, "3/3", []string{taskHealth, taskLimits, taskTests, taskDocs}, blocked),
		watchFrame(3, taskHealth, "0/2", []string{taskLimits, taskTests, taskDocs}, blocked),
		watchFrame(3, taskHealth, "1/2", []string{taskLimits, taskTests, taskDocs}, blocked),
		watchFrame(3, taskHealth, "2/2", []string{taskLimits, taskTests, taskDocs}, blocked),
		watchFrame(4, taskLimits, "0/2", []string{taskTests, taskDocs}, blocked),
		watchFrame(4, taskLimits, "1/2", []string{taskTests, taskDocs}, blocked),
		watchFrame(4, taskLimits, "2/2", []string{taskTests, taskDocs}, blocked),
		watchFrame(5, taskTests, "0/1", []string{taskDocs}, blocked),
	}
	figure := `<figure class="window scene-watch" data-scene="watch" aria-label="coop tasks watch: the queue's progress, ` +
		`the task in progress with its subtasks, the ones up next, and one waiting for your decision">` +
		windowBar("coop tasks watch") + "<pre><code>" + frames[4] + "</code></pre>"
	for _, frame := range frames {
		figure += "<template>" + frame + "</template>"
	}
	return figure + "</figure>"
}()

// termLine is one line of a terminal scene, with its style class ("" for none).
type termLine struct{ cls, text string }

var blank = termLine{}

func plain(texts ...string) []termLine {
	lines := make([]termLine, len(texts))
	for i, text := range texts {
		lines[i] = termLine{text: text}
	}
	return lines
}

// atShop is a command typed at the example project's prompt.
func atShop(command string) termLine {
	return termLine{"cmdline", `<span class="path">~/code/shop</span> <span class="prompt">$ </span>` + command}
}

// term is a terminal drawn whole, not replayed.
func term(lines ...termLine) string {
	out := "<pre><code>"
	for _, l := range lines {
		out += line(l.cls, l.text)
	}
	return out + "</code></pre>"
}

// liveBar is where a beat moves the loop's live bar: tasks done, tasks active, what it is on.
type liveBar struct {
	done, active int
	on           string
}

// beat is what a replay adds at once: its lines, and the live bar they move (nil leaves it).
type beat struct {
	bar   *liveBar
	lines []termLine
}

// replay draws a replayed terminal's lines, each with its beat; a beat's first line carries the bar.
func replay(beats []beat) string {
	var out strings.Builder
	for i, b := range beats {
		for j, l := range b.lines {
			data := ` data-beat="` + strconv.Itoa(i) + `"`
			if b.bar != nil && j == 0 {
				data += fmt.Sprintf(` data-bar="%d,%d,%s"`, b.bar.done, b.bar.active, b.bar.on)
			}
			out.WriteString(`<span class="` + lineClass(l.cls) + `"` + data + ">" + l.text + "</span>")
		}
	}
	return out.String()
}

// `coop loop frontier` as it really scrolls (internal/loop/report.go task headers and final review,
// iteration.go "Starting", streamjson.go activity: ✦ the agent's note, ✎ edits, ⚙ commands). site.js
// plays it a beat at a time, slow enough to read; a beat can move the live bar (done, active, what it
// is on). The window shows the newest rows, as a terminal does; without the script it shows how the
// run ended.
const loopAgent = "claude:claude-opus-5-5/xhigh@work"

var taskRule, reviewRule = strings.Repeat("━", 64), strings.Repeat("─", 64)

func taskHeader(n, attempt int, title, queue string) []termLine {
	return []termLine{blank, {"rule", taskRule}, {"dim", fmt.Sprintf(" Task %d - Attempt %d", n, attempt)}, blank, {"", " " + title}, blank,
		{"", " Agent  " + loopAgent}, {"", " Queue  " + queue}, {"rule", taskRule}, blank, {"strong", "Starting " + loopAgent}}
}

func review(round, tasks int) []termLine {
	return []termLine{blank, {"rule", reviewRule}, {"", fmt.Sprintf(" Final review · Round %d of 3", round)}, blank,
		{"", " Agent  " + loopAgent}, {"", fmt.Sprintf(" Tasks  %d completed", tasks)}, {"rule", reviewRule}}
}

var loopScene = `<figure class="window scene-replay scene-loop" data-scene="loop" aria-label="coop loop working through three tasks: ` +
	`a fresh agent on each, a final review that reopens one, and the run passing review">` + windowBar("coop loop frontier") + "<pre><code>" +
	replay([]beat{
		{nil, []termLine{atShop("coop loop frontier")}},
		{&liveBar{0, 1, taskCheckout}, taskHeader(1, 1, taskCheckout, "0 completed · 1 active · 2 pending · 0 blocked")},
		{nil, plain("✦ A retried checkout charges twice. I’ll key each order on its Idempotency-Key, so a replay returns the first charge.")},
		{nil, plain("✎ Edit src/payments/checkout.ts")},
		{nil, plain("⚙ Bash npm test")},
		{&liveBar{1, 1, taskHealth}, plain("", okMark+" Task completed: "+taskCheckout)},
		{nil, taskHeader(2, 1, taskHealth, "1 completed · 1 active · 1 pending · 0 blocked")},
		{nil, plain("✦ The probe counts orders on every call. Caching it for five seconds takes that load off the database.")},
		{nil, plain("✎ Edit src/health.ts")},
		{&liveBar{2, 1, taskLimits}, plain("", okMark+" Task completed: "+taskHealth)},
		{nil, taskHeader(3, 1, taskLimits, "2 completed · 1 active · 0 pending · 0 blocked")},
		{nil, plain("✎ Edit src/middleware/rate-limit.ts")},
		{nil, plain("⚙ Bash npm test")},
		{&liveBar{3, 0, "signoff: make-checkout-idempotent +2"}, plain("", okMark+" Task completed: "+taskLimits)},
		{nil, review(1, 3)},
		{&liveBar{2, 1, taskLimits}, []termLine{blank, {"rule", reviewRule}, {"", ` Final review · <span class="warn">1 task needs more work</span>`},
			blank, {"", "  " + taskLimits}, blank, {"dim", " Continuing the task queue"}, {"rule", reviewRule}}},
		{nil, taskHeader(3, 2, taskLimits, "2 completed · 1 active · 0 pending · 0 blocked")},
		{nil, plain("✦ Internal callers were throttled too. The limit now applies only to public routes.")},
		{nil, plain("✎ Edit src/middleware/rate-limit.ts")},
		{&liveBar{3, 0, "signoff: rate-limit-the-public-api"}, plain("", okMark+" Task completed: "+taskLimits)},
		{nil, review(2, 1)},
		{nil, plain("", okMark+` All tasks passed <span class="nowrap">final review · 3/3 done</span>`)},
	}) + `<span class="line live-bar" aria-hidden="true"></span></code></pre></figure>`

// Forks, open to merge (internal/cli/fork_cmd.go, internal/forkctl/{supervise,ls,status,review,merge}.go):
// a fork working in the background, listed ready to merge, its review summary, and the merge. The
// merge's delete prompt that follows is left off the end.
var forkScene = `<figure class="window scene-replay scene-fork" data-scene="fork" aria-label="coop fork: a fork works in the ` +
	`background, shows as ready to merge, is reviewed like a pull request and merged after the project checks">` +
	windowBar("coop fork") + "<pre><code>" +
	replay([]beat{
		{nil, []termLine{atShop("coop fork api codex --loop -d")}},
		{nil, plain("Creating fork: api", "", "  /Users/you/code/shop-forks/api")},
		{nil, plain(okMark+" Started fork api in the background", "", "  Agent: codex", "  Logs:  coop fork logs api --follow",
			"  Stop:  coop fork stop api")},
		{nil, []termLine{blank, atShop("coop fork ls")}},
		{nil, plain("Forks", "", `  <span class="strong">api</span> · ready to merge · updated 4 minutes ago`, "    Agent: codex",
			"    Branch: api", "    Tasks: 1 ready to merge", "    Changes: +88 −6", "", "Review a fork: coop fork review &lt;name&gt;")},
		{nil, []termLine{blank, atShop("coop fork review api --stat")}},
		{nil, plain("Changes in fork api", "  Into: main", "  2 commits · 3 files · +88 −6", "", "Commits",
			"  9c41e2a webhook: verify Stripe signatures", "  3a7f0db webhook: dedupe replayed events by id", "", "Files",
			"  code:", "    M  src/webhook/handler.ts  +40 -6", "    A  src/webhook/verify.ts  +24 -0", "  tests:",
			"    A  tests/webhook.test.ts  +24 -0", "", "Project checks will run when you merge.")},
		{nil, []termLine{blank, atShop("coop fork merge api")}},
		{nil, plain("Merge fork api into main", "  2 commits · +88 −6", "", "Merge these commits? [Y/n] y")},
		{nil, plain("Rebasing onto main", "Running project checks", "  Running: npm test")},
		{nil, plain("", okMark+" Merged fork api into main")},
	}) + "</code></pre></figure>"

// The docs' terminals, from the CLI's approved transcripts (internal/cli/testdata/approved/).
// `coop doctor` (18a-doctor-all-passed.txt): each section of checks arrives as a beat of the replay.
var doctorSections = []struct {
	title  string
	checks []string
}{
	{"Protecting secrets", []string{".env is hidden", ".envrc is hidden", "Terraform variable files are hidden", "Private keys are hidden",
		"Custom .coopignore paths are hidden", "Secret directories are empty", "Links cannot reveal hidden secrets",
		"Hidden secret files cannot be written", "Environment templates stay readable", "Source files stay readable",
		"The test secret cannot be read"}},
	{"Host access and privileges", []string{"The host Coop CLI is absent", "The host Docker socket is absent", "The box runs as a non-root user",
		"Linux capabilities are removed", "The process limit is enforced (4096)"}},
	{"Offline access", []string{"Offline mode leaves only the loopback interface"}},
	{"Task access", []string{"The task channel exposes only its 8 task tools", "Calls outside the task tools are refused",
		"Changes to a task held by another process are refused", "The assigned task can be updated"}},
	{"Credentials and settings", []string{"Claude's native home is available", "Host account grants stay outside Claude's home",
		"Codex's native home is hidden from Claude", "Gemini's native home is hidden from Claude",
		"Grok's native home is hidden from Claude", "Claude's saved login takes priority over its environment key",
		"Codex's environment key is hidden from Claude", "Gemini's environment key is hidden from Claude",
		"The box can write its settings directory"}},
	{"Fork handoff", []string{".env is absent from the clone", ".envrc is absent from the clone", "Secret directories are absent from the clone",
		"Private keys are absent from the clone", "Tracked source files are present in the clone",
		"The test secret is absent from the clone", "The clone's origin is a local path"}},
}

var doctorScene = func() string {
	beats := []beat{{nil, []termLine{atShop("coop doctor")}}, {nil, plain("Checking the Coop box on the Docker runtime")}}
	checks := 0
	for _, section := range doctorSections {
		lines := []termLine{blank, {"strong", section.title}}
		for _, check := range section.checks {
			lines = append(lines, termLine{"", "  " + okMark + " " + check})
		}
		beats = append(beats, beat{nil, lines})
		checks += len(section.checks)
	}
	beats = append(beats, beat{nil, plain("", fmt.Sprintf("%s All %d checks passed", okMark, checks))})
	return `<figure class="window scene-replay scene-doctor" data-scene="doctor" aria-label="coop doctor tries to break out of its own ` +
		`box: secrets, host access, the network, tasks, credentials and the fork handoff, and all 37 checks pass">` +
		windowBar("coop doctor") + "<pre><code>" + replay(beats) + "</code></pre></figure>"
}()

// `coop check-secrets` with one finding (approved/19e-finding-and-ignored-blind-spot.txt, its first block).
var checkSecretsScene = `<figure class="window" aria-label="coop check-secrets finds an OpenAI API key in a source file, says how to ` +
	`remove it, and how to mark a false positive">` + windowBar("coop check-secrets") +
	term(atShop("coop check-secrets"), termLine{"", failMark + " 1 possible secret found"}, blank,
		termLine{"", "  config/client.go:12"}, termLine{"", "    OpenAI API key"}, blank,
		termLine{"", "Remove real secrets from files you intend to commit."},
		termLine{"", "If this is a false positive, add this entry to .coopsecretsignore"},
		termLine{"", "and replace &lt;reason&gt; with an explanation:"},
		blank, termLine{"", `  <span class="dim"># config/client.go — OpenAI API key</span>`},
		termLine{"hash", `  fp-v1:b2758b3a796f81888b6f896f9c420ee855608fdcb828dfc2b83f736768171a6c <span class="dim"># &lt;reason&gt;</span>`}) +
	"</figure>"

// What `coop claude` prints before the agent starts, with networking filtered (internal/box/launch_sections.go).
var claudeRunScene = `<figure class="window" aria-label="coop claude hides two secret paths, connects your account, allows only ` +
	`Anthropic's endpoints, then starts Claude Code">` + windowBar("coop claude") +
	term(atShop("coop claude"), termLine{"strong", "Protecting secrets"}, termLine{"", "  " + okMark + " 2 secret paths hidden from the box"},
		blank, termLine{"strong", "Connecting account"}, termLine{"", "  " + okMark + " Claude (default) · Signed in"},
		blank, termLine{"strong", "Configuring network access"}, termLine{"", "  " + okMark + " Anthropic endpoints allowed"},
		termLine{"", "  " + okMark + " Everything else blocked"}, blank, termLine{"strong", "Starting Claude Code"}) +
	"</figure>"

// docsScenes are the terminals site/docs.html marks by name.
var docsScenes = map[string]string{"doctor": doctorScene, "check-secrets": checkSecretsScene, "claude": claudeRunScene,
	"loop": loopScene, "fork": forkScene}

// meter is a target's usage as `coop usage` draws it: 10 cells, cyan, yellow from 80%, red at 100%.
func meter(used int) string {
	full := int(math.RoundToEven(float64(used) / 10))
	tone := "ok"
	if used >= 100 {
		tone = "full"
	} else if used >= 80 {
		tone = "high"
	}
	return `<span class="meter" aria-hidden="true"><span class="meter-used ` + tone + `">` + strings.Repeat("█", full) + "</span>" +
		strings.Repeat("░", 10-full) + `</span><span class="pct">` + strconv.Itoa(used) + "%</span>"
}

func slot(slug, target string, used int, state string) string {
	said := map[string]string{"spent": "limit reached", "live": "running"}[state]
	return `<li class="` + strings.TrimSpace("slot "+state) + `" data-used="` + strconv.Itoa(used) + `">` + mark(slug) +
		`<span class="target">` + target + `</span><span class="usage">` + meter(used) + `</span><span class="state">` + said + "</span></li>"
}

// Account rotation: each target with its usage meter. The work account is spent; the loop runs on
// the home account.
var rotate = `<figure class="scene-card rotate" data-scene="rotate" aria-label="The loop's targets with their usage: ` +
	`the work account reached its limit, so the same model runs on the home account; Codex on two accounts ` +
	`waits its turn"><ol class="slots" role="list">` +
	slot("claude", "claude:opus@work", 100, "spent") + slot("claude", "claude:opus@home", 40, "live") +
	slot("openai", "codex:gpt-6-astra@work", 0, "") + slot("openai", "codex:gpt-6-astra@home", 0, "") +
	"</ol></figure>"

// seat is one role of a preset; the lead (cls "lead") heads the team instead of sitting in its list.
func seat(slug, role, model, kind, cls string) string {
	tag, class := "li", "seat"
	if cls != "" {
		tag, class = "div", "seat "+cls
	}
	if kind != "" {
		kind = `<span class="kind">` + kind + "</span>"
	}
	return "<" + tag + ` class="` + class + `" data-role="` + role + `">` +
		`<span class="seat-head">` + mark(slug) + `<span class="role">` + role + "</span></span>" +
		`<span class="model">` + model + "</span>" + kind + "</" + tag + ">"
}

// The starter team `coop presets init` writes (internal/preset/template.go): a lead, two read-only
// advisers and one editor ("2 advisers, 1 editor", as `coop presets` lists it).
var team = `<figure class="scene-card team" data-scene="team" aria-label="The starter preset: a lead model and ` +
	`the three it hands work to, two advisers and an editor, from three providers">` +
	`<p class="card-title"><span class="prompt">$ </span>coop frontier</p><div class="org">` +
	seat("claude", "lead", "Opus 5.5", "", "lead") + `<ul class="org-roles" role="list">` +
	seat("claude", "thinker", "Fable 5.1", "adviser", "") + seat("openai", "critic", "GPT-6 Astra", "adviser", "") +
	seat("gemini", "fast", "Gemini 3.8 Flash", "editor", "") + "</ul></div></figure>"

func service(name, glyph string) string {
	return `<li class="svc" data-svc="` + name + `">` + glyph + `<span class="svc-name">` + name + `</span><span class="svc-dot"></span></li>`
}

// Project services: the sandbox and, beside it, the containers `coop up` starts, all inside the
// private network they share; with filtered networking that network has no direct internet route
// (docs/networking.md, "A service: grant").
var services = `<figure class="scene-card services" data-scene="services" aria-label="coop up starts a database and Redis ` +
	`beside the sandbox, each in its own container, on a private network with no direct internet">` +
	`<p class="card-title"><span class="prompt">$ </span>coop up</p><div class="svc-map">` +
	`<span class="net-tag">Private network</span>` +
	`<div class="svc-box">` + wall + `<span class="zone-tag">Sandbox</span>` +
	`<p class="svc-agent">` + mark("claude") + `Claude Code</p><p class="svc-project">shop/</p></div>` +
	`<ul class="svc-list" role="list">` + service("db", database) + service("redis", layers) + "</ul></div></figure>"

// Integrations: a real Zed window running co:op (docs/integrations.md "Zed and other ACP editors"), its
// provider menu open beside co:op's preset and account menus. The screenshot is the window inside its
// border, cut from a retina capture at 1178x694, with its two rounded window corners repainted in the
// bar color, so the page can round them at any size.
const integrations = `<figure class="zed"><img class="zed-shot" src="assets/img/zed-coop.png" width="589" height="347" ` +
	`alt="Zed's agent panel running co:op, its provider menu open on Claude Code, Codex, Gemini CLI and Grok, ` +
	`beside menus for the preset, account, model and effort"></figure>`

// The Orchestrator page (tools/site/orchestrator.tpl.html) tells one night of `coop loop` beside a
// pinned board. The board is drawn here in its last state, the morning, so the page reads whole
// without JavaScript; site/assets/js/orchestrator.js replays the night from the timeline embedded
// beside it.
//
// Tonight's queue: 121 tasks, 120 of them done by 07:00 and one left for your decision, as in the
// owner's 120-task, 9-hour run the main page cites. Only the tasks the board ever shows need a title.
const (
	nightN, nightWaiting, nightReopened = 121, 56, 119
	nightModel                          = "Opus 5.5"
	// The strip under the moon: one row of dots, each standing for a few tasks in queue order, so the
	// whole night fits in a glance.
	nightPerDot = 3
	// The decision a task leaves for you: its options, and the one the agent recommends.
	nightRecommended = "B"
)

var (
	nightTitles = map[int]string{
		0: "Make /checkout idempotent", 1: "Backfill parser tests", 2: "Cache the health probe",
		3: "Document the config file", 4: "Bump the Postgres driver", 5: "Remove the old importer",
		6: "Retry failed webhooks", 7: "Trim the Docker image",
		38: "Fix the flaky login test", 39: "Split the billing module", 40: "Paginate the orders API",
		41: "Index orders by customer", 42: "Add a dark mode toggle", 43: "Translate the signup form",
		44: "Log slow queries", 45: "Validate upload sizes", 46: "Archive stale carts",
		54: "Add a status page", 55: "Clean up feature flags", 56: "Choose session length",
		57: "Fix the timezone bug", 58: "Sort the admin tables", 59: "Export orders as CSV",
		60: "Upgrade to React 19", 61: "Cache the product pages", 62: "Compress API responses", 63: "Add a sitemap",
		116: "Lazy-load the images", 117: "Tidy the error messages", 118: "Add search to the docs",
		119: "Rate-limit the API", 120: "Speed up the test suite",
	}
	nightOptions = [][2]string{{"A", "1 day"}, {"B", "7 days"}, {"C", "30 days"}}
)

func nightCommit(i int) string {
	sum := sha1.Sum([]byte(nightTitles[i]))
	return hex.EncodeToString(sum[:])[:7]
}

// nightState is the board at one part of the story, which orchestrator.js reads by its JSON keys.
// Only the first Queued tasks exist yet (all of them when it is nil), and Tail shows the newest of
// them, the ones just added. Tasks before Done in the queue are finished, except the one Waiting for
// your answer and the one in hand (Now, Reopened when the final review sent it back); the rest wait
// their turn. On is the account the task in hand runs on, Use how much of each account's usage limit
// is spent; Handoff marks the step the next account took over; Review is the headline while the
// final review runs; Last is a task that finished after the ones behind it in the queue; Ask opens
// the decision in full, on the steps about it, where everywhere else it is one line. The optional
// numbers are pointers because 0 is a real value: the script reads a missing Now, Waiting or Last
// as no task and a missing Queued as all of them, so a zero must still be written.
type nightState struct {
	Clock    string      `json:"clock"`
	Headline string      `json:"headline,omitempty"`
	Done     int         `json:"done"`
	Queued   *int        `json:"queued,omitempty"`
	Tail     bool        `json:"tail,omitempty"`
	Now      *int        `json:"now,omitempty"`
	Reopened bool        `json:"reopened,omitempty"`
	Waiting  *int        `json:"waiting,omitempty"`
	Ask      bool        `json:"ask,omitempty"`
	On       string      `json:"on,omitempty"`
	Use      *nightUsage `json:"use,omitempty"`
	Handoff  bool        `json:"handoff,omitempty"`
	Review   string      `json:"review,omitempty"`
	Last     *int        `json:"last,omitempty"`
}

// nightUsage is the percent of each account's usage limit spent. It is a struct, not a map, because
// orchestrator.js draws the accounts in the order the JSON lists them, and a map would put home first.
type nightUsage struct {
	Work int `json:"work"`
	Home int `json:"home"`
}

// One state per story part: the hero, then each step.
var nightTimeline = []nightState{
	{Clock: "22:00", Headline: "118 tasks queued for tonight", Queued: new(118)},
	{Clock: "22:00", Headline: "121 tasks queued for tonight", Tail: true},
	{Clock: "22:01", Now: new(0), On: "work", Use: &nightUsage{Work: 12}},
	{Clock: "22:14", Done: 3, Now: new(3), On: "work", Use: &nightUsage{Work: 24}},
	{Clock: "01:12", Done: 42, Now: new(42), On: "home", Use: &nightUsage{Work: 100, Home: 6}, Handoff: true},
	{Clock: "02:31", Done: 59, Now: new(59), Waiting: new(nightWaiting), Ask: true, On: "home",
		Use: &nightUsage{Work: 100, Home: 31}},
	{Clock: "05:44", Done: nightN, Now: new(nightReopened), Reopened: true, Waiting: new(nightWaiting), On: "home",
		Use: &nightUsage{Work: 100, Home: 74}, Review: "Final review of 120 tasks"},
	{Clock: "07:00", Headline: "120 tasks done. One decision for you.", Done: nightN, Waiting: new(nightWaiting),
		Ask: true, Last: new(nightReopened)},
}

func nightStatus(state nightState, i int) string {
	queued := nightN
	if state.Queued != nil {
		queued = *state.Queued
	}
	if i >= queued {
		return "none"
	}
	if state.Waiting != nil && i == *state.Waiting {
		return "waiting"
	}
	if state.Now != nil && i == *state.Now {
		if state.Reopened {
			return "reopened"
		}
		return "active"
	}
	if i < state.Done {
		return "done"
	}
	return "todo"
}

// nightDot is a dot's state: the liveliest of its tasks, one coming back from the review, the one in
// hand, the decision waiting, any still to do, else done (or, before a task is queued, nothing yet).
func nightDot(state nightState, k int) string {
	rank := []string{"none", "done", "todo", "waiting", "active", "reopened"}
	liveliest := 0
	for i := k * nightPerDot; i < min(nightN, (k+1)*nightPerDot); i++ {
		liveliest = max(liveliest, slices.Index(rank, nightStatus(state, i)))
	}
	return rank[liveliest]
}

// A task's status as one icon: a ring that turns while an agent works, closes on a tick when done,
// asks while it waits for you, and turns back the other way, arrow first, when the review reopens it.
const nightStatusIcon = `<svg class="status" viewBox="0 0 24 24" aria-hidden="true"><circle class="track" cx="12" cy="12" r="9"/>` +
	`<g class="turn"><path class="arc" d="M12 3a9 9 0 1 1 0 18a9 9 0 1 1 0-18" pathLength="100"/><path class="head" d="M15 .2L12 3l3 2.8"/></g>` +
	`<path class="tick" d="M8 12.4l2.8 2.8 5.2-5.7" pathLength="100"/>` +
	`<path class="ask" d="M9.5 9.6a2.5 2.5 0 1 1 3.6 2.2c-.7.4-1.1.9-1.1 1.6v.3M12 16.5h.01"/></svg>`

// The agent on the task in hand: its model, and the accounts it can run on, each filling as its
// usage does.
var nightAgent = `<span class="agent">` + mark("claude") + `<span class="model">` + nightModel + `</span><span class="accounts"></span></span>` +
	`<span class="account"><i class="used"></i><span class="name"></span></span>`

// nightRow is a task on the board; a negative i is the empty row the script fills in.
func nightRow(i int, state string) string {
	attrs, title, commit, options := "", "", "", ""
	if i >= 0 {
		attrs = ` data-task="` + strconv.Itoa(i) + `" data-state="` + state + `"`
		title, commit = html.EscapeString(nightTitles[i]), nightCommit(i)
	}
	if i == nightWaiting {
		attrs += " data-open"
		options = `<ol class="options">`
		for _, option := range nightOptions {
			if option[0] == nightRecommended {
				options += `<li class="pick"><span class="key">` + option[0] + "</span>" + html.EscapeString(option[1]) + `<span class="rec">recommended</span></li>`
			} else {
				options += `<li><span class="key">` + option[0] + "</span>" + html.EscapeString(option[1]) + "</li>"
			}
		}
		options += "</ol>"
	}
	return `<li class="task"` + attrs + ">" + nightStatusIcon + `<span class="title">` + title + `</span><code class="commit">` + commit + "</code>" +
		options + "</li>"
}

// nightBoard is the morning, drawn whole for a reader without the script: the closing statement, the
// decision waiting, the newest commits and the strip of the whole queue.
func nightBoard() (string, error) {
	morning := nightTimeline[len(nightTimeline)-1]
	var done []int
	if morning.Last != nil {
		done = append(done, *morning.Last)
	}
	finished := 0
	for i := nightN - 1; i >= 0; i-- {
		if nightStatus(morning, i) == "done" {
			finished++
			if morning.Last == nil || i != *morning.Last {
				done = append(done, i)
			}
		}
	}
	if finished != 120 {
		return "", fmt.Errorf("the morning headline counts 120 tasks done, the timeline %d", finished)
	}
	dots := ""
	for k := range (nightN + nightPerDot - 1) / nightPerDot {
		dots += `<li data-state="` + nightDot(morning, k) + `"></li>`
	}
	newest := ""
	for _, i := range done[:3] {
		newest += nightRow(i, "done")
	}
	commits := make(map[int]string, len(nightTitles))
	for i := range nightTitles {
		commits[i] = nightCommit(i)
	}
	timeline, err := json.Marshal(struct {
		N       int            `json:"n"`
		Per     int            `json:"per"`
		Titles  map[int]string `json:"titles"`
		Commits map[int]string `json:"commits"`
		States  []nightState   `json:"states"`
	}{N: nightN, Per: nightPerDot, Titles: nightTitles, Commits: commits, States: nightTimeline})
	if err != nil {
		return "", err
	}
	return `<div class="board" data-day aria-hidden="true">` +
		`<p class="board-head"><span class="board-title">coop loop</span>` +
		`<span class="clock"><time>` + morning.Clock + "</time></span></p>" +
		`<div class="night-track" style="--night: 1"><span class="sky"></span></div>` +
		`<ol class="cells">` + dots + "</ol>" +
		`<div class="focus"><p class="headline">` + html.EscapeString(morning.Headline) + `</p><ol class="lane now"></ol></div>` +
		`<ol class="lane waiting">` + nightRow(nightWaiting, "waiting") + "</ol>" +
		`<ol class="lane next"></ol>` +
		`<ol class="lane done">` + newest + `<li class="rest">… <span class="count">` + strconv.Itoa(len(done)-3) + "</span> more</li></ol>" +
		"</div>" +
		`<template id="night-task">` + nightRow(-1, "") + `</template><template id="night-agent">` + nightAgent + "</template>" +
		`<script type="application/json" id="night-timeline">` + string(timeline) + "</script>", nil
}
