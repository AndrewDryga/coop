package main

// The website's generated pages: the homepage, site/index.html, from tools/site/index.tpl.html; the
// Orchestrator page, site/orchestrator.html, from tools/site/orchestrator.tpl.html; and the terminal
// windows in site/docs.html, which is written by hand except between its <!-- gen_site: NAME --> and
// <!-- /gen_site --> markers, where the scene NAME goes (scenes.go). Edit a template or a scene, then
// regenerate; never edit the generated parts by hand. Every page has its links off the site marked
// to open in a new tab with rel="nofollow noopener", and its stylesheet and script links stamped
// with a hash of those files, so a page written without either fails -check until regenerated.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	siteHost = "coop.dryga.com"
	markPath = "assets/img/favicon.svg" // co:op's mark, as the nav and footer show it
)

// sitePages renders the generated pages from the checkout at root, keyed by their path in it.
func sitePages(root string, generated map[string]string) (map[string]string, error) {
	sprite, err := markSprite(root)
	if err != nil {
		return nil, err
	}
	board, err := nightBoard()
	if err != nil {
		return nil, err
	}
	version, err := assetVersion(root)
	if err != nil {
		return nil, err
	}
	index, err := fillTemplate(root, "index.tpl.html", []block{
		{"sprite", sprite}, {"scene", scene}, {"own_runs", ownRuns}, {"queue", queueScene}, {"loop", loopScene},
		{"fork", forkScene}, {"rotate", rotate}, {"team", team}, {"services", services}, {"integrations", integrations},
		{"check", checkIcon}, {"providers", providers}, {"install", installCommand}, {"cmd_login", loginCommand},
		{"cmd_init", initCommand}, {"cmd_run", command("Start", `<span data-cmd="run">coop claude</span>`, "start")},
		{"mark", markPath},
	})
	if err != nil {
		return nil, err
	}
	orchestrator, err := fillTemplate(root, "orchestrator.tpl.html", []block{
		{"sprite", sprite}, {"mark", markPath}, {"providers", providers}, {"check", checkIcon}, {"board", board},
		{"scene", scene}, {"services", services}, {"team", team}, {"fork", forkScene}, {"integrations", integrations},
		{"install", installCommand}, {"cmd_login", loginCommand}, {"cmd_init", initCommand},
		{"cmd_loop", command("Start", `<span data-cmd="loop">coop loop claude</span>`, "start")},
	})
	if err != nil {
		return nil, err
	}
	docs, err := os.ReadFile(filepath.Join(root, "site", "docs.html"))
	if err != nil {
		return nil, err
	}
	filled, err := fillDocs(string(docs))
	if err != nil {
		return nil, err
	}
	pages := map[string]string{
		filepath.Join("site", "index.html"):        stampAssets(externalLinks(typographic(index)), version),
		filepath.Join("site", "orchestrator.html"): stampAssets(externalLinks(typographic(orchestrator)), version),
		filepath.Join("site", "docs.html"):         stampAssets(externalLinks(filled), version),
	}
	for _, name := range []string{"sandbox-guide", "overnight-guide"} {
		guide, err := fillTemplate(root, name+".tpl.html", nil)
		if err != nil {
			return nil, err
		}
		pages[filepath.Join("site", name+".html")] = stampAssets(externalLinks(typographic(guide)), version)
	}
	references, err := referencePages(root, generated, version)
	if err != nil {
		return nil, err
	}
	maps.Copy(pages, references)
	pages[filepath.Join("site", "sitemap.xml")] = siteMap(pages)
	return pages, nil
}

var logoPath = regexp.MustCompile(`<path d="([^"]+)"`)

// markSprite holds each provider mark's path once per page; every mark() points at it.
func markSprite(root string) (string, error) {
	sprite := `<svg class="sprite" aria-hidden="true">`
	for _, slug := range []string{"claude", "openai", "gemini", "grok"} {
		logo, err := os.ReadFile(filepath.Join(root, "tools", "site", "logos", slug+".svg"))
		if err != nil {
			return "", err
		}
		path := logoPath.FindSubmatch(logo)
		if path == nil {
			return "", fmt.Errorf("tools/site/logos/%s.svg has no path", slug)
		}
		sprite += `<symbol id="mark-` + slug + `" viewBox="0 0 24 24"><path fill-rule="evenodd" d="` + string(path[1]) + `"/></symbol>`
	}
	return sprite + "</svg>", nil
}

