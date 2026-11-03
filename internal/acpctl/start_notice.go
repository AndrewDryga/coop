package acpctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/AndrewDryga/coop/internal/safetext"
)

// ServePort is one project port the box publishes: Box is what the dev server listens on inside the
// box, Host the stable localhost port a browser on this machine opens it at.
type ServePort struct{ Box, Host int }

// ServiceLink is one published port of a project service: the Compose service's name, the scheme its
// compose file declares (http unless a coop.service.scheme label says otherwise) and the stable
// localhost port it answers on.
type ServiceLink struct {
	Name, Scheme string
	Host         int
}

// NetworkLimit is what a filtered session's boxes may reach besides the agent's provider, grouped
// like box.NetworkAllowed: approved destinations, MCP servers and a count of anything else.
type NetworkLimit struct {
	Sites, Servers []string
	Other          int
}

// Problem is something the person should fix that would otherwise reach only the editor's log.
// Title and Detail are plain text. Command, when set, is shown as code to run, followed by Purpose
// ("to see why"), if any.
type Problem struct{ Title, Detail, Command, Purpose string }

// maxNamedAllowances is how many approved destinations the network line names before it counts them.
const maxNamedAllowances = 5

// SetServices records the project services this session's boxes publish. Listing them asks the
// container runtime, so the supervisor calls this from a goroutine; a thread that opens before it
// lands simply gets no services block.
func (c *Control) SetServices(links []ServiceLink) {
	c.mu.Lock()
	c.services = slices.Clone(links)
	c.mu.Unlock()
}

// SetNetwork records a filtered session's limits for every thread's start message.
func (c *Control) SetNetwork(limit NetworkLimit) {
	c.mu.Lock()
	c.network = &limit
	c.mu.Unlock()
}

// AddStartProblems records problems found while the session started. They go out once, with the
// first thread's start message, or with the next session update once that message has gone.
func (c *Control) AddStartProblems(problems ...Problem) {
	for _, p := range problems {
		c.mu.Lock()
		text := problemText(p)
		fresh, late := !c.told[text], c.problemsTold
		c.told[text] = true
		if fresh && !late {
			c.problems = append(c.problems, p)
		}
		c.mu.Unlock()
		if fresh && late {
			c.Notify(text)
		}
	}
}

