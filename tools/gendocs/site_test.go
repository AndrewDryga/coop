package main

// The site's content checks, which make docs-check runs: what the generated pages and the
// hand-written docs must hold.

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// checkout is the repository, seen from this package's directory.
const checkout = "../.."

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readSite(t *testing.T, name string) string {
	t.Helper()
	return readFile(t, filepath.Join(checkout, "site", filepath.FromSlash(name)))
}

type element struct {
	tag   string
	attrs map[string]string
}

// parsedPage is a page as a reader's tools see it: its tags in order, the text of each command a Copy
// button copies, the links of the main navigation, and all its text.
type parsedPage struct {
	elements    []element
	copySources []string
	mainNav     []string
	text        string
}

func parsePage(t *testing.T, source string) parsedPage {
	t.Helper()
	var page parsedPage
	var text strings.Builder
	copying, inNav := false, false
	end := func(tag string) {
		switch tag {
		case "code":
			copying = false
		case "nav":
			inNav = false
		}
	}
	z := html.NewTokenizer(strings.NewReader(source))
	for {
		switch kind := z.Next(); kind {
		case html.ErrorToken:
			if !errors.Is(z.Err(), io.EOF) {
				t.Fatal(z.Err())
			}
			page.text = text.String()
			return page
		case html.StartTagToken, html.SelfClosingTagToken:
			token := z.Token()
			attrs := map[string]string{}
			for _, a := range token.Attr {
				attrs[a.Key] = a.Val
			}
			page.elements = append(page.elements, element{token.Data, attrs})
			if _, ok := attrs["data-copy-source"]; ok && token.Data == "code" {
				copying = true
				page.copySources = append(page.copySources, "")
			}
			if token.Data == "nav" && attrs["aria-label"] == "Main" {
				inNav = true
			}
			if token.Data == "a" && inNav {
				page.mainNav = append(page.mainNav, attrs["href"])
			}
			if kind == html.SelfClosingTagToken {
				end(token.Data)
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			end(string(name))
		case html.TextToken:
			data := string(z.Text())
			text.WriteString(data)
			if copying {
				page.copySources[len(page.copySources)-1] += data
			}
		}
	}
}

// has reports whether the page holds a tag with exactly these attributes.
func (p parsedPage) has(tag string, attrs map[string]string) bool {
	return slices.ContainsFunc(p.elements, func(e element) bool { return e.tag == tag && maps.Equal(e.attrs, attrs) })
}

func (p parsedPage) count(match func(element) bool) int {
	n := 0
	for _, e := range p.elements {
		if match(e) {
			n++
		}
	}
	return n
}

var copySource = regexp.MustCompile(`(?s)<code data-copy-source>(.*?)</code>`)

// copiedCommands is the text of every command a Copy button copies in the markup.
func copiedCommands(markup string) []string {
	var commands []string
	for _, m := range copySource.FindAllStringSubmatch(markup, -1) {
		commands = append(commands, html.UnescapeString(htmlTag.ReplaceAllString(m[1], "")))
	}
	return commands
}

func shellParses(t *testing.T, command string) {
	t.Helper()
	sh := exec.Command("sh", "-n")
	sh.Stdin = strings.NewReader(command)
	if out, err := sh.CombinedOutput(); err != nil {
		t.Errorf("sh -n rejects %q: %v\n%s", command, err, out)
	}
}

func TestEvaluationsAreNavigableAndCopyable(t *testing.T) {
	source := readSite(t, "docs.html")
	page := parsePage(t, source)
	if page.count(func(e element) bool { return e.tag == "a" && e.attrs["href"] == "#evals" }) == 0 {
		t.Error("nothing links to #evals")
	}
	if n := page.count(func(e element) bool { return e.tag == "section" && e.attrs["id"] == "evals" }); n != 1 {
		t.Errorf("%d sections are #evals, want 1", n)
	}
	_, section, _ := strings.Cut(source, `id="evals">`)
	section, _, _ = strings.Cut(section, "</section>")
	commands := copiedCommands(section)
	if len(commands) < 3 || !strings.Contains(commands[0], "--dry-run") {
		t.Errorf("the evals section copies %q, want at least three commands, the first a --dry-run", commands)
	}
}

var (
	plainCodeBlock  = regexp.MustCompile(`(?s)<pre class="code"[^>]*>(.*?)</pre>`)
	commandList     = regexp.MustCompile(`(?s)<ul class="cmds"[^>]*>(.*?)</ul>`)
	commandRow      = regexp.MustCompile(`(?s)<code data-copy-source>(.*?)</code>(?:<span class="cmd-note">(.*?)</span>)?`)
	placeholderName = regexp.MustCompile(`<[\w-]+>`)
)

func TestDocsCommandsCopyAsRunnableShell(t *testing.T) {
	source := readSite(t, "docs.html")
	// every shell command sits in a copyable list, never in a plain code block, so each one copies alone
	for _, block := range plainCodeBlock.FindAllStringSubmatch(source, -1) {
		if strings.Contains(block[1], `<span class="k">`) {
			t.Errorf("a shell command in a plain code block has no Copy button of its own:\n%s", block[1])
		}
	}
	commands := copiedCommands(source)
	if len(commands) <= 30 {
		t.Errorf("the docs copy %d commands, want more than 30", len(commands))
	}
	// a note sits beside its command in one line down to a 1024px window: 57 characters for both
	for _, list := range commandList.FindAllStringSubmatch(source, -1) {
		rows := commandRow.FindAllStringSubmatch(list[1], -1)
		if rows == nil {
			t.Errorf("a command list without a command:\n%s", list[1])
		}
		widest := 0
		for _, row := range rows {
			widest = max(widest, utf8.RuneCountInString(html.UnescapeString(htmlTag.ReplaceAllString(row[1], ""))))
		}
		for _, row := range rows {
			if note := row[2]; note != "" && widest+utf8.RuneCountInString(html.UnescapeString(note)) > 57 {
				t.Errorf("this note wraps beside its block's widest command: %s", note)
			}
		}
	}
	for _, command := range commands {
		if strings.HasPrefix(command, "$") || strings.Contains(command, "#") {
			t.Errorf("a copied command carries a prompt or a comment: %s", command)
		}
		shellParses(t, placeholderName.ReplaceAllString(command, "placeholder")) // <host>, <url>: names you fill in
	}
}

var retiredVolumeFlag = regexp.MustCompile(`\bcoop down (?:-v|--volumes)\b`)

func TestServiceExamplesUseTheSupportedDeleteFlag(t *testing.T) {
	for name, text := range map[string]string{
		"README.md": readFile(t, filepath.Join(checkout, "README.md")),
		"docs.html": parsePage(t, readSite(t, "docs.html")).text,
	} {
		if found := retiredVolumeFlag.FindAllString(text, -1); found != nil {
			t.Errorf("%s uses a retired flag: %q", name, found)
		}
		if !strings.Contains(text, "coop down --delete-volumes") {
			t.Errorf("%s is missing the supported volume-deletion command", name)
		}
	}
}

var promptMark = regexp.MustCompile(`^\$\s*`)

func TestSetupCommandsCopyAsRunnableShell(t *testing.T) {
	page := parsePage(t, readSite(t, "index.html"))
	// what site.js copies: the command's text without its "$ " prompt
	var commands []string
	for _, text := range page.copySources {
		commands = append(commands, strings.TrimSpace(promptMark.ReplaceAllString(text, "")))
	}
	buttons := page.count(func(e element) bool { _, copy := e.attrs["data-copy"]; return e.tag == "button" && copy })
	want := []string{"curl -fsSL https://coop.dryga.com/install.sh | sh", "coop login claude", "cd your-project && coop init", "coop claude"}
	if buttons != len(commands) || !slices.Equal(commands, want) {
		t.Errorf("%d Copy buttons copy %q, want one each for %q", buttons, commands, want)
	}
	for _, command := range commands {
		shellParses(t, command)
	}
}

var (
	attackCase    = regexp.MustCompile(`(?s)<li class="attack" data-attack="([\w-]+)">(.*?)</li>`)
	caseBlock     = regexp.MustCompile(`(?s)<div class="case">(.*?)</div>`)
	attackChapter = regexp.MustCompile(`(?s)<ol class="attacks"[^>]*>(.*?)</ol>`)
)

func TestCaseFiguresSitOutsideTheirCaseBlock(t *testing.T) {
	// A case's block is its threat and how the box stops it. The owner rejected a figure set
	// inside it (2026-10-05): a figure goes above or below the block, never between those lines.
	cases := attackCase.FindAllStringSubmatch(readSite(t, "index.html"), -1)
	if len(cases) < 7 {
		t.Fatalf("the homepage has %d cases, want at least 7", len(cases))
	}
	for _, c := range cases {
		block := caseBlock.FindStringSubmatch(c[2])
		switch {
		case block == nil:
			t.Errorf("case %s has no .case block", c[1])
		case !strings.Contains(block[1], `class="threat"`):
			t.Errorf("case %s: its block has no threat", c[1])
		case strings.Contains(block[1], `class="evidence"`):
			t.Errorf("case %s: a figure inside the case's block", c[1])
		}
	}
}

func TestAChapterGivesEveryCaseAFigureOrNone(t *testing.T) {
	// Pinned, the figure of the case on screen shows above the rail. A case without one left that
	// space empty, and the owner rejected the gap (2026-10-05).
	chapters := attackChapter.FindAllStringSubmatch(readSite(t, "index.html"), -1)
	if len(chapters) < 2 {
		t.Fatalf("the homepage has %d chapters of cases, want at least 2", len(chapters))
	}
	for _, chapter := range chapters {
		cases := attackCase.FindAllStringSubmatch(chapter[1], -1)
		var bare []string
		for _, c := range cases {
			if !strings.Contains(c[2], `class="evidence"`) {
				bare = append(bare, c[1])
			}
		}
		if len(bare) != 0 && len(bare) != len(cases) {
			t.Errorf("only some cases have a figure; missing: %v", bare)
		}
	}
}

func TestInstallScriptIsPublishedFromTheRepoNotCopied(t *testing.T) {
	// coop.dryga.com/install.sh is the repo's install.sh, copied in by the Pages job at deploy.
	// A committed site/install.sh would drift from it, and a deploy that skipped install.sh
	// changes would serve a stale installer.
	if _, err := os.Stat(filepath.Join(checkout, "site", "install.sh")); err == nil {
		t.Error("site/install.sh must not be committed")
	}
	workflow := readFile(t, filepath.Join(checkout, ".github", "workflows", "pages.yml"))
	if !strings.Contains(workflow, "run: cp install.sh site/install.sh") {
		t.Error("the Pages job must copy install.sh into the site")
	}
	if !strings.Contains(workflow, "'install.sh'") {
		t.Error("an install.sh change must redeploy the site")
	}
}

func TestTerminalsMatchTheirCLISources(t *testing.T) {
	problems, err := terminalDrift(readFrom(checkout))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Errorf("terminal output no longer matches its source:\n%s", strings.Join(problems, "\n"))
	}
}

