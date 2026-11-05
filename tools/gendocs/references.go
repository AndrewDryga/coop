package main

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	dom "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// docsPage is one page of the documentation.
type docsPage struct {
	Group, Slug, Label string
	// Parent nests the page under another in the menu. Nested pages are alternatives to their parent,
	// so previous and next step over them.
	Parent string
	// Source is the Markdown a generated page comes from. A page without one has a template of its own.
	Source, Description string
	// Keywords are words readers search for that the page itself doesn't use.
	Keywords string
}

// docsPages is the one map of the documentation, in reading order. It owns the docs menu, the
// previous and next links, the search index, the docs/ folder's map and the routes and summaries of
// pages generated from Markdown. Prose stays in the sources; a new page needs an entry here.
var docsPages = []docsPage{
	{Group: "Get started", Slug: "docs", Label: "Overview"},
	{Group: "Get started", Slug: "install", Label: "Install", Source: "docs/install.md", Description: "Install co:op, keep it up to date, and verify a download yourself."},
	{Group: "Get started", Slug: "sandbox-guide", Label: "First sandbox run"},
	{Group: "Get started", Slug: "claude-code-sandbox", Label: "Claude Code", Parent: "sandbox-guide"},
	{Group: "Get started", Slug: "codex-sandbox", Label: "Codex CLI", Parent: "sandbox-guide"},
	{Group: "Get started", Slug: "gemini-cli-sandbox", Label: "Gemini CLI", Parent: "sandbox-guide"},
	{Group: "Get started", Slug: "grok-cli-sandbox", Label: "Grok CLI", Parent: "sandbox-guide"},
	{Group: "Get started", Slug: "overnight-guide", Label: "First overnight run"},
	{Group: "Sandbox", Slug: "box", Label: "Files, images and services", Source: "docs/box.md", Description: "Control visible files, prepare a project image, and run databases and development servers beside your agent."},
	{Group: "Sandbox", Slug: "networking", Label: "Networking", Source: "docs/networking.md", Description: "Choose which network destinations an agent can reach, and review access before a run.", Keywords: "allowlist whitelist firewall"},
	{Group: "Agents", Slug: "agents", Label: "Accounts, models, presets", Source: "docs/agents.md", Description: "Sign in, choose models and accounts, and configure presets, teammates and instructions."},
	{Group: "Agents", Slug: "integrations", Label: "Editors and MCP", Source: "docs/integrations.md", Description: "Connect an editor or MCP server and check which actions run on your computer or in the container."},
	{Group: "Agents", Slug: "usage-reports", Label: "Usage and billing", Source: "docs/usage-reports.md", Description: "Connect your provider's billing reports and compare reported costs with local usage estimates."},
	{Group: "Hand off work", Slug: "loop", Label: "Task queues and loops", Source: "docs/loop.md", Description: "Set up a task queue for an agent to work through on its own, with decisions and a final review."},
	{Group: "Hand off work", Slug: "forks", Label: "Forks and review", Source: "docs/forks.md", Description: "Work in a separate copy of your project. Review the changes and checks before merging them."},
	{Group: "Hand off work", Slug: "evals", Label: "Evaluations", Source: "docs/evals.md", Description: "Run repeatable checks to compare changes to models, presets and loops. Keep the results for review."},
	{Group: "Reference", Slug: "cli", Label: "Commands", Source: "docs/cli.md", Description: "Look up co:op commands and options, with examples of how to use them."},
	{Group: "Reference", Slug: "configuration", Label: "Configuration", Source: "docs/configuration.md", Description: "Find project settings, environment variables, configuration files and exit codes."},
	{Group: "Reference", Slug: "session-api", Label: "Worker API", Source: "docs/session-api.md", Description: "Connect a worker and manage sessions, turns, evidence and publication through the worker API."},
	{Group: "Reference", Slug: "migrating", Label: "Upgrading", Source: "MIGRATING.md", Description: "What to change when you upgrade co:op, newest version first. Follow the steps for each version since yours."},
}

