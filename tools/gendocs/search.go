package main

// The docs' search index, site/search-index.json. Search runs in the reader's browser with no outside
// service: site.js fetches this file the first time someone opens search. An entry is one section,
// so a match links straight to it. The index is generated from the finished pages, so it can't list
// a heading the reader won't find.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	dom "golang.org/x/net/html"
)

type searchEntry struct {
	URL      string `json:"url"`
	Page     string `json:"page"`
	Heading  string `json:"heading"`
	Text     string `json:"text"`
	Keywords string `json:"keywords,omitempty"`
	// Variant marks a walkthrough that repeats its parent's steps for one agent.
	Variant bool `json:"variant,omitempty"`
}

// One entry per line, so a change to one section is a one-line diff.
func searchIndex(pages map[string]string) (string, error) {
	var b strings.Builder
	b.WriteString("[\n")
	first := true
	for _, page := range docsPages {
		markup, ok := pages[filepath.Join("site", page.Slug+".html")]
		if !ok {
			return "", fmt.Errorf("docsPages lists %s, and nothing generates it", page.Slug)
		}
		entries, err := searchEntries(page, markup)
		if err != nil {
			return "", fmt.Errorf("%s: %w", page.Slug, err)
		}
		for _, entry := range entries {
			var line bytes.Buffer
			encoder := json.NewEncoder(&line)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(entry); err != nil {
				return "", err
			}
			if !first {
				b.WriteString(",\n")
			}
			first = false
			b.WriteString(strings.TrimSuffix(line.String(), "\n"))
		}
	}
	b.WriteString("\n]\n")
	return b.String(), nil
}

// inlineTags continue the sentence around them. Anything else separates words.
var inlineTags = []string{"a", "abbr", "b", "code", "em", "i", "kbd", "small", "span", "strong", "wbr"}

// leftOutOfSearch holds what a reader doesn't search for: navigation, repeated labels and the
// terminal replays, whose output would match nearly every word.
func leftOutOfSearch(node *dom.Node) bool {
	if slices.Contains([]string{"script", "style", "svg", "button", "nav", "samp"}, node.Data) || domAttr(node, "aria-hidden") == "true" {
		return true
	}
	classes := strings.Fields(domAttr(node, "class"))
	for _, class := range []string{"docs-side", "window", "guide-eyebrow", "reference-scroll-hint", "reference-source", "sr-only"} {
		if slices.Contains(classes, class) {
			return true
		}
	}
	return slices.Contains(classes, "code-file-name") && slices.Contains(strings.Fields(domAttr(node.Parent, "class")), "reference-code-frame")
}

func searchEntries(page docsPage, markup string) ([]searchEntry, error) {
	document, err := dom.Parse(strings.NewReader(markup))
	if err != nil {
		return nil, err
	}
	var main *dom.Node
	walkDOM(document, func(node *dom.Node) {
		if node.Type == dom.ElementNode && node.Data == "main" {
			main = node
		}
	})
	if main == nil {
		return nil, fmt.Errorf("no <main>")
	}
	variant := page.Parent != ""
	entries := []searchEntry{{URL: page.Slug + ".html", Page: page.Label, Heading: page.Label, Keywords: page.Keywords, Variant: variant}}
	var text strings.Builder
	finish := func() {
		entries[len(entries)-1].Text = strings.Join(strings.Fields(text.String()), " ")
		text.Reset()
	}
	var read func(node *dom.Node)
	read = func(node *dom.Node) {
		if node.Type == dom.TextNode {
			text.WriteString(node.Data)
			return
		}
		if node.Type == dom.ElementNode {
			if leftOutOfSearch(node) {
				return
			}
			if node.Data == "h1" {
				entries[0].Heading = strings.TrimSuffix(headingText(node), ".")
				return
			}
			if slices.Contains([]string{"h2", "h3", "h4"}, node.Data) {
				id := domAttr(node, "id")
				if id == "" && node.Data == "h2" && node.Parent.Data == "section" {
					id = domAttr(node.Parent, "id")
				}
				if id != "" {
					finish()
					entries = append(entries, searchEntry{URL: page.Slug + ".html#" + id, Page: page.Label, Heading: headingText(node), Variant: variant})
					return
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			read(child)
		}
		if node.Type == dom.ElementNode && !slices.Contains(inlineTags, node.Data) {
			text.WriteByte(' ')
		}
	}
	read(main)
	finish()
	return entries, nil
}

// headingText is a heading's words without the step number a reader doesn't type.
func headingText(heading *dom.Node) string {
	var b strings.Builder
	for child := heading.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == dom.ElementNode && domAttr(child, "aria-hidden") == "true" {
			continue
		}
		b.WriteString(nodeText(child))
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