func TestTerminalDriftNamesThePageBlockAndLine(t *testing.T) {
	// Drift on each side: the CLI rewords a line the loop shows, the doctor's transcript loses a
	// check, and the homepage shows a fork line the CLI never prints.
	drifted := func(rel string) (string, error) {
		text, err := readFrom(checkout)(rel)
		switch {
		case rel == "internal/loop/report.go":
			text = strings.ReplaceAll(text, "Task completed: ", "Finished: ")
		case strings.HasSuffix(rel, "18a-doctor-all-passed.txt"):
			text = strings.ReplaceAll(text, "  ✓ .envrc is hidden\n", "")
		case rel == "site/index.html":
			text = strings.ReplaceAll(text, "Rebasing onto main", "Replaying onto main")
		}
		return text, err
	}
	problems, err := terminalDrift(drifted)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`index.html, block loop: shape "Task completed: {}" needs "Task completed: "`,
		`docs.html, block loop: shape "Task completed: {}" needs "Task completed: "`,
		`docs.html, block doctor: line "  ✓ .envrc is hidden" is not in internal/cli/testdata/approved/18a-doctor-all-passed.txt`,
		`index.html, block fork: line "Replaying onto main" matches no shape listed for it`,
	} {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, want) }) {
			t.Errorf("no problem starts %s; got:\n%s", want, strings.Join(problems, "\n"))
		}
	}
}