// projectLinks are the only documents the menu leaves the site for. They are about the project, not
// about using co:op, so they stay on GitHub.
var projectLinks = []struct{ Label, Href string }{
	{"Changelog", repoURL + "/blob/main/CHANGELOG.md"},
	{"Contributing", repoURL + "/blob/main/CONTRIBUTING.md"},
	{"Report an issue", repoURL + "/issues"},
}

const repoURL = "https://github.com/AndrewDryga/coop"

func docsPageBySlug(slug string) *docsPage {
	for i := range docsPages {
		if docsPages[i].Slug == slug {
			return &docsPages[i]
		}
	}
	return nil
}

func docsPageBySource(source string) *docsPage {
	for i := range docsPages {
		if source != "" && docsPages[i].Source == source {
			return &docsPages[i]
		}
	}
	return nil
}

// foldScript closes a disclosure below the width that shows it open, before the first paint, so a
// phone never sees the whole list flash past. Without JavaScript it simply stays open.
func foldScript(width string) string {
	return `<script>if(!matchMedia("(min-width:` + width + `)").matches)document.currentScript.parentElement.open=false</script>`
}

var openContents = regexp.MustCompile(`(<details class="docs-side" open>\s*<summary>[^<]*</summary>)(?:<script>.*?</script>)?`)

// foldContents gives every page's contents the same treatment; running it again changes nothing.
func foldContents(page string) string {
	return openContents.ReplaceAllString(page, "${1}"+foldScript("1280px"))
}

