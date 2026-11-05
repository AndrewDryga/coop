package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// landingPages are the site's two marketing pages. Every other page is documentation.
var landingPages = []string{"index.html", "orchestrator.html"}

// ownerIDs is every id a reader's link can land on, per page.
func ownerIDs(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(checkout, "site", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]map[string]bool{}
	for _, file := range files {
		name := filepath.Base(file)
		ids[name] = map[string]bool{}
		for _, el := range parsePage(t, readFile(t, file)).elements {
			if id := el.attrs["id"]; id != "" {
				ids[name][id] = true
			}
		}
	}
	return ids
}

// A page outside the menu is how the docs got hard to navigate: nothing in the site's chrome led to
// it, so a reader found it only through a link in someone else's paragraph.
func TestEveryDocsPageIsInTheMenu(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(checkout, "site", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	var published []string
	for _, file := range files {
		if name := filepath.Base(file); !slices.Contains(landingPages, name) {
			published = append(published, name)
		}
	}
	var listed []string
	for _, page := range docsPages {
		listed = append(listed, page.Slug+".html")
		if page.Parent == "" {
			continue
		}
		if parent := docsPageBySlug(page.Parent); parent == nil || parent.Parent != "" || parent.Group != page.Group {
			t.Errorf("%s nests under %q, which is not a top-level page in its group", page.Slug, page.Parent)
		}
	}
	slices.Sort(listed)
	if !slices.Equal(listed, published) {
		t.Errorf("docsPages lists %v, and site/ publishes %v; every docs page needs exactly one entry", listed, published)
	}
	for i, page := range docsPages {
		if i > 0 && page.Group != docsPages[i-1].Group && slices.ContainsFunc(docsPages[:i], func(p docsPage) bool { return p.Group == page.Group }) {
			t.Errorf("group %q is split in two; keep its pages together", page.Group)
		}
	}
	for _, page := range docsPages {
		markup := readSite(t, page.Slug+".html")
		start := strings.Index(markup, `<nav aria-label="Documentation">`)
		if start < 0 {
			t.Errorf("%s does not show the docs menu", page.Slug)
			continue
		}
		menu, _, _ := strings.Cut(markup[start:], "</nav>")
		for _, other := range docsPages {
			if !strings.Contains(menu, `href="`+other.Slug+`.html"`) {
				t.Errorf("%s: the menu leaves out %s", page.Slug, other.Slug)
			}
		}
		current := regexp.MustCompile(`<a href="([\w-]+)\.html" aria-current="page">`).FindAllStringSubmatch(menu, -1)
		if len(current) != 1 || current[0][1] != page.Slug {
			t.Errorf("%s: the menu marks %v as the current page", page.Slug, current)
		}
		if !strings.Contains(markup, `<details class="docs-side" open>`) {
			t.Errorf("%s has no contents for its own sections", page.Slug)
		}
	}
}

// The owner rejected a menu sharing a column with the page's contents: the two lists merge and neither
// is usable. The article keeps its reading width and type whatever navigation does around it.
func TestMenuArticleAndContentsKeepTheirOwnColumns(t *testing.T) {
	css := readSite(t, "assets/css/site.css")
	for _, rule := range []string{
		`.docs-main { grid-column: 1; width: 100%; max-width: 44rem; min-width: 0; margin: 48px auto 0; font-size: 1.125rem; line-height: 1.7778; }`,
		`.guide-hero h1 { font-size: clamp(2.5rem, 1.9rem + 2vw, 3.25rem); }`,
		`.docs-side { position: sticky; top: 32px; grid-column: 2; grid-row: 1 / span 2;`,
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("site.css changed a rule the docs' layout rests on: %s", rule)
		}
	}
	for _, page := range docsPages {
		markup := readSite(t, page.Slug+".html")
		rail, contents := strings.Index(markup, `class="docs-rail"`), strings.Index(markup, `class="docs-side"`)
		main, title := strings.Index(markup, `<main id="main"`), strings.Index(markup, `class="guide-hero"`)
		if rail < 0 || main < rail || title < main || contents < title {
			t.Errorf("%s: want the menu, then <main> with the title and then the contents; got offsets %d, %d, %d, %d", page.Slug, rail, main, title, contents)
		}
	}
}

func TestPreviousAndNextFollowReadingOrder(t *testing.T) {
	for slug, want := range map[string][2]string{
		"docs":                {"", "install.html"},
		"sandbox-guide":       {"install.html", "overnight-guide.html"},
		"claude-code-sandbox": {"sandbox-guide.html", "overnight-guide.html"},
		"grok-cli-sandbox":    {"sandbox-guide.html", "overnight-guide.html"},
		"overnight-guide":     {"sandbox-guide.html", "box.html"},
		"migrating":           {"session-api.html", ""},
	} {
		pager := docsPager(slug)
		for i, class := range []string{"docs-pager-previous", "docs-pager-next"} {
			link := `class="` + class + `" href="` + want[i] + `"`
			if want[i] == "" {
				if strings.Contains(pager, class) {
					t.Errorf("%s: expected no %s in %s", slug, class, pager)
				}
			} else if !strings.Contains(pager, link) {
				t.Errorf("%s: missing %s in %s", slug, link, pager)
			}
		}
	}
}

