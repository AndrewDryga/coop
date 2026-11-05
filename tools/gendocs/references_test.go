package main

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dom "golang.org/x/net/html"
)

func TestReferenceMarkdownPreservesStructureAndCode(t *testing.T) {
	source := "# Reference\n\nIntro with [network rules](networking.md#provider-access).\n\n## Your own paths: `.coopignore`\n\n| Rule | Result |\n| --- | --- |\n| `*.example.com` | <unsafe> |\n\n```yaml\n# not a heading\npath: \"a & b < c\"\n  nested: 'quoted'\n```\n\n    transcript: denied\n    # also not a heading\n\n### Repeated\n\nText.\n\n### Repeated\n\nOther.\n\n<script>alert(1)</script>\n"
	title, content, toc, err := renderReference(docsPage{Slug: "box", Source: "docs/box.md"}, source)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Reference" {
		t.Fatalf("title %q", title)
	}
	for _, want := range []string{`href="networking.html#provider-access"`, `id="your-own-paths-coopignore"`, `id="repeated-1"`, `class="table-scroll"`, `scope="col"`, `data-copy-kind="configuration"`} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(toc, "not a heading") || strings.Contains(content, "<script>") {
		t.Fatal("code/raw HTML became active document content")
	}
	parsed, err := dom.Parse(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	walkDOM(parsed, func(n *dom.Node) {
		if n.Data == "pre" {
			codes = append(codes, nodeText(n))
		}
	})
	if len(codes) != 2 || codes[0] != "# not a heading\npath: \"a & b < c\"\n  nested: 'quoted'\n" || codes[1] != "transcript: denied\n# also not a heading\n" {
		t.Fatalf("code changed: %q", codes)
	}
}

func TestReferenceLinksKeepDocumentAndRepositoryDestinations(t *testing.T) {
	for href, want := range map[string]string{
		"configuration.md?view=full#project-settings-agentprojectyaml": "configuration.html?view=full#project-settings-agentprojectyaml",
		"#authentication":                 "#authentication",
		"../MIGRATING.md":                 "migrating.html",
		"install.md#verifying-a-download": "install.html#verifying-a-download",
		"../README.md#license":            "https://github.com/AndrewDryga/coop/blob/main/README.md#license",
		"../examples/evals/maintenance/":  "https://github.com/AndrewDryga/coop/tree/main/examples/evals/maintenance",
		"../internal/workerproto/job.go":  "https://github.com/AndrewDryga/coop/blob/main/internal/workerproto/job.go",
		"https://example.com/a.md#b":      "https://example.com/a.md#b",
	} {
		if got := referenceLink("docs/agents.md", href); got != want {
			t.Errorf("%s => %s, want %s", href, got, want)
		}
	}
}

func referenceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"docs", "site", "tools/site"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	template, err := os.ReadFile(filepath.Join(checkout, "tools/site/reference.tpl.html"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "tools/site/reference.tpl.html"), template, 0644); err != nil {
		t.Fatal(err)
	}
	for _, ref := range docsPages {
		if ref.Source == "" {
			continue
		}
		if err = os.WriteFile(filepath.Join(root, filepath.FromSlash(ref.Source)), []byte("# "+ref.Label+"\n\nDisk content.\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReferenceGenerationUsesFreshCLIAndRejectsUnownedSources(t *testing.T) {
	root := referenceFixture(t)
	generated := map[string]string{filepath.Join("docs", "cli.md"): "# Fresh command reference\n\nFresh in-memory help.\n"}
	pages, err := referencePages(root, generated, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if page := pages[filepath.Join("site", "cli.html")]; !strings.Contains(page, "Fresh in-memory help.") || strings.Contains(page, "Disk content.") {
		t.Fatal("website rendered stale on-disk CLI")
	}
	again, err := referencePages(root, generated, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for name, page := range pages {
		if again[name] != page {
			t.Fatalf("nondeterministic %s", name)
		}
	}
	unknown := filepath.Join(root, "docs/new-topic.md")
	if err = os.WriteFile(unknown, []byte("# New"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = referencePages(root, generated, "fixture"); err == nil || !strings.Contains(err.Error(), "no public reference entry") {
		t.Fatalf("unowned source ignored: %v", err)
	}
	if err = os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "site/retired.html"), []byte("<!-- Generated public reference by tools/gendocs. -->"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = referencePages(root, generated, "fixture"); err == nil || !strings.Contains(err.Error(), "unexpected generated reference") {
		t.Fatalf("stale output ignored: %v", err)
	}
}

func TestCLIReferenceSectionsPreserveHelpBodies(t *testing.T) {
	manual := "coop: synopsis\n\n" + strings.Repeat("=", 78) + "\n\ncoop run: run a command\n\nUsage:\n  coop run -- echo '# example'\n"
	_, content, toc, err := renderReference(docsPage{Slug: "cli", Source: "docs/cli.md"}, "# CLI\n\n```\n"+manual+"```\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toc, ">coop run</a>") {
		t.Fatal("command missing from contents")
	}
	parsed, err := dom.Parse(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	walkDOM(parsed, func(n *dom.Node) {
		if n.Data == "pre" {
			blocks = append(blocks, nodeText(n))
		}
	})
	if len(blocks) != 2 || blocks[1] != "Usage:\n  coop run -- echo '# example'\n" {
		t.Fatalf("help body changed: %q", blocks)
	}
}

// Validate fragments too: an existing file alone is not proof that a rewritten
// Markdown link or a generated contents entry lands on the requested section.
func TestSiteLinksResolveToSections(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(checkout, "site", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]parsedPage{}
	ids := map[string]map[string]bool{}
	for _, file := range files {
		name := filepath.Base(file)
		pages[name] = parsePage(t, readFile(t, file))
		ids[name] = map[string]bool{}
		for _, el := range pages[name].elements {
			if id := el.attrs["id"]; id != "" {
				if ids[name][id] {
					t.Errorf("%s: duplicate id %s", name, id)
				}
				ids[name][id] = true
			}
		}
	}
	base, _ := url.Parse("https://" + siteHost + "/")
	for name, page := range pages {
		from := base.ResolveReference(&url.URL{Path: name})
		for _, el := range page.elements {
			if el.tag != "a" || el.attrs["href"] == "" {
				continue
			}
			u, err := url.Parse(el.attrs["href"])
			if err != nil {
				t.Error(err)
				continue
			}
			u = from.ResolveReference(u)
			if u.Host != siteHost || u.Fragment == "" {
				continue
			}
			target := strings.TrimPrefix(u.Path, "/")
			if target == "" {
				target = "index.html"
			}
			if !ids[target][u.Fragment] {
				t.Errorf("%s: broken section link %s", name, el.attrs["href"])
			}
		}
	}
}

func TestReferencePagesHaveMetadataNavigationAndSource(t *testing.T) {
	for _, ref := range docsPages {
		if ref.Source == "" {
			continue
		}
		t.Run(ref.Slug, func(t *testing.T) {
			markup := readSite(t, ref.Slug+".html")
			page := parsePage(t, markup)
			if page.count(func(e element) bool { return e.tag == "h1" }) != 1 {
				t.Error("expected one h1")
			}
			for _, text := range []string{`href="https://` + siteHost + `/` + ref.Slug + `.html"`, `<p class="guide-eyebrow"><a href="docs.html">Documentation</a> / ` + ref.Group + `</p>`, `href="` + repoURL + `/blob/main/` + ref.Source + `"`} {
				if !strings.Contains(markup, text) {
					t.Errorf("missing %s", text)
				}
			}
			for _, el := range page.elements {
				if el.tag == "a" && strings.HasSuffix(strings.Split(el.attrs["href"], "#")[0], ".md") && !offSite(el.attrs["href"]) {
					t.Errorf("unrewritten link %s", el.attrs["href"])
				}
			}
		})
	}
}

func TestReferenceCheckIsReadOnly(t *testing.T) {
	root := referenceFixture(t)
	source := filepath.Join(root, "docs/networking.md")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = referencePages(root, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("render mutated source")
	}
	outputs, _ := filepath.Glob(filepath.Join(root, "site", "*"))
	if len(outputs) != 0 {
		t.Fatalf("render wrote outputs: %v", outputs)
	}
}

func TestShellExamplesAreNotOneClickRecipes(t *testing.T) {
	_, content, _, err := renderReference(docsPage{Slug: "forks", Source: "docs/forks.md"}, "# Forks\n\n```bash\ncoop fork review work\ncoop fork merge work --yes\ncoop fork rm work\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "data-copy-source") {
		t.Fatal("a mixed shell example became a one-click recipe")
	}
	if !strings.Contains(content, "Shell examples") || !strings.Contains(content, "coop fork merge work --yes") {
		t.Fatal("read-only example lost its label or source")
	}
}

func TestCommandContentsNameTheCommandAlone(t *testing.T) {
	manual := "coop: run agents\n\n" + strings.Repeat("=", 78) + "\n\ncoop run: run a command in the box\n\nUsage: coop run\n"
	_, content, toc, err := renderReference(docsPage{Slug: "cli", Source: "docs/cli.md"}, "# CLI\n\n```\n"+manual+"```\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toc, `>coop run</a>`) || strings.Contains(toc, "run a command in the box") {
		t.Errorf("contents: %s", toc)
	}
	if !strings.Contains(content, "coop run: run a command in the box</h2>") {
		t.Errorf("the heading lost its description: %s", content)
	}
}

func TestADocumentOpeningWithAHeadingHasNoEmptyOverview(t *testing.T) {
	_, content, toc, err := renderReference(docsPage{Slug: "migrating", Source: "MIGRATING.md"}, "# Migrating\n\n## v2\n\nSteps.\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, `id="overview"`) || strings.Contains(toc, "Overview") {
		t.Errorf("an empty overview: %s\n%s", content, toc)
	}
	_, content, toc, err = renderReference(docsPage{Slug: "box", Source: "docs/box.md"}, "# Box\n\nAn intro.\n\n## One\n\nText.\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `id="overview"`) || !strings.Contains(toc, "Overview") {
		t.Errorf("the intro lost its overview: %s\n%s", content, toc)
	}
}

func TestOnlyAMarkedShellFenceCopiesAsOneCommand(t *testing.T) {
	_, content, _, err := renderReference(docsPage{Slug: "install", Source: "docs/install.md"}, "# Install\n\n```bash copy-install\ncurl -fsSL https://x/install.sh | sh\n```\n\n```bash copy\ncoop doctor\n```\n\n```bash\ncoop down --delete-volumes\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-copy-kind="install"`, `data-copy-kind="command"`} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %s in %s", want, content)
		}
	}
	if strings.Count(content, "data-copy-source") != 2 {
		t.Errorf("want Copy on the two marked fences only: %s", content)
	}
	if _, plain, ok := strings.Cut(content, "Shell examples"); !ok || strings.Contains(plain, "data-copy-source") {
		t.Errorf("a plain shell fence got a Copy button: %s", content)
	}
}

func TestReferenceTemplateTreatsMarkdownPlaceholdersAsLiteral(t *testing.T) {
	root := referenceFixture(t)
	generated := map[string]string{filepath.Join("docs", "networking.md"): "# Networking\n\n```yaml\nvalue: '{{toc}} {{unlisted}}'\n```\n"}
	pages, err := referencePages(root, generated, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pages[filepath.Join("site", "networking.html")], "{{toc}} {{unlisted}}") {
		t.Fatal("template interpreted code as placeholders")
	}
}

func TestFullCLIReferenceRetainsEveryCommandBody(t *testing.T) {
	source := readFile(t, filepath.Join(checkout, "docs", "cli.md"))
	_, manual, ok := strings.Cut(source, "```\n")
	if !ok {
		t.Fatal("missing generated help fence")
	}
	manual = strings.TrimSuffix(manual, "```\n")
	parts := strings.Split(manual, "\n\n"+strings.Repeat("=", 78)+"\n\n")
	if len(parts) < 20 {
		t.Fatal("manual command separator changed")
	}
	_, content, _, err := renderReference(docsPage{Slug: "cli", Source: "docs/cli.md"}, source)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := dom.Parse(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	var bodies, headings []string
	walkDOM(doc, func(n *dom.Node) {
		if n.Data == "pre" {
			bodies = append(bodies, nodeText(n))
		}
		if n.Data == "h2" && domAttr(n, "class") != "sr-only" {
			headings = append(headings, nodeText(n))
		}
	})
	if len(bodies) != len(parts) || len(headings) != len(parts)-1 {
		t.Fatalf("manual sections %d, rendered %d bodies and %d headings", len(parts), len(bodies), len(headings))
	}
	for i, part := range parts {
		if i > 0 {
			line, rest, ok := strings.Cut(part, "\n\n")
			if !ok || headings[i-1] != line {
				t.Fatalf("heading %d changed", i)
			}
			part = rest
		}
		if bodies[i] != part {
			t.Errorf("help body %d changed", i)
		}
	}
}
