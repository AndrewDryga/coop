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
type ServePort struct {
	Box, Host int
	Taken     bool // something else holds Host, so a link would not reach the box
}

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

// ServeUpText is what a thread hears when a server starts listening on a serve port in its box.
func ServeUpText(p ServePort, command string) string {
	text := fmt.Sprintf("Port %d is up", p.Box)
	if command = shortCommand(command); command != "" {
		text += ": " + codeSpan(command)
	}
	if p.Taken {
		return text + fmt.Sprintf(", but localhost:%d is in use by another program on this computer, so it does not reach this box.", p.Host)
	}
	return text + fmt.Sprintf(" → [localhost:%d](http://localhost:%d)", p.Host, p.Host)
}

// ServeLoopbackText says why a server the box runs cannot be opened: a published port reaches the
// box's own network address, and a server bound to 127.0.0.1 does not listen there.
func ServeLoopbackText(p ServePort, command string) string {
	what := "It"
	if command = shortCommand(command); command != "" {
		what = codeSpan(command)
	}
	return fmt.Sprintf("⚠ **Your browser cannot reach port %d:** %s listens only on 127.0.0.1 inside the box. Bind it to 0.0.0.0.", p.Box, what)
}

// shortCommand keeps a command line short enough to read in a thread.
func shortCommand(command string) string {
	const limit = 80
	if runes := []rune(strings.TrimSpace(command)); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return strings.TrimSpace(command)
}

// SetServePorts records the stable ports' state for every later thread's start message.
func (c *Control) SetServePorts(ports []ServePort) {
	c.mu.Lock()
	c.serve = slices.Clone(ports)
	c.mu.Unlock()
}

// SetNetwork records a filtered session's limits for every thread's start message.
func (c *Control) SetNetwork(limit NetworkLimit) {
	c.mu.Lock()
	c.network = &limit
	c.mu.Unlock()
}

// SetBoxWait records why messages wait for the project's box, for every thread that opens and every
// message sent from now on; "" says they no longer wait. The supervisor writes it, in fixed sentences.
func (c *Control) SetBoxWait(text string) {
	c.mu.Lock()
	c.boxWait = text
	c.mu.Unlock()
}

// AddStartProblems records problems found while the session started. They go out once, with the
// first thread's start message, or with the next session update once that message has gone.
func (c *Control) AddStartProblems(problems ...Problem) {
	for _, p := range problems {
		c.mu.Lock()
		text := ProblemText(p)
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
	if c.boxWait != "" {
		// The ports and services above belong to a box that is not running yet, but they will be right
		// once it is. What the person has to do comes first.
		text = strings.TrimSuffix(c.boxWait+"\n\n"+text, "\n\n")
	}
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
			if p.Taken {
				lines = append(lines, fmt.Sprintf("- Port %d: localhost:%d is in use by another program on this computer, so it does not reach this box. Coop takes it once it is free.", p.Box, p.Host))
				continue
			}
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
		blocks = append(blocks, ProblemText(p))
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
// ProblemText renders a problem the way a thread's start message does.
func ProblemText(p Problem) string {
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