func TestPagesShareTheMainNavigation(t *testing.T) {
	for name, install := range map[string]string{"index.html": "#start", "orchestrator.html": "#start", "docs.html": "./#start"} {
		page := parsePage(t, readSite(t, name))
		for _, href := range []string{"orchestrator.html", "docs.html", "https://github.com/AndrewDryga/coop", install} {
			if !slices.Contains(page.mainNav, href) {
				t.Errorf("%s: the main navigation does not link %s", name, href)
			}
		}
		if page.count(func(e element) bool { return e.tag == "a" && e.attrs["href"] == "#main" }) == 0 {
			t.Errorf("%s: missing skip link", name)
		}
		if page.count(func(e element) bool { return e.attrs["id"] == "main" }) == 0 {
			t.Errorf("%s: missing #main", name)
		}
	}
}

// offSite is a link to another host: http(s), and not coop.dryga.com.
func offSite(href string) bool {
	rest, ok := strings.CutPrefix(href, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(href, "http://")
	}
	return ok && rest != siteHost && !strings.HasPrefix(rest, siteHost+"/") && !strings.HasPrefix(rest, siteHost+":")
}

func TestLinksOffTheSiteOpenInANewTabWithNofollow(t *testing.T) {
	for _, name := range []string{"index.html", "orchestrator.html", "docs.html"} {
		external := 0
		for _, e := range parsePage(t, readSite(t, name)).elements {
			href := e.attrs["href"]
			if e.tag != "a" || href == "" {
				continue
			}
			target, targeted := e.attrs["target"]
			rel := strings.Fields(e.attrs["rel"])
			switch {
			case !offSite(href) && targeted:
				t.Errorf("%s: %s stays on the site but opens a new tab", name, href)
			case offSite(href) && (target != "_blank" || !slices.Contains(rel, "nofollow") || !slices.Contains(rel, "noopener")):
				t.Errorf(`%s: %s leaves the site without target="_blank" and rel="nofollow noopener"`, name, href)
			}
			if offSite(href) {
				external++
			}
		}
		if external == 0 {
			t.Errorf("%s should cite something", name)
		}
	}
}