// startNoticeFor returns a thread's start message when line is the result that established an
// editor session (it carries a sessionId, as session/new does), or nil. The sessionId alone decides:
// Gemini and Grok answer session/new with models but no configOptions. Only answers to the editor's
// own requests reach here (the proxy swallows coop's injected requests and a box restart's replay),
// and the message is stable, so once per session is enough.
func (c *Control) startNoticeFor(line []byte) []byte {
	if !bytes.Contains(line, []byte(`"sessionId"`)) || !bytes.Contains(line, []byte(`"result"`)) {
		return nil
	}
	var m struct {
		Result *struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	if json.Unmarshal(line, &m) != nil || m.Result == nil || m.Result.SessionID == "" {
		return nil
	}
	sid := m.Result.SessionID
	c.mu.Lock()
	if c.reported[sid] {
		c.mu.Unlock()
		return nil
	}
	c.reported[sid] = true
	var problems []Problem
	if !c.problemsTold {
		problems, c.problemsTold = c.problems, true
	}
	text := startNoticeText(c.serve, c.services, c.network, problems)
	c.mu.Unlock()
	if text == "" {
		return nil
	}
	return c.statusMessage(sid, text)
}

// startNoticeText is the message a new thread opens with. Each block appears only when it has
// something to say: the box's ports, the project's services, a filtered session's limits, then
// problems. Editors render agent text as Markdown, so each port or service is its own list item
// (bare newlines would fold into one paragraph), each browser address is a real link, and text Coop
// did not write itself is escaped so it cannot turn into a link or formatting.
func startNoticeText(ports []ServePort, services []ServiceLink, network *NetworkLimit, problems []Problem) string {
	var blocks []string
	if len(ports) > 0 {
		title := "Open the box's ports in your browser"
		if len(ports) == 1 {
			title = "Open the box's port in your browser"
		}
		lines := []string{"**" + title + "**", ""}
		for _, p := range ports {
			lines = append(lines, fmt.Sprintf("- Port %d → [localhost:%d](http://localhost:%d)", p.Box, p.Host, p.Host))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	if len(services) > 0 {
		lines := []string{"**Project services**", ""}
		for _, s := range services {
			lines = append(lines, "- "+markdownText(s.Name)+" → "+serviceAddress(s))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	if network != nil {
		blocks = append(blocks, networkText(*network))
	}
	for _, p := range problems {
		blocks = append(blocks, problemText(p))
	}
	return strings.Join(blocks, "\n\n")
}

// serviceAddress links a browser scheme. Anything else (postgresql, redis) is shown whole as code,
// ready for the client that speaks it.
func serviceAddress(s ServiceLink) string {
	address := fmt.Sprintf("localhost:%d", s.Host)
	if s.Scheme == "http" || s.Scheme == "https" {
		return "[" + address + "](" + s.Scheme + "://" + address + ")"
	}
	return codeSpan(s.Scheme + "://" + address)
}

// networkText says what a filtered session may reach, in the terminal's grouping
// (box.NetworkAllowed). The agent's provider always leads, however it is reached: through Coop's
// credential broker the policy holds no grant for it, so naming vendors from the policy alone said
// nothing was allowed. A short list is named in full; a longer one is counted, with the command
// that lists it.
func networkText(n NetworkLimit) string {
	allowed := []string{"the agent's provider"}
	named := len(n.Sites)+len(n.Servers) <= maxNamedAllowances
	if named {
		allowed = append(allowed, n.Sites...)
		for _, server := range n.Servers {
			allowed = append(allowed, "the "+server+" MCP server")
		}
	} else {
		if len(n.Sites) > 0 {
			allowed = append(allowed, count(len(n.Sites), "approved network rule"))
		}
		if len(n.Servers) > 0 {
			allowed = append(allowed, count(len(n.Servers), "MCP server"))
		}
	}
	if n.Other > 0 {
		allowed = append(allowed, count(n.Other, "other destination"))
	}
	for i := range allowed {
		allowed[i] = markdownText(allowed[i])
	}
	line := "**Internet is filtered.** Allowed: " + strings.Join(allowed, ", ") + "."
	if !named {
		line += " " + codeSpan("coop net") + " lists them."
	}
	return line + "\n\nTo allow another site, add it to " + codeSpan(".agent/project.yaml") + " and run " +
		codeSpan("coop approve") + ". Open threads pick it up on their own."
}

// problemText renders one problem on one line: the title in bold, the detail, then what to run.
func problemText(p Problem) string {
	text := "⚠ **" + markdownText(p.Title) + ":** " + markdownText(p.Detail)
	if p.Command != "" {
		text += " Run " + codeSpan(p.Command)
		if p.Purpose != "" {
			text += " " + markdownText(p.Purpose)
		}
		text += "."
	}
	return text
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// visible shows each character that would act on the display, break the line, turn the text
// around or draw as nothing (safetext.Hidden) as its \u escape, the way the terminal shows them, so
// a name Coop repeats stays on its line and cannot pass for another.
func visible(s string) string {
	var b strings.Builder
	previous := rune(-1)
	for _, r := range s {
		switch hidden := safetext.Hidden(previous, r); {
		case !hidden:
			b.WriteRune(r)
		case r > 0xFFFF:
			fmt.Fprintf(&b, "\\U%08X", r)
		default:
			fmt.Fprintf(&b, "\\u%04X", r)
		}
		previous = r
	}
	return b.String()
}

// markdownText makes text Coop did not write itself (an account, preset, service or MCP server
// name, an approved domain) literal in Markdown. Hidden characters are shown escaped (visible). Every character Markdown could read as a link, an image,
// emphasis, code, HTML or an entity is backslash-escaped, and so are the ones GFM autolinking keys
// on: a dot inside a word (www.example.com), a colon before a slash (https://) and an at sign.
func markdownText(s string) string {
	runes := []rune(visible(s))
	var b strings.Builder
	for i, r := range runes {
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		if strings.ContainsRune("\\`*_[]()<>!|~&#{}@", r) || r == '.' && next != 0 && !unicode.IsSpace(next) || r == ':' && next == '/' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// codeSpan shows s as inline code: fenced by one more backtick than its longest run, so a backtick
// inside cannot end the span, with hidden characters shown escaped as in markdownText.
func codeSpan(s string) string {
	s = visible(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if longest > 0 {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}