// block is what fills a template's {{name}}.
type block struct{ name, html string }

var placeholder = regexp.MustCompile(`\{\{\w+\}\}`)

func fillTemplate(root, name string, blocks []block) (string, error) {
	template, err := os.ReadFile(filepath.Join(root, "tools", "site", name))
	if err != nil {
		return "", err
	}
	values := make(map[string]string, len(blocks))
	for _, b := range blocks {
		values["{{"+b.name+"}}"] = b.html
	}
	var left []string
	// Substitute only tokens in the original template. Rendered code may itself
	// contain {{names}}, which must remain literal instead of becoming template input.
	page := placeholder.ReplaceAllStringFunc(string(template), func(token string) string {
		if value, ok := values[token]; ok {
			return value
		}
		left = append(left, token)
		return token
	})
	if len(left) != 0 {
		return "", fmt.Errorf("tools/site/%s: nothing fills %s", name, strings.Join(left, ", "))
	}
	return page, nil
}

var docsRegion = regexp.MustCompile(`(<!-- gen_site: ([\w-]+) -->)(?s:.*?)(<!-- /gen_site -->)`)

// fillDocs puts each marked region's scene into the docs.
func fillDocs(docs string) (string, error) {
	const indexStart, indexEnd = "<!-- gen_reference_index -->", "<!-- /gen_reference_index -->"
	before, rest, ok := strings.Cut(docs, indexStart)
	if !ok {
		return "", fmt.Errorf("docs.html is missing the reference index")
	}
	_, after, ok := strings.Cut(rest, indexEnd)
	if !ok {
		return "", fmt.Errorf("docs.html is missing the reference index end")
	}
	docs = before + indexStart + "\n" + referenceIndex() + indexEnd + after
	var out strings.Builder
	last := 0
	regions := docsRegion.FindAllStringSubmatchIndex(docs, -1)
	for _, m := range regions {
		name := docs[m[4]:m[5]]
		terminal, ok := docsScenes[name]
		if !ok {
			return "", fmt.Errorf("site/docs.html marks a scene named %q, and there is none", name)
		}
		out.WriteString(docs[last:m[0]] + docs[m[2]:m[3]] + terminal + docs[m[6]:m[7]])
		last = m[1]
	}
	if len(regions) != len(docsScenes) {
		return "", fmt.Errorf("site/docs.html marks %d scenes, expected %d", len(regions), len(docsScenes))
	}
	return out.String() + docs[last:], nil
}

var (
	htmlTag  = regexp.MustCompile(`<[^>]+>`)
	verbatim = regexp.MustCompile(`^</?(?:pre|code|script|style)\b`)
)

// typographic curls the apostrophes in the prose (don’t, team’s, developers’); code, commands,
// scripts and attribute values keep straight ones, since people copy those.
func typographic(page string) string {
	var out strings.Builder
	inside := 0
	write := func(part string) {
		if verbatim.MatchString(part) {
			if strings.HasPrefix(part, "</") {
				inside--
			} else {
				inside++
			}
		}
		if strings.HasPrefix(part, "<") || inside != 0 {
			out.WriteString(part)
		} else {
			out.WriteString(curlApostrophes(part))
		}
	}
	last := 0
	for _, tag := range htmlTag.FindAllStringIndex(page, -1) {
		write(page[last:tag[0]])
		write(page[tag[0]:tag[1]])
		last = tag[1]
	}
	write(page[last:])
	return out.String()
}

// curlApostrophes curls an apostrophe inside a word (don’t) or after a plural s (developers’).
func curlApostrophes(text string) string {
	var out strings.Builder
	last := 0
	for i := 0; i < len(text); i++ {
		if text[i] != '\'' {
			continue
		}
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[i+1:])
		if (isWord(before) && isWord(after)) || (before == 's' && (unicode.IsSpace(after) || strings.ContainsRune(".,;:!?)", after))) {
			out.WriteString(text[last:i] + "’")
			last = i + 1
		}
	}
	return out.String() + text[last:]
}

// isWord reports a letter, a digit or an underscore, in any script.
func isWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