func TestFadedCaseFiguresCannotTakeClicks(t *testing.T) {
	// The story stacks a chapter's case figures in one cell and fades all but the current one. At
	// opacity 0 alone, a later figure stayed on top and took the clicks meant for the visible
	// figure's link, so a faded figure must be hidden as well.
	css := readSite(t, "assets/css/site.css")
	for rule, want := range map[string]string{
		`\.js-story \.case-figures > \.evidence \{([^}]*)\}`:  "visibility: hidden",
		`\.js-story \.case-figures > \.is-active \{([^}]*)\}`: "visibility: visible",
	} {
		if found := regexp.MustCompile(rule).FindStringSubmatch(css); found == nil || !strings.Contains(found[1], want) {
			t.Errorf("site.css: %s needs %s", rule, want)
		}
	}
}

func TestExternalLinkMarkingKeepsRelAndSettles(t *testing.T) {
	page := strings.Join([]string{
		`<a href="https://github.com/x">a</a>`,
		`<a class="c" rel="me" target="_self" href="http://example.org/">b</a>`,
		`<a href='https://example.com/q?a=1'>c</a>`,
		`<a href="https://coop.dryga.com/docs.html">d</a>`,
		`<a href="docs.html#loop">e</a> <a href="#main">f</a> <abbr title="https://x.org">g</abbr>`,
	}, "\n")
	want := strings.Join([]string{
		`<a href="https://github.com/x" target="_blank" rel="nofollow noopener">a</a>`,
		`<a class="c" rel="me nofollow noopener" target="_blank" href="http://example.org/">b</a>`,
		`<a href='https://example.com/q?a=1' target="_blank" rel="nofollow noopener">c</a>`,
		`<a href="https://coop.dryga.com/docs.html">d</a>`,
		`<a href="docs.html#loop">e</a> <a href="#main">f</a> <abbr title="https://x.org">g</abbr>`,
	}, "\n")
	marked := externalLinks(page)
	if marked != want {
		t.Errorf("marked:\n%s\nwant:\n%s", marked, want)
	}
	if again := externalLinks(marked); again != marked {
		t.Errorf("marking again changed the page:\n%s", again)
	}
}