// docsNav is the menu every docs page shows, with its own entry marked. The search button appears
// before the first paint, so the menu doesn't shift, and only with JavaScript to run it.
func docsNav(current string) string {
	var b strings.Builder
	b.WriteString(`<div class="docs-rail">` + "\n")
	b.WriteString(`<button class="docs-search-open" type="button" data-docs-search hidden><svg class="icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><circle cx="11" cy="11" r="6.5"/><path d="M16 16l4.5 4.5"/></svg><span>Search</span><kbd aria-hidden="true"></kbd></button>` + "\n")
	b.WriteString(`<details class="docs-nav" open><summary>Docs menu</summary><script>document.documentElement.classList.add("docs-js");document.querySelector("[data-docs-search]").hidden=!window.HTMLDialogElement</script>` + foldScript("1024px") + "\n")
	b.WriteString(`<nav aria-label="Documentation">` + "\n")
	link := func(page docsPage) string {
		mark := ""
		if page.Slug == current {
			mark = ` aria-current="page"`
		}
		return fmt.Sprintf(`<a href="%s.html"%s>%s</a>`, page.Slug, mark, html.EscapeString(page.Label))
	}
	group := ""
	for _, page := range docsPages {
		if page.Parent != "" {
			continue
		}
		if page.Group != group {
			if group != "" {
				b.WriteString("</ul>\n")
			}
			group = page.Group
			fmt.Fprintf(&b, `<p class="docs-nav-group">%s</p>`+"\n"+`<ul role="list">`+"\n", html.EscapeString(group))
		}
		b.WriteString("<li>" + link(page))
		var nested []string
		for _, child := range docsPages {
			if child.Parent == page.Slug {
				nested = append(nested, "<li>"+link(child)+"</li>")
			}
		}
		if nested != nil {
			b.WriteString(`<ul role="list">` + strings.Join(nested, "") + "</ul>")
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n" + `<p class="docs-nav-group">Project</p>` + "\n" + `<ul role="list">` + "\n")
	for _, l := range projectLinks {
		fmt.Fprintf(&b, `<li><a href="%s">%s<span class="go" aria-hidden="true">↗</span></a></li>`+"\n", l.Href, html.EscapeString(l.Label))
	}
	b.WriteString("</ul>\n</nav>\n</details>\n</div>")
	return b.String()
}

// docsPager links the pages before and after this one in reading order. A nested page sits between
// its parent and whatever follows the parent.
func docsPager(slug string) string {
	var reading []docsPage
	for _, page := range docsPages {
		if page.Parent == "" {
			reading = append(reading, page)
		}
	}
	at := slices.IndexFunc(reading, func(p docsPage) bool { return p.Slug == slug })
	var previous, next *docsPage
	if parent := docsPageBySlug(slug).Parent; parent != "" {
		at = slices.IndexFunc(reading, func(p docsPage) bool { return p.Slug == parent })
		previous = &reading[at]
	} else if at > 0 {
		previous = &reading[at-1]
	}
	if at+1 < len(reading) {
		next = &reading[at+1]
	}
	var b strings.Builder
	b.WriteString(`<nav class="docs-pager" aria-label="Previous and next pages">`)
	if previous != nil {
		fmt.Fprintf(&b, `<a class="docs-pager-previous" href="%s.html"><span>Previous</span>%s</a>`, previous.Slug, html.EscapeString(previous.Label))
	}
	if next != nil {
		fmt.Fprintf(&b, `<a class="docs-pager-next" href="%s.html"><span>Next</span>%s</a>`, next.Slug, html.EscapeString(next.Label))
	}
	b.WriteString("</nav>")
	return b.String()
}

// docsChrome is what every docs template fills the same way.
func docsChrome(page docsPage) []block {
	eyebrow := `<p class="guide-eyebrow"><a href="docs.html">Documentation</a> / ` + html.EscapeString(page.Group) + `</p>`
	return []block{{"docs_nav", docsNav(page.Slug)}, {"eyebrow", eyebrow}, {"pager", docsPager(page.Slug)}}
}

// docsFolderMap is docs/README.md: the same map for someone browsing the repository.
func docsFolderMap() string {
	var b strings.Builder
	b.WriteString("# co:op documentation\n\n<!-- Generated by tools/gendocs from docsPages. Do NOT edit by hand; run: go run ./tools/gendocs -->\n\n")
	b.WriteString("The website has a menu and search: <https://" + siteHost + "/docs.html>. These Markdown files are its source.\n")
	group := ""
	for _, page := range docsPages {
		if page.Group != group {
			group = page.Group
			b.WriteString("\n## " + group + "\n\n")
		}
		target := "https://" + siteHost + "/" + page.Slug + ".html"
		if page.Source != "" {
			target = strings.TrimPrefix(page.Source, "docs/")
			if !strings.Contains(page.Source, "/") {
				target = "../" + page.Source
			}
		}
		indent := ""
		if page.Parent != "" {
			indent = "  "
		}
		fmt.Fprintf(&b, "%s- [%s](%s)\n", indent, page.Label, target)
	}
	return b.String()
}

func referencePages(root string, generated map[string]string, version string) (map[string]string, error) {
	outputs, err := filepath.Glob(filepath.Join(root, "site", "*.html"))
	if err != nil {
		return nil, err
	}
	for _, output := range outputs {
		data, err := os.ReadFile(output)
		if err != nil {
			return nil, err
		}
		if bytes.Contains(data, []byte("<!-- Generated public reference by tools/gendocs.")) && docsPageBySlug(strings.TrimSuffix(filepath.Base(output), ".html")) == nil {
			return nil, fmt.Errorf("unexpected generated reference %s; remove the retired page", output)
		}
	}
	sources, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		name := filepath.Base(source)
		if name != "README.md" && docsPageBySource("docs/"+name) == nil {
			return nil, fmt.Errorf("%s has no public reference entry", source)
		}
	}
	pages := make(map[string]string)
	for _, ref := range docsPages {
		if ref.Source == "" {
			continue
		}
		sourcePath := filepath.FromSlash(ref.Source)
		source, ok := generated[sourcePath]
		if !ok {
			data, err := os.ReadFile(filepath.Join(root, sourcePath))
			if err != nil {
				return nil, err
			}
			source = string(data)
		}
		title, content, toc, err := renderReference(ref, source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sourcePath, err)
		}
		page, err := fillTemplate(root, "reference.tpl.html", append(docsChrome(ref), []block{
			{"title", html.EscapeString(title)}, {"description", html.EscapeString(ref.Description)},
			{"slug", ref.Slug}, {"content", content}, {"toc", toc},
			{"source_path", ref.Source}, {"source_url", repoURL + "/blob/main/" + ref.Source},
		}...))
		if err != nil {
			return nil, err
		}
		pages[filepath.Join("site", ref.Slug+".html")] = stampAssets(externalLinks(typographic(foldContents(page))), version)
	}
	return pages, nil
}