var (
	anchorTag  = regexp.MustCompile(`<a\b[^>]*>`)
	hrefAttr   = regexp.MustCompile(`\shref=(?:"(https?://[^"']*)"|'(https?://[^"']*)')`)
	targetAttr = regexp.MustCompile(`(\starget=)(?:"[^"']*"|'[^"']*')`)
	anyTarget  = regexp.MustCompile(`\starget=`)
	relAttr    = regexp.MustCompile(`\srel=(?:"([^"']*)"|'([^"']*)')`)
)

// externalLinks makes every link off the site open in a new tab with rel="nofollow noopener": the
// site does not pass search ranking to the many pages it cites, and the new tab cannot reach back
// into this one. Relative links, anchors and coop.dryga.com stay as they are; a rel already on a
// link keeps its other words. Running it again changes nothing, so -check catches a link that
// lacks it.
func externalLinks(page string) string {
	return anchorTag.ReplaceAllStringFunc(page, func(tag string) string {
		href := hrefAttr.FindStringSubmatch(tag)
		if href == nil {
			return tag
		}
		if u, err := url.Parse(href[1] + href[2]); err == nil && strings.ToLower(u.Hostname()) == siteHost {
			return tag
		}
		if anyTarget.MatchString(tag) {
			tag = targetAttr.ReplaceAllString(tag, `${1}"_blank"`)
		} else {
			tag = tag[:len(tag)-1] + ` target="_blank">`
		}
		var words []string
		rel := relAttr.FindStringSubmatchIndex(tag)
		if rel != nil {
			value := rel[2:4] // the double-quoted value, else the single-quoted one
			if value[0] < 0 {
				value = rel[4:6]
			}
			words = strings.Fields(tag[value[0]:value[1]])
		}
		for _, word := range []string{"nofollow", "noopener"} {
			if !slices.Contains(words, word) {
				words = append(words, word)
			}
		}
		attr := ` rel="` + strings.Join(words, " ") + `"`
		if rel != nil {
			return tag[:rel[0]] + attr + tag[rel[1]:]
		}
		return tag[:len(tag)-1] + attr + ">"
	})
}

var assetLink = regexp.MustCompile(`(assets/(?:css|js)/[\w.-]+\.(?:css|js))\?v=[\w-]+`)

// assetVersion is a short hash of the stylesheets and scripts, put on their links (?v=) so a browser
// holding an older copy fetches the new one the moment any of them changes.
func assetVersion(root string) (string, error) {
	digest := sha256.New()
	for _, name := range []string{"css/site.css", "js/site.js", "js/analytics.js", "css/orchestrator.css", "js/orchestrator.js"} {
		asset, err := os.ReadFile(filepath.Join(root, "site", "assets", filepath.FromSlash(name)))
		if err != nil {
			return "", err
		}
		digest.Write(asset)
	}
	return hex.EncodeToString(digest.Sum(nil))[:10], nil
}

func stampAssets(page, version string) string {
	return assetLink.ReplaceAllString(page, "${1}?v="+version)
}

// readFrom reads a file of the checkout at root by its slash-separated path.
func readFrom(root string) func(string) (string, error) {
	return func(rel string) (string, error) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		return string(data), err
	}
}

// terminalSource says where a terminal's lines come from, so a CLI change can't leave the site
// showing output the CLI no longer prints (terminalDrift). A transcript is approved output, and every
// line of the block must be one of its lines. Otherwise the block cites the Go files that print it
// and the shape of each line it shows, {} standing for a value; the shape's literal parts, its own
// text between the values unless listed, must still be in one of those files. A line that is only an
// example value, such as a task title, is listed as one.
type terminalSource struct {
	transcript string
	goFiles    []string
	examples   []string
	shapes     []lineShape
}

type lineShape struct {
	text  string
	parts []string
}

func shape(text string, parts ...string) lineShape {
	if len(parts) == 0 {
		for _, part := range strings.Split(text, "{}") {
			if strings.TrimSpace(part) != "" {
				parts = append(parts, part)
			}
		}
	}
	return lineShape{text, parts}
}