var schemeOrAnchor = regexp.MustCompile(`^(?:[a-zA-Z][a-zA-Z0-9+.-]*:|//|#)`)

// brokenLocalLinks lists the href and src values on a page that do not name a file in the page's
// folder. Anchors and queries are dropped; other schemes, links off the site (//host) and in-page
// anchors are skipped, and a folder link means its index.html. srcset is not read: no page uses it.
func brokenLocalLinks(t *testing.T, path string) []string {
	t.Helper()
	root := filepath.Dir(path)
	var broken []string
	for _, e := range parsePage(t, readFile(t, path)).elements {
		for _, link := range []string{e.attrs["href"], e.attrs["src"]} {
			if link == "" || schemeOrAnchor.MatchString(link) {
				continue
			}
			local, _, _ := strings.Cut(link, "#")
			local, _, _ = strings.Cut(local, "?")
			if unescaped, err := url.PathUnescape(local); err == nil {
				local = unescaped
			}
			if local == "" {
				continue // a query alone links the page itself
			}
			target := filepath.Join(root, strings.TrimLeft(local, "/"))
			if strings.HasSuffix(local, "/") {
				target = filepath.Join(target, "index.html")
			}
			rel, err := filepath.Rel(root, target)
			inside := err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
			if info, err := os.Stat(target); !inside || err != nil || !info.Mode().IsRegular() {
				broken = append(broken, link)
			}
		}
	}
	return broken
}

func TestLocalLinksPointAtFilesThatExist(t *testing.T) {
	pages, err := filepath.Glob(filepath.Join(checkout, "site", "*.html"))
	if err != nil || pages == nil {
		t.Fatalf("no pages in site/: %v", err)
	}
	for _, page := range pages {
		if broken := brokenLocalLinks(t, page); broken != nil {
			t.Errorf("%s: a link to a deleted page or asset 404s: %q", filepath.Base(page), broken)
		}
	}
}