func renderReference(ref docsPage, source string) (title, content, toc string, err error) {
	var rendered bytes.Buffer
	source = copyFence.ReplaceAllString(source, "```$1")
	if err = goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert([]byte(source), &rendered); err != nil {
		return
	}
	root := &dom.Node{Type: dom.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, parseErr := dom.ParseFragment(&rendered, root)
	if parseErr != nil {
		err = parseErr
		return
	}
	for _, node := range nodes {
		root.AppendChild(node)
	}
	if ref.Slug == "cli" {
		splitCLIReference(root)
	}
	breakAfterUnderscores(root)
	ids := map[string]bool{"main": true, "overview": true}
	heading := "Overview"
	walkDOM(root, func(node *dom.Node) {
		if node.Type != dom.ElementNode {
			return
		}
		if node.Data == "h1" {
			title = nodeText(node)
			return
		}
		if slices.Contains([]string{"h2", "h3", "h4", "h5", "h6"}, node.Data) {
			heading = nodeText(node)
			id := uniqueHeadingID(heading, ids)
			setDOMAttr(node, "id", id)
			if node.Data == "h2" || node.Data == "h3" {
				class := ""
				if node.Data == "h3" {
					class = ` class="reference-subsection"`
				}
				label := heading
				if ref.Slug == "cli" {
					// the command alone; its heading carries the description
					label, _, _ = strings.Cut(heading, ": ")
				}
				toc += `<li` + class + `><a href="#` + html.EscapeString(id) + `">` + html.EscapeString(label) + `</a></li>` + "\n"
			}
		}
		if node.Data == "a" {
			if href := domAttr(node, "href"); href != "" {
				setDOMAttr(node, "href", referenceLink(ref.Source, href))
			}
		}
		if node.Data == "table" {
			columns := 0
			walkDOM(node, func(child *dom.Node) {
				if child.Data == "th" {
					columns++
				}
			})
			class := "ref"
			if columns > 2 {
				class += " ref-wide"
			}
			setDOMAttr(node, "class", class)
			frame := elementNode("div", "class", "reference-table")
			wrapper := elementNode("div", "class", "table-scroll", "role", "region", "tabindex", "0", "aria-label", heading+" table")
			node.Parent.InsertBefore(frame, node)
			node.Parent.RemoveChild(node)
			frame.AppendChild(wrapper)
			wrapper.AppendChild(node)
			frame.AppendChild(scrollHint())
		}
		if node.Data == "th" {
			setDOMAttr(node, "scope", "col")
		}
		if node.Data == "pre" {
			styleReferenceCode(node, heading)
		}
	})
	if title == "" {
		err = fmt.Errorf("missing document title")
		return
	}
	// Group real H2 sections for shared spacing and active-contents tracking. Subheadings
	// keep their own IDs; comments and code that look like Markdown headings stay verbatim.
	article := elementNode("div")
	section := elementNode("section", "class", "doc-section reference-section", "id", "overview")
	overview := elementNode("h2", "class", "sr-only")
	overview.AppendChild(&dom.Node{Type: dom.TextNode, Data: "Overview"})
	section.AppendChild(overview)
	article.AppendChild(section)
	for node := root.FirstChild; node != nil; {
		next := node.NextSibling
		root.RemoveChild(node)
		if node.Type == dom.ElementNode && node.Data == "h1" {
			node = next
			continue
		}
		if node.Type == dom.ElementNode && node.Data == "h2" {
			id := domAttr(node, "id")
			removeDOMAttr(node, "id")
			section = elementNode("section", "class", "doc-section reference-section", "id", id)
			article.AppendChild(section)
		}
		section.AppendChild(node)
		node = next
	}
	// A document that opens with a heading has no overview to show or to list.
	if blank := article.FirstChild; hasOnlyItsHeading(blank) {
		article.RemoveChild(blank)
	} else {
		toc = `<li><a href="#overview">Overview</a></li>` + "\n" + toc
	}
	var output bytes.Buffer
	for node := article.FirstChild; node != nil; node = node.NextSibling {
		if err = dom.Render(&output, node); err != nil {
			return
		}
	}
	content = output.String()
	return
}

func hasOnlyItsHeading(section *dom.Node) bool {
	for child := section.FirstChild.NextSibling; child != nil; child = child.NextSibling {
		if child.Type != dom.TextNode || strings.TrimSpace(child.Data) != "" {
			return false
		}
	}
	return true
}

// The CLI manual has explicit command separators, not Markdown headings. Split only
// those boundaries, preserving each help body exactly; never parse headings from examples.
func splitCLIReference(root *dom.Node) {
	for node := root.FirstChild; node != nil; node = node.NextSibling {
		if node.Data != "pre" {
			continue
		}
		parts := strings.Split(nodeText(node), "\n\n"+strings.Repeat("=", 78)+"\n\n")
		if len(parts) < 2 {
			return
		}
		for i, part := range parts {
			if i > 0 {
				line, rest, ok := strings.Cut(part, "\n\n")
				if !ok {
					return
				}
				h := elementNode("h2")
				h.AppendChild(&dom.Node{Type: dom.TextNode, Data: line})
				root.InsertBefore(h, node)
				part = rest
			}
			pre := elementNode("pre")
			code := elementNode("code")
			code.AppendChild(&dom.Node{Type: dom.TextNode, Data: part})
			pre.AppendChild(code)
			root.InsertBefore(pre, node)
		}
		root.RemoveChild(node)
		return
	}
}

// copyFence is a shell fence its author marks as one command to copy, as in ```bash copy. Plain shell
// fences can mix alternatives and destructive steps, so they never get a Copy button. GitHub reads
// the first word as the language, so the mark doesn't change how the file looks there.
var copyFence = regexp.MustCompile("(?m)^```(?:bash|sh) (copy(?:-install)?)$")

func styleReferenceCode(pre *dom.Node, heading string) {
	setDOMAttr(pre, "class", "code")
	setDOMAttr(pre, "role", "region")
	setDOMAttr(pre, "aria-label", heading+" code example")
	setDOMAttr(pre, "tabindex", "0")
	code := pre.FirstChild
	if code == nil {
		return
	}
	language := strings.TrimPrefix(domAttr(code, "class"), "language-")
	kind, label := "", ""
	switch language {
	case "bash", "sh", "shell", "zsh":
		label = "Shell examples"
	case "copy":
		kind, label = "command", "Shell"
	case "copy-install":
		kind, label = "install", "Shell"
	case "yaml", "yml":
		kind, label = "configuration", "YAML"
	case "json":
		kind, label = "configuration", "JSON"
	case "toml":
		kind, label = "configuration", "TOML"
	case "dockerfile", "Dockerfile":
		kind, label = "configuration", "Dockerfile"
	}
	class := "reference-code-frame"
	// Reference fences can mix alternatives, review and destructive commands. A
	// shell language tag is not permission to copy that whole fence as one recipe.
	if kind != "" {
		class += " code-file"
	} else if label != "" {
		class += " reference-example"
	}
	wrapper := elementNode("div", "class", class)
	if label != "" {
		name := elementNode("p", "class", "code-file-name")
		name.AppendChild(&dom.Node{Type: dom.TextNode, Data: label})
		wrapper.AppendChild(name)
	}
	pre.Parent.InsertBefore(wrapper, pre)
	pre.Parent.RemoveChild(pre)
	wrapper.AppendChild(pre)
	wrapper.AppendChild(scrollHint())
	if kind != "" {
		setDOMAttr(wrapper, "data-copy-kind", kind)
		setDOMAttr(pre, "data-copy-source", "")
	}
}

func scrollHint() *dom.Node {
	hint := elementNode("p", "class", "reference-scroll-hint")
	hint.AppendChild(&dom.Node{Type: dom.TextNode, Data: "Scroll horizontally to see more →"})
	return hint
}

func uniqueHeadingID(title string, used map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-':
			b.WriteRune(r)
		}
	}
	base := b.String()
	if base == "" {
		base = "section"
	}
	id := base
	for n := 1; used[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	used[id] = true
	return id
}