var terminalSources = map[string]terminalSource{
	"doctor":        {transcript: "internal/cli/testdata/approved/18a-doctor-all-passed.txt"},
	"check-secrets": {transcript: "internal/cli/testdata/approved/19e-finding-and-ignored-blind-spot.txt"},
	"claude": {goFiles: []string{"internal/box/launch_sections.go"}, examples: []string{"Claude Code"}, shapes: []lineShape{
		shape("Protecting secrets"), shape("{} hidden from the box", "hidden from the box", `"secret path"`),
		shape("Connecting account"), shape("{} · Signed in", `"Signed in"`), shape("Configuring network access"),
		shape("{} endpoints allowed"), shape("Everything else blocked"), shape("Starting {}", `"Starting "`)}},
	"loop": {goFiles: []string{"internal/loop/report.go", "internal/loop/iteration.go", "internal/loop/banners.go",
		"internal/loop/streamjson.go", "internal/loop/streamjson_providers.go"},
		examples: []string{taskCheckout, taskHealth, taskLimits}, shapes: []lineShape{
			shape("━", `strings.Repeat("━"`), shape("─", `strings.Repeat("─"`), shape("Task {} - Attempt {}"),
			shape("Agent  {}"), shape("Queue  {}"), shape("Starting {}", `"Starting "`), shape("✦ {}", `llmIcon = "✦"`),
			shape("✎ Edit {}", `"✎"`), shape("⚙ Bash {}", `"⚙", "Bash"`), shape("Task completed: {}"),
			shape("Final review · Round {} of {}"), shape("Tasks  {} completed", `label: "Tasks"`, `"%d completed"`),
			shape("Final review · {} needs more work", `"Final review`, `"needs"`, `" more work"`),
			shape("Continuing the task queue"), shape("All tasks passed final review · {}/{} done")}},
	"fork": {goFiles: []string{"internal/cli/fork_cmd.go", "internal/forkctl/supervise.go", "internal/forkctl/ls.go",
		"internal/forkctl/status.go", "internal/forkctl/review.go", "internal/forkctl/dossier.go",
		"internal/forkctl/merge.go", "internal/ui/confirm.go"},
		examples: []string{"/Users/you/code/shop-forks/api", "9c41e2a webhook: verify Stripe signatures",
			"3a7f0db webhook: dedupe replayed events by id"}, shapes: []lineShape{
			shape("Creating fork: {}", `"Creating fork"`, `"%s: %s"`), shape("Started fork {} in the background"),
			shape("Agent: {}"), shape("Logs:  coop fork logs {} --follow"), shape("Stop:  coop fork stop {}"),
			shape("Forks", `listing("Forks")`), shape("{} · ready to merge · updated {}", `" · updated "`, `"ready to merge"`),
			shape("Branch: {}"), shape("Tasks: {} ready to merge", `"    Tasks: %s"`, `"ready to merge"`), shape("Changes: {}"),
			shape("Review a fork: coop fork review <name>"), shape("Changes in fork {}"), shape("Into: {}"),
			shape("{} · {} · +{} −{}", `"  %s · %s · +%d −%d"`), shape("Commits", `say("Commits")`), shape("Files", `say("Files")`),
			shape("code:", `dossierCode   = "code"`), shape("tests:", `dossierTests  = "tests"`),
			shape("{}  {}  +{} -{}", `" + f.path`, `"  +%d -%d"`), shape("Project checks will run when you merge."),
			shape("Merge fork {} into {}"), shape("{} · +{} −{}", `"  %s · +%d −%d"`),
			shape("Merge these commits? [Y/n] {}", `"Merge these commits?"`, `"Y/n"`), shape("Rebasing onto {}"),
			shape("Running project checks"), shape("Running: {}"), shape("Merged fork {} into {}")}},
	"watch": {goFiles: []string{"internal/tasks/watch.go", "internal/tasks/lease.go", "internal/taskstate/taskstate.go", "internal/ui/live.go"},
		shapes: []lineShape{
			shape("[{}]  {} todo · {} in_progress · {} blocked · {} done", `"%d %s"`, `"10_in_progress"`, `"░"`),
			shape("◰ {} ({}/{}) · busy {}", `"◰"`, `" (%d/%d)"`, `"busy "`), shape("○ {}", `"○"`), shape("⚑ {}", `"⚑"`)}},
}

var (
	windowFigure = regexp.MustCompile(`(?s)<figure class="window[^"]*"[^>]*>.*?</figure>`)
	sceneName    = regexp.MustCompile(`data-scene="([^"]+)"`)
	regionName   = regexp.MustCompile(`<!-- gen_site: ([\w-]+) -->`)
	lineOpen     = regexp.MustCompile(`<span class="line([^"]*)"[^>]*>`)
	lineEnd      = regexp.MustCompile(`<span class="line|</code>|</template>`)
)