func TestALinkToAMissingPageIsReported(t *testing.T) {
	tmp := t.TempDir()
	site := filepath.Join(tmp, "site")
	files := map[string]string{
		filepath.Join(site, "docs.html"):    "",
		filepath.Join(site, "my page.html"): "",
		filepath.Join(tmp, "README.md"):     "", // outside the site: the live site has no such file
		filepath.Join(site, "index.html"): `<a href="docs.html#start">kept</a><a href="./#top">home</a><a href="/docs.html">root</a>` +
			`<a href="docs.html?v=1">busted</a><a href="my%20page.html">spaced</a>` +
			`<script src="//cdn.example/x.js"></script><a href="https://example.com/x">off</a>` +
			`<a href="mailto:a@example.com">mail</a>` +
			`<a href="gone.html">gone</a><img src="assets/missing.png?v=1"><a href="../README.md">up</a>`,
	}
	if err := os.Mkdir(site, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := brokenLocalLinks(t, filepath.Join(site, "index.html"))
	if want := []string{"gone.html", "assets/missing.png?v=1", "../README.md"}; !slices.Equal(got, want) {
		t.Errorf("broken links %q, want %q", got, want)
	}
}

var sitemapPage = regexp.MustCompile(`<loc>https://coop\.dryga\.com/([^<]*)</loc>`)

func TestSitemapListsEveryCanonicalPage(t *testing.T) {
	listed := map[string]bool{}
	for _, m := range sitemapPage.FindAllStringSubmatch(readSite(t, "sitemap.xml"), -1) {
		listed[m[1]] = true
	}
	pages := map[string]bool{}
	paths, err := filepath.Glob(filepath.Join(checkout, "site", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		if name == "index.html" {
			name = ""
		}
		pages[name] = true
	}
	if !maps.Equal(listed, pages) {
		t.Errorf("the sitemap lists %v, want every page and no page that is gone: %v", slices.Sorted(maps.Keys(listed)), slices.Sorted(maps.Keys(pages)))
	}
}

var taskQueueRow = regexp.MustCompile(`(?s)<h3>Task queue</h3>.*?</li>`)

func TestTaskQueueLeadsToTheOrchestratorPage(t *testing.T) {
	row := taskQueueRow.FindString(readSite(t, "index.html"))
	if row == "" {
		t.Fatal("the homepage lost its Task queue feature")
	}
	if !strings.Contains(row, `href="orchestrator.html"`) {
		t.Error("the Task queue feature does not lead to the Orchestrator page")
	}
}

func TestOrchestratorMetadataAndAssetsUseTheCanonicalName(t *testing.T) {
	page := parsePage(t, readSite(t, "orchestrator.html"))
	const canonical = "https://coop.dryga.com/orchestrator.html"
	for _, want := range []element{
		{"link", map[string]string{"rel": "canonical", "href": canonical}},
		{"meta", map[string]string{"property": "og:url", "content": canonical}},
		{"meta", map[string]string{"property": "og:image", "content": "https://coop.dryga.com/assets/img/og-orchestrator.png"}},
	} {
		if !page.has(want.tag, want.attrs) {
			t.Errorf("missing <%s %v>", want.tag, want.attrs)
		}
	}
	if page.count(func(e element) bool {
		return e.tag == "a" && e.attrs["href"] == "orchestrator.html" && e.attrs["aria-current"] == "page"
	}) == 0 {
		t.Error("the Orchestrator menu item must mark the current page")
	}
	assets := map[string]bool{}
	for _, e := range page.elements {
		link, ok := e.attrs["href"]
		if !ok {
			link = e.attrs["src"]
		}
		path, _, _ := strings.Cut(link, "?")
		assets[path] = true
	}
	if !assets["assets/css/orchestrator.css"] || !assets["assets/js/orchestrator.js"] {
		t.Error("the Orchestrator page must load orchestrator.css and orchestrator.js")
	}
}

func TestEachLandingPageNamesItsCommittedSocialCard(t *testing.T) {
	// The cards are redrawn by hand (site/README.md, "SEO assets") and committed beside the page.
	for name, card := range map[string]string{"index.html": "og-image.png", "orchestrator.html": "og-orchestrator.png"} {
		page := parsePage(t, readSite(t, name))
		if !page.has("meta", map[string]string{"property": "og:image", "content": "https://coop.dryga.com/assets/img/" + card}) {
			t.Errorf("%s does not name its social card, %s", name, card)
		}
		if _, err := os.Stat(filepath.Join(checkout, "site", "assets", "img", card)); err != nil {
			t.Error(err)
		}
	}
}

var (
	servicesRow  = regexp.MustCompile(`(?s)<h3>Project services</h3>.*?</li>`)
	evidence     = regexp.MustCompile(`(?s)<p class="evidence"><strong>[^<]+</strong>.*?</p>`)
	measuredWith = regexp.MustCompile(`github\.com/AndrewDryga/coop/blob/main/(tools/[\w./-]+)`)
)

func TestTheServicesFigureLinksTheToolThatMeasuredIt(t *testing.T) {
	// our own number: the reader can rerun the measurement, so the tool it links must exist
	row := servicesRow.FindString(readSite(t, "orchestrator.html"))
	if row == "" {
		t.Fatal("the Orchestrator page lost its Project services feature")
	}
	figure := evidence.FindString(row)
	if figure == "" {
		t.Fatal("the Project services row has no figure")
	}
	tool := measuredWith.FindStringSubmatch(figure)
	if tool == nil {
		t.Fatal("the services figure does not link the tool that measured it")
	}
	if info, err := os.Stat(filepath.Join(checkout, filepath.FromSlash(tool[1]))); err != nil || !info.Mode().IsRegular() {
		t.Errorf("%s is not a file in the repository", tool[1])
	}
}

// commandField is what one .command owns, as site.js's copy and picker handlers read it: the copied
// text before, inside and after its rewritable data-cmd span, and how many copy sources, buttons and
// status lines sit directly in it.
type commandField struct {
	prefix, text, suffix, step string
	sources, buttons, statuses int
}

var voidElements = []string{"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr"}

func parseCommand(markup string) (commandField, error) {
	var field commandField
	type frame struct{ tag string }
	var stack []*frame
	var root, code, cmd *frame
	afterCmd, seenCmd := false, false
	end := func(tag string) error {
		if len(stack) == 0 || stack[len(stack)-1].tag != tag {
			return errors.New("command markup must be balanced: closed " + tag)
		}
		closed := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if closed == cmd {
			cmd, afterCmd = nil, true
		}
		if closed == code {
			code = nil
		}
		return nil
	}
	z := html.NewTokenizer(strings.NewReader(`<div class="command">` + markup + "</div>"))
	for {
		switch kind := z.Next(); kind {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				return field, nil
			}
			return field, z.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			token := z.Token()
			if slices.Contains(voidElements, token.Data) {
				continue
			}
			attrs := map[string]string{}
			for _, a := range token.Attr {
				attrs[a.Key] = a.Val
			}
			has := func(key string) bool { _, ok := attrs[key]; return ok }
			opened := &frame{token.Data}
			var parent *frame
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			if root == nil {
				root = opened
			}
			if token.Data == "code" && has("data-copy-source") {
				if parent != root {
					return field, errors.New("copy source must belong to its command")
				}
				code = opened
				field.sources++
			}
			if has("data-cmd") {
				if token.Data != "span" || parent != code || seenCmd {
					return field, errors.New("command must have one direct mutable span")
				}
				cmd, seenCmd, field.step = opened, true, attrs["data-cmd"]
			}
			if token.Data == "button" && has("data-copy") {
				if parent != root {
					return field, errors.New("copy button must belong to its command")
				}
				field.buttons++
			}
			if has("data-copy-status") {
				if parent != root {
					return field, errors.New("copy status must belong to its command")
				}
				field.statuses++
			}
			stack = append(stack, opened)
			if kind == html.SelfClosingTagToken {
				if err := end(token.Data); err != nil {
					return field, err
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if slices.Contains(voidElements, string(name)) {
				continue
			}
			if err := end(string(name)); err != nil {
				return field, err
			}
		case html.TextToken:
			switch data := string(z.Text()); {
			case code == nil:
			case cmd != nil:
				field.text += data
			case afterCmd:
				field.suffix += data
			default:
				field.prefix += data
			}
		}
	}
}

var (
	commandMarkup = regexp.MustCompile(`(?s)<div class="command">(.*?)</div>`)
	// a step of site.js's picker table and the command it writes: ["login", `coop login ${agent}`]
	pickerStep = regexp.MustCompile("\\[\"(\\w+)\", `([^`]*)`\\]")
)

// pickerCommands is what a reader copies after choosing an agent, ${agent} standing for its name.
var pickerCommands = map[string]string{"login": "coop login ${agent}", "run": "coop ${agent}", "loop": "coop loop ${agent}"}

// The picker rewrites each command's data-cmd span, and the copy handlers copy what a command shows,
// so each command owns one copy source, button and status line, with its one rewritable span directly
// in the copy source. Executing site.js itself needed Node, which the repository's tools no longer
// use; its picker table must still write the exact command for every step a page uses.
func TestEveryStartCommandFollowsTheAgentPicker(t *testing.T) {
	written := map[string]string{}
	for _, m := range pickerStep.FindAllStringSubmatch(readSite(t, "assets/js/site.js"), -1) {
		written[m[1]] = m[2]
	}
	for name, steps := range map[string][]string{"index.html": {"login", "run"}, "orchestrator.html": {"login", "loop"}} {
		source := readSite(t, name)
		page := parsePage(t, source)
		var used []string
		commands := commandMarkup.FindAllStringSubmatch(source, -1)
		for _, m := range commands {
			field, err := parseCommand(m[1])
			if err != nil {
				t.Fatalf("%s: %v:\n%s", name, err, m[1])
			}
			if field.sources != 1 || field.buttons != 1 || field.statuses != 1 {
				t.Errorf("%s: a command owns %d copy sources, %d buttons and %d status lines, want one each",
					name, field.sources, field.buttons, field.statuses)
			}
			if field.step == "" {
				continue
			}
			used = append(used, field.step)
			if want := pickerCommands[field.step]; written[field.step] != want {
				t.Errorf("%s: site.js's picker writes %q for the %s command, want %q", name, written[field.step], field.step, want)
			}
			// Without the script the commands stay on Claude, the picker's first choice.
			want := strings.ReplaceAll(pickerCommands[field.step], "${agent}", "claude")
			if copied := strings.TrimSpace(promptMark.ReplaceAllString(field.prefix+field.text+field.suffix, "")); copied != want {
				t.Errorf("%s: the %s command copies %q, want %q", name, field.step, copied, want)
			}
		}
		if !slices.Equal(used, steps) {
			t.Errorf("%s: the picker's commands are %v, want %v", name, used, steps)
		}
		buttons := page.count(func(e element) bool { _, copy := e.attrs["data-copy"]; return e.tag == "button" && copy })
		statuses := page.count(func(e element) bool { _, status := e.attrs["data-copy-status"]; return status })
		if len(commands) != len(page.copySources) || buttons != len(commands) || statuses != len(commands) {
			t.Errorf("%s: %d commands, %d copy sources, %d Copy buttons and %d status lines, want one of each per command",
				name, len(commands), len(page.copySources), buttons, statuses)
		}
		var agents []string
		for _, e := range page.elements {
			agent, ok := e.attrs["data-agent"]
			if !ok || e.tag != "button" {
				continue
			}
			tab := cmp.Or(e.attrs["tabindex"], "0")
			want := [2]string{"false", "-1"}
			if agents == nil { // Claude is chosen and in the tab order; arrow keys reach the others
				want = [2]string{"true", "0"}
			}
			if got := [2]string{e.attrs["aria-checked"], tab}; got != want {
				t.Errorf("%s: the %s choice has aria-checked and tabindex %q, want %q", name, agent, got, want)
			}
			agents = append(agents, agent)
		}
		if want := []string{"claude", "codex", "gemini", "grok"}; !slices.Equal(agents, want) {
			t.Errorf("%s: the agent picker offers %v, want %v", name, agents, want)
		}
	}
}

var (
	boardMarkup    = regexp.MustCompile(`(?s)<div class="board".*?<template id="night-task">`)
	boardDot       = regexp.MustCompile(`<li data-state="(\w+)"></li>`)
	waitingLane    = regexp.MustCompile(`(?s)<ol class="lane waiting">.*?</ol>`)
	timelineScript = regexp.MustCompile(`<script type="application/json" id="night-timeline">(.*?)</script>`)
)

func TestOrchestratorBoardReadsWholeWithoutTheScript(t *testing.T) {
	page := readSite(t, "orchestrator.html")
	drawn := boardMarkup.FindString(page)
	dots := boardDot.FindAllStringSubmatch(drawn, -1)
	if want := (nightN + nightPerDot - 1) / nightPerDot; len(dots) != want {
		t.Errorf("the strip has %d dots, want %d", len(dots), want)
	}
	waiting := 0
	for _, state := range dots {
		if state[1] == "waiting" {
			waiting++
		}
	}
	if waiting != 1 {
		t.Errorf("the morning keeps the one decision in sight: %d waiting dots", waiting)
	}
	if !strings.Contains(drawn, "120 tasks done. One decision for you.") {
		t.Error("the board does not close the night")
	}
	if !strings.Contains(waitingLane.FindString(drawn), "data-open") {
		t.Error("the decision waiting is not open")
	}
	script := timelineScript.FindStringSubmatch(page)
	if script == nil {
		t.Fatal("the page carries no night timeline")
	}
	var timeline struct {
		States []json.RawMessage `json:"states"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(script[1])), &timeline); err != nil {
		t.Fatal(err)
	}
	if steps := strings.Count(page, `<li class="night-step">`); len(timeline.States) != 1+steps {
		t.Errorf("%d timeline states for %d steps, want one for the hero and one per step", len(timeline.States), steps)
	}
}