// referenceLink resolves a Markdown link from source: to the published page when there is one, else
// to the repository. Only source code and project files stay there.
func referenceLink(source, href string) string {
	u, err := url.Parse(href)
	if err != nil || strings.HasPrefix(href, "#") || u.Scheme != "" || u.Host != "" {
		return href
	}
	if u.Path == "" {
		return href
	}
	target := path.Clean(path.Join(path.Dir(source), u.Path))
	if page := docsPageBySource(target); page != nil {
		u.Path = page.Slug + ".html"
		return u.String()
	}
	// Directory links use GitHub's tree view.
	kind := "blob"
	if strings.HasSuffix(u.Path, "/") {
		kind = "tree"
	}
	repo, err := url.Parse(repoURL)
	if err != nil {
		return href
	}
	u.Scheme, u.Host, u.Path = repo.Scheme, repo.Host, repo.Path+"/"+kind+"/main/"+target
	return u.String()
}

func siteMap(pages map[string]string) string {
	var names []string
	for name := range pages {
		if filepath.Ext(name) != ".html" {
			continue
		}
		name = filepath.Base(name)
		if name == "index.html" {
			name = ""
		}
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\">\n")
	for _, name := range names {
		fmt.Fprintf(&b, "  <url><loc>https://%s/%s</loc></url>\n", siteHost, name)
	}
	b.WriteString("</urlset>\n")
	return b.String()
}