// terminalBlocks finds each terminal on a page by name (its data-scene, else the gen_site region
// holding it) with the lines a visitor sees: replay frames included, the reader's typed commands and
// blank or ellipsis-only lines left out.
func terminalBlocks(page string) map[string][]string {
	blocks := map[string][]string{}
	for _, at := range windowFigure.FindAllStringIndex(page, -1) {
		figure := page[at[0]:at[1]]
		name := ""
		if named := sceneName.FindStringSubmatch(figure[:strings.IndexByte(figure, '>')+1]); named != nil { // its opening tag
			name = named[1]
		} else if regions := regionName.FindAllStringSubmatch(page[:at[0]], -1); regions != nil {
			name = regions[len(regions)-1][1]
		}
		var lines []string
		for rest := figure; ; {
			open := lineOpen.FindStringSubmatchIndex(rest)
			if open == nil {
				break
			}
			end := lineEnd.FindStringIndex(rest[open[1]:])
			if end == nil {
				break
			}
			cls, inner := rest[open[2]:open[3]], rest[open[1]:open[1]+end[0]]
			rest = rest[open[1]+end[0]:]
			text := strings.TrimRightFunc(html.UnescapeString(htmlTag.ReplaceAllString(inner, "")), unicode.IsSpace)
			if !strings.Contains(cls, "cmdline") && !slices.Contains([]string{"", "…", "..."}, strings.TrimSpace(text)) && !slices.Contains(lines, text) {
				lines = append(lines, text)
			}
		}
		if name != "" && lines != nil {
			blocks[name] = lines
		}
	}
	return blocks
}

// terminalDrift lists every problem with the terminals' sources, as "page, block: what" lines.
func terminalDrift(read func(rel string) (string, error)) ([]string, error) {
	problems := map[string]bool{}
	for _, page := range []string{"index.html", "docs.html"} {
		text, err := read("site/" + page)
		if err != nil {
			return nil, err
		}
		for name, lines := range terminalBlocks(text) {
			source, ok := terminalSources[name]
			if !ok {
				problems[fmt.Sprintf("%s, block %s: no source is listed in terminalSources", page, name)] = true
				continue
			}
			if source.transcript != "" {
				transcript, err := read(source.transcript)
				if err != nil {
					return nil, err
				}
				known := map[string]bool{}
				for _, l := range strings.Split(transcript, "\n") {
					known[strings.TrimRightFunc(l, unicode.IsSpace)] = true
				}
				for _, l := range lines {
					if !known[l] {
						problems[fmt.Sprintf("%s, block %s: line %q is not in %s", page, name, l, source.transcript)] = true
					}
				}
				continue
			}
			var code []string
			for _, path := range source.goFiles {
				text, err := read(path)
				if err != nil {
					return nil, err
				}
				code = append(code, text)
			}
			var patterns []*regexp.Regexp
			for _, s := range source.shapes {
				for _, part := range s.parts {
					if !slices.ContainsFunc(code, func(text string) bool { return strings.Contains(text, part) }) {
						problems[fmt.Sprintf("%s, block %s: shape %q needs %q, which none of %s has",
							page, name, s.text, part, strings.Join(source.goFiles, ", "))] = true
					}
				}
				literal := strings.Split(s.text, "{}")
				for i := range literal {
					literal[i] = regexp.QuoteMeta(literal[i])
				}
				patterns = append(patterns, regexp.MustCompile("^"+strings.Join(literal, ".+?")+"$"))
			}
			for _, l := range lines {
				said := strings.TrimSpace(l)
				for _, status := range []string{"✓ ", "✗ ", "⚠ "} { // ui.OK/Fail/Warn print these before the message the source holds
					said = strings.TrimPrefix(said, status)
				}
				if said != "" && (strings.Trim(said, "━") == "" || strings.Trim(said, "─") == "") {
					said = string([]rune(said)[:1]) // a rule's shape is its one character, whatever its width
				}
				matches := func(p *regexp.Regexp) bool { return p.MatchString(said) }
				if !slices.Contains(source.examples, said) && !slices.ContainsFunc(patterns, matches) {
					problems[fmt.Sprintf("%s, block %s: line %q matches no shape listed for it", page, name, l)] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(problems)), nil
}