// The website is the one place to read the docs. Source code, the changelog and the like stay on
// GitHub; a published page, or a README section standing in for one, is never a detour there.
func TestDocsDoNotSendReadersToGitHubForPublishedPages(t *testing.T) {
	links := regexp.MustCompile(`<a\b[^>]*\bhref="([^"]+)"[^>]*>(.*?)</a>`)
	for _, page := range docsPages {
		markup := readSite(t, page.Slug+".html")
		_, main, _ := strings.Cut(markup, `<main `)
		main, _, _ = strings.Cut(main, `</main>`)
		for _, m := range links.FindAllStringSubmatch(main, -1) {
			u, err := url.Parse(m[1])
			if err != nil || u.Host != "github.com" {
				continue
			}
			repository := strings.TrimPrefix(repoURL, "https://github.com")
			if u.Path == repository && u.Fragment != "" {
				t.Errorf("%s links %s, a README section on GitHub; link the page on this site", page.Slug, m[1])
			}
			file, found := strings.CutPrefix(u.Path, repository+"/blob/main/")
			if !found {
				continue
			}
			if file == page.Source && strings.Contains(m[2], "View this page") {
				continue
			}
			if file == "README.md" || docsPageBySource(file) != nil {
				t.Errorf("%s links %s on GitHub; link the page on this site", page.Slug, m[1])
			}
		}
	}
}

func TestSearchIndexCoversEveryDocsPage(t *testing.T) {
	var entries []searchEntry
	if err := json.Unmarshal([]byte(readSite(t, "search-index.json")), &entries); err != nil {
		t.Fatal(err)
	}
	ids := ownerIDs(t)
	pages := map[string]int{}
	for _, entry := range entries {
		page, fragment, _ := strings.Cut(entry.URL, "#")
		pages[page]++
		if slices.Contains(landingPages, page) {
			t.Errorf("%s is a landing page, not documentation", entry.URL)
		}
		if ids[page] == nil {
			t.Errorf("%s points at a page that does not exist", entry.URL)
		} else if fragment != "" && !ids[page][fragment] {
			t.Errorf("%s points at a section that does not exist", entry.URL)
		}
		if entry.Page == "" || entry.Heading == "" {
			t.Errorf("%s has no page name or heading: %+v", entry.URL, entry)
		}
		if strings.Contains(entry.Text, "Scroll horizontally") || strings.Contains(entry.Text, "View this page") {
			t.Errorf("%s indexes page furniture: %.80s", entry.URL, entry.Text)
		}
	}
	for _, page := range docsPages {
		if pages[page.Slug+".html"] == 0 {
			t.Errorf("search finds nothing on %s", page.Slug)
		}
	}
	// the sections people search by name, each one a deep link
	for _, want := range []string{"box.html#project-services", "networking.html#allow-one-website", "install.html#verifying-a-download", "install.html#uninstall", "agents.html#presets", "docs.html#loop", "claude-code-sandbox.html#setup"} {
		if !slices.ContainsFunc(entries, func(e searchEntry) bool { return e.URL == want }) {
			t.Errorf("the index has no entry for %s", want)
		}
	}
}