func elementNode(tag string, attrs ...string) *dom.Node {
	node := &dom.Node{Type: dom.ElementNode, Data: tag, DataAtom: atom.Lookup([]byte(tag))}
	for i := 0; i < len(attrs); i += 2 {
		setDOMAttr(node, attrs[i], attrs[i+1])
	}
	return node
}
func domAttr(node *dom.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}
func setDOMAttr(node *dom.Node, key, value string) {
	for i := range node.Attr {
		if node.Attr[i].Key == key {
			node.Attr[i].Val = value
			return
		}
	}
	node.Attr = append(node.Attr, dom.Attribute{Key: key, Val: value})
}
func removeDOMAttr(node *dom.Node, key string) {
	node.Attr = slices.DeleteFunc(node.Attr, func(a dom.Attribute) bool { return a.Key == key })
}
func nodeText(node *dom.Node) string {
	if node.Type == dom.TextNode {
		return node.Data
	}
	var b strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(nodeText(child))
	}
	return b.String()
}
func walkDOM(node *dom.Node, visit func(*dom.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		walkDOM(child, visit)
		child = next
	}
}

// breakAfterUnderscores lets a long name in a table cell, such as COOP_ALLOW_CASE_EXPOSED_SECRETS,
// wrap only after an underscore. A <wbr> adds no character, so the copied name stays exact.
func breakAfterUnderscores(root *dom.Node) {
	var codes []*dom.Node
	walkDOM(root, func(node *dom.Node) {
		if node.Type == dom.ElementNode && node.Data == "code" && insideTableCell(node) {
			codes = append(codes, node)
		}
	})
	for _, code := range codes {
		for text := code.FirstChild; text != nil; {
			next := text.NextSibling
			if text.Type == dom.TextNode && strings.Contains(text.Data, "_") {
				parts := strings.SplitAfter(text.Data, "_")
				for i, part := range parts {
					if part == "" {
						continue
					}
					code.InsertBefore(&dom.Node{Type: dom.TextNode, Data: part}, text)
					if i < len(parts)-1 && strings.HasSuffix(part, "_") {
						code.InsertBefore(&dom.Node{Type: dom.ElementNode, Data: "wbr", DataAtom: atom.Wbr}, text)
					}
				}
				code.RemoveChild(text)
			}
			text = next
		}
	}
}

func insideTableCell(node *dom.Node) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == dom.ElementNode && (parent.Data == "td" || parent.Data == "th") {
			return true
		}
	}
	return false
}
