package main

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	dom "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type referencePage struct {
	Slug, Label, Description string
	Related                  []string
}

// One inventory owns routes, summaries, the library and related-page links. The prose
// remains in docs/*.md; additions there require an explicit publication decision here.
var referenceLibrary = []referencePage{
	{"networking", "Networking", "Choose which network destinations an agent can reach, and review access before a run.", []string{"box", "agents", "integrations"}},
	{"box", "Sandbox, images and services", "Control visible files, prepare a project image, and run databases and development servers beside your agent.", []string{"networking", "configuration", "agents"}},
	{"agents", "Agents, accounts and presets", "Sign in, choose models and accounts, and configure presets, teammates and instructions.", []string{"networking", "usage-reports", "loop"}},
	{"forks", "Forks and review", "Work in a separate copy of your project. Review the changes and checks before merging them.", []string{"loop", "box", "configuration"}},
	{"loop", "Task queues and loops", "Set up a task queue for an agent to work through on its own, with decisions and a final review.", []string{"forks", "agents", "configuration"}},
	{"integrations", "Editors and MCP", "Connect an editor or MCP server and check which actions run on your computer or in the container.", []string{"networking", "agents", "session-api"}},
	{"configuration", "Configuration", "Find project settings, environment variables, configuration files and exit codes.", []string{"box", "networking", "cli"}},
	{"usage-reports", "Usage and billing reports", "Connect your provider's billing reports and compare reported costs with local usage estimates.", []string{"agents", "configuration", "cli"}},
	{"evals", "Evaluations", "Run repeatable checks to compare changes to models, presets and loops. Keep the results for review.", []string{"agents", "loop", "cli"}},
	{"session-api", "Worker API", "Connect a worker and manage sessions, turns, evidence and publication through the worker API.", []string{"integrations", "networking", "forks"}},
	{"cli", "Command reference", "Look up co:op commands and options, with examples of how to use them.", []string{"configuration", "agents", "networking"}},
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
		if bytes.Contains(data, []byte("<!-- Generated public reference by tools/gendocs.")) && referenceBySlug(strings.TrimSuffix(filepath.Base(output), ".html")) == nil {
			return nil, fmt.Errorf("unexpected generated reference %s; remove the retired page", output)
		}
	}
	sources, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		slug := strings.TrimSuffix(filepath.Base(source), ".md")
		if referenceBySlug(slug) == nil {
			return nil, fmt.Errorf("%s has no public reference entry", source)
		}
	}
	pages := make(map[string]string)
	for _, ref := range referenceLibrary {
		sourcePath := filepath.Join("docs", ref.Slug+".md")
		source, ok := generated[sourcePath]
		if !ok {
			data, err := os.ReadFile(filepath.Join(root, sourcePath))
			if err != nil {
				return nil, err
			}
			source = string(data)
		}
		title, content, toc, err := renderReference(ref.Slug, source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sourcePath, err)
		}
		page, err := fillTemplate(root, "reference.tpl.html", []block{
			{"title", html.EscapeString(title)}, {"description", html.EscapeString(ref.Description)},
			{"slug", ref.Slug}, {"content", content}, {"toc", toc}, {"related", relatedReferences(ref)},
		})
		if err != nil {
			return nil, err
		}
		pages[filepath.Join("site", ref.Slug+".html")] = stampAssets(externalLinks(typographic(page)), version)
	}
	return pages, nil
}

func referenceBySlug(slug string) *referencePage {
	for i := range referenceLibrary {
		if referenceLibrary[i].Slug == slug {
			return &referenceLibrary[i]
		}
	}
	return nil
}

func renderReference(slug, source string) (title, content, toc string, err error) {
	var rendered bytes.Buffer
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
	if slug == "cli" {
		splitCLIReference(root)
	}
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
				toc += `<li` + class + `><a href="#` + html.EscapeString(id) + `">` + html.EscapeString(heading) + `</a></li>` + "\n"
			}
		}
		if node.Data == "a" {
			if href := domAttr(node, "href"); href != "" {
				setDOMAttr(node, "href", referenceLink(slug, href))
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
	toc = `<li><a href="#overview">Overview</a></li>` + "\n" + toc
	var output bytes.Buffer
	for node := article.FirstChild; node != nil; node = node.NextSibling {
		if err = dom.Render(&output, node); err != nil {
			return
		}
	}
	content = output.String()
	return
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

func referenceLink(slug, href string) string {
	u, err := url.Parse(href)
	if err != nil || strings.HasPrefix(href, "#") || u.Scheme != "" || u.Host != "" {
		return href
	}
	target := path.Clean(path.Join("docs", u.Path))
	if u.Path == "" {
		return href
	}
	if strings.HasPrefix(target, "docs/") && strings.HasSuffix(target, ".md") {
		name := strings.TrimSuffix(strings.TrimPrefix(target, "docs/"), ".md")
		if referenceBySlug(name) != nil {
			u.Path = name + ".html"
			return u.String()
		}
	}
	// Source-only material stays in the repository. Directory links use GitHub's tree view.
	kind := "blob"
	if strings.HasSuffix(u.Path, "/") {
		kind = "tree"
	}
	u.Scheme, u.Host, u.Path = "https", "github.com", "/AndrewDryga/coop/"+kind+"/main/"+target
	return u.String()
}

func relatedReferences(ref referencePage) string {
	var b strings.Builder
	for _, slug := range ref.Related {
		related := referenceBySlug(slug)
		if related == nil {
			continue
		}
		fmt.Fprintf(&b, `<li><a href="%s.html">%s</a><p>%s</p></li>`+"\n", related.Slug, html.EscapeString(related.Label), html.EscapeString(related.Description))
	}
	return b.String()
}

func referenceIndex() string {
	var b strings.Builder
	for _, ref := range referenceLibrary {
		fmt.Fprintf(&b, `<li><a href="%s.html">%s</a><p>%s</p></li>`+"\n", ref.Slug, html.EscapeString(ref.Label), html.EscapeString(ref.Description))
	}
	return b.String()
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