func TestSearchEntriesFollowHeadingsAndSkipFurniture(t *testing.T) {
	entries, err := searchEntries(docsPage{Slug: "page", Label: "A page", Parent: "guide", Keywords: "word"}, `<html><body><main>
		<header><p class="guide-eyebrow">Docs / Group</p><h1>The title.</h1><p>The lead.</p></header>
		<details class="docs-side"><nav><a href="#one">One</a></nav></details>
		<article>
			<section id="one"><h2><span aria-hidden="true">01</span>First <code>step</code></h2>
				<p>Run <code>coop</code>, then wait.</p>
				<figure class="window"><pre>terminal noise</pre></figure>
				<h3>No id stays in its section</h3>
				<h3 id="deep">Deep</h3><p>Deep text.</p>
			</section>
			<nav class="docs-pager"><a href="next.html">Next</a></nav>
		</article>
	</main></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	want := []searchEntry{
		{URL: "page.html", Page: "A page", Heading: "The title", Text: "The lead.", Keywords: "word", Variant: true},
		{URL: "page.html#one", Page: "A page", Heading: "First step", Text: "Run coop, then wait. No id stays in its section", Variant: true},
		{URL: "page.html#deep", Page: "A page", Heading: "Deep", Text: "Deep text.", Variant: true},
	}
	if !slices.Equal(entries, want) {
		t.Errorf("got %+v\nwant %+v", entries, want)
	}
}

// Released binaries print the help's links, and other sites link these sections, so they must keep
// landing somewhere. reference-guides was the old library list; it now lands on the start links.
func TestPublishedDeepLinksStillResolve(t *testing.T) {
	ids := ownerIDs(t)
	printed := regexp.MustCompile(`https://`+regexp.QuoteMeta(siteHost)+`/([\w-]+\.html)(?:#([\w-]+))?`).FindAllStringSubmatch(readFile(t, filepath.Join(checkout, "internal", "cli", "help.go")), -1)
	if len(printed) < 5 {
		t.Errorf("found only %d links in the CLI help", len(printed))
	}
	for _, link := range printed {
		if ids[link[1]] == nil || (link[2] != "" && !ids[link[1]][link[2]]) {
			t.Errorf("the CLI help prints %s, which does not exist", link[0])
		}
	}
	for page, fragments := range map[string][]string{
		"docs.html":        {"idea", "install", "quickstart", "run", "sandbox", "secrets", "networking", "doctor", "toolchain", "credentials", "models", "peers", "presets", "forks", "loop", "parallel-forks", "evals", "editors", "mcp-remote", "reference-guides", "config", "troubleshooting"},
		"box.html":         {"project-services", "builds-and-network-modes"},
		"session-api.html": {"restart-and-recovery"},
		"agents.html":      {"instructions"},
	} {
		for _, fragment := range fragments {
			if !ids[page][fragment] {
				t.Errorf("%s#%s no longer exists", page, fragment)
			}
		}
	}
	for _, page := range []string{"usage-reports.html", "evals.html"} {
		if ids[page] == nil {
			t.Errorf("%s no longer exists", page)
		}
	}
}

// The overview summarizes; a reader who wants more should not have to hunt for where it continues.
func TestOverviewSectionsLinkTheirDepthPage(t *testing.T) {
	selfContained := []string{"idea", "quickstart"}
	sections := regexp.MustCompile(`(?s)<section class="doc-section" id="([\w-]+)">(.*?)</section>`).FindAllStringSubmatch(readSite(t, "docs.html"), -1)
	more := regexp.MustCompile(`<p class="doc-more"><a href="([\w-]+\.html)(?:#([\w-]+))?">`)
	ids := ownerIDs(t)
	if len(sections) < 20 {
		t.Fatalf("found %d sections", len(sections))
	}
	for _, section := range sections {
		if slices.Contains(selfContained, section[1]) {
			continue
		}
		link := more.FindStringSubmatch(section[2])
		if link == nil {
			t.Errorf("section %s does not say where its topic goes deeper", section[1])
			continue
		}
		if ids[link[1]] == nil || (link[2] != "" && !ids[link[1]][link[2]]) {
			t.Errorf("section %s links %s#%s, which does not exist", section[1], link[1], link[2])
		}
	}
}

// The README is a front door: it hands readers to the website, with its menu and search, instead of
// to the Markdown sources.
func TestReadmeSendsReadersToTheWebsite(t *testing.T) {
	readme := readFile(t, filepath.Join(checkout, "README.md"))
	ids := ownerIDs(t)
	for _, m := range regexp.MustCompile(`\]\(([^)\s]+)\)`).FindAllStringSubmatch(readme, -1) {
		target := m[1]
		if docsPageBySource(strings.Split(target, "#")[0]) != nil {
			t.Errorf("the README links %s; link the page on the website", target)
		}
		rest, onSite := strings.CutPrefix(target, "https://"+siteHost+"/")
		if !onSite || rest == "" || !strings.HasSuffix(strings.Split(rest, "#")[0], ".html") {
			continue
		}
		page, fragment, _ := strings.Cut(rest, "#")
		if ids[page] == nil || (fragment != "" && !ids[page][fragment]) {
			t.Errorf("the README links %s, which does not exist", target)
		}
	}
}

func TestDocsFolderMapListsEveryPage(t *testing.T) {
	folder := readFile(t, filepath.Join(checkout, "docs", "README.md"))
	for _, page := range docsPages {
		if !strings.Contains(folder, "["+page.Label+"](") {
			t.Errorf("docs/README.md leaves out %s", page.Label)
		}
	}
	for _, m := range regexp.MustCompile(`\]\(([^)\s]+\.md)\)`).FindAllStringSubmatch(folder, -1) {
		if _, err := os.Stat(filepath.Join(checkout, "docs", m[1])); err != nil {
			t.Errorf("docs/README.md links %s: %v", m[1], err)
		}
	}
}
