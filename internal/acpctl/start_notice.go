package acpctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/AndrewDryga/coop/internal/acpproxy"
)

// ServePort is one project port the box publishes: Box is what the dev server listens on inside the
// box, Host the stable localhost port a browser on this machine opens it at.
type ServePort struct {
	Box, Host int
	Taken     bool // something else holds Host, so a link would not reach the box
}

// ServiceLink is one published port of a project service: the Compose service's name, the scheme it
// speaks (its coop.service.scheme label, else its image's, else http) and the stable localhost port
// it answers on.
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
// Title and Detail are plain text, and Title is a sentence. Command, when set, is shown as a code block
// to run, introduced by Purpose ("on the host, to see why").
type Problem struct{ Title, Detail, Command, Purpose string }

// maxNamedAllowances is how many sites or MCP servers a network line names before it says how many more.
const maxNamedAllowances = 3

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
	return fmt.Sprintf("⚠ **Your browser cannot reach port %d.**\n\n%s listens only on 127.0.0.1 inside the box. Bind it to 0.0.0.0.", p.Box, what)
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
// message sent from now on; "" says they no longer wait. The supervisor writes it, in fixed sentences
// and with anything it quotes already escaped.
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
// own requests reach here (the proxy swallows coop's injected requests and a box restart's replay).
// A reopened thread hears it from beginReplay instead, once per open, before its replay.
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
	seen := c.reported[sid]
	c.reported[sid] = true
	c.mu.Unlock()
	if seen {
		return nil
	}
	return c.startNotice(sid)
}

// startNotice builds a thread's start message from what is true now, or nil when it has nothing to say.
func (c *Control) startNotice(sid string) []byte {
	c.mu.Lock()
	var problems []Problem
	if !c.problemsTold {
		problems, c.problemsTold = c.problems, true
	}
	text := startNoticeText(c.serve, c.services, c.network, problems)
	if c.boxWait != "" {
		// What the person has to do comes first. The rest describes a box that is not running yet, and
		// it says so: a link posted as live would open nothing.
		if text != "" {
			text = threadSectionBreak + "Nothing is published and no project rules apply yet. Once the box is running:\n\n" + text
		}
		text = c.boxWait + text
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
			lines = append(lines, "- "+MarkdownText(s.Name)+" → "+serviceAddress(s))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	if network != nil {
		blocks = append(blocks, networkText(*network))
	}
	for _, p := range problems {
		blocks = append(blocks, ProblemText(p))
	}
	return strings.Join(blocks, threadSectionBreak)
}

// threadSectionBreak sets the sections of a thread message apart. Zed puts space under a paragraph and
// none under a list, so a title after a list sits closer to what it follows than to its own text.
const threadSectionBreak = "\n\n---\n\n"

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
// nothing was allowed. Sites and MCP servers each get a line naming up to maxNamedAllowances of them
// and how many more, with the command that lists the rest.
func networkText(n NetworkLimit) string {
	allowed := []string{MarkdownText("the agent's provider")}
	more := false
	line := func(one, many string, names []string) {
		if len(names) == 0 {
			return
		}
		label := many
		if len(names) == 1 {
			label = one
		}
		shown := names
		if len(names) > maxNamedAllowances {
			shown, more = names[:maxNamedAllowances], true
		}
		escaped := make([]string, len(shown))
		for i, name := range shown {
			escaped[i] = MarkdownText(name)
		}
		list := strings.Join(escaped, ", ")
		if rest := len(names) - len(shown); rest > 0 {
			list += fmt.Sprintf(" and %d more", rest)
		}
		allowed = append(allowed, label+": "+list)
	}
	line("Site", "Sites", n.Sites)
	line("MCP server", "MCP servers", n.Servers)
	if n.Other > 0 {
		allowed = append(allowed, MarkdownText(count(n.Other, "other destination")))
		more = true
	}
	text := "**Internet is filtered.**\n\nAllowed: " + allowed[0] + "."
	if len(allowed) > 1 {
		text = "**Internet is filtered.**\n\nAllowed:\n\n- " + strings.Join(allowed, "\n- ")
	}
	// One section: its parts are paragraphs, and a rule only sets sections apart (startNoticeText).
	// "On the host" is said once: by the first command that needs it.
	where := "in this project, on the host"
	if more {
		text += "\n\nTo see them all, run this on the host:\n\n" + CodeBlock("coop net")
		where = "in this project"
	}
	return text + "\n\nTo allow another site, add it to " + codeSpan(".agent/project.yaml") + ", then run this " + where + ":\n\n" +
		CodeBlock("coop approve")
}

// ProblemText renders a problem the way a thread's start message does: the title alone on its line, the
// detail, then what to run.
func ProblemText(p Problem) string {
	text := "⚠ **" + MarkdownText(p.Title) + "**"
	if p.Detail != "" {
		text += "\n\n" + MarkdownText(p.Detail)
	}
	if p.Command != "" {
		text += "\n\nRun this " + MarkdownText(p.Purpose) + ":\n\n" + CodeBlock(acpproxy.Visible(p.Command))
	}
	return text
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// MarkdownText makes text literal in Markdown: anything Coop did not write itself (an account, preset,
// service or MCP server name, an approved domain), or a sentence of its own that names a file or a setting.
// Hidden characters are shown escaped (acpproxy.Visible). Every character Markdown could read as a link, an image,
// emphasis, code, HTML or an entity is backslash-escaped, and so are the ones GFM autolinking keys
// on: a dot inside a word (www.example.com), a colon before a slash (https://) and an at sign.
func MarkdownText(s string) string {
	runes := []rune(acpproxy.Visible(s))
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
// inside cannot end the span, with hidden characters shown escaped as in MarkdownText.
func codeSpan(s string) string {
	s = acpproxy.Visible(s)
	longest := longestBackticks(s)
	fence := strings.Repeat("`", longest+1)
	if longest > 0 {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// CodeBlock shows text as a code block of its own, which an editor offers to copy whole: every command
// a thread asks the person to run goes in one. A backtick inside cannot end it, and hidden characters are
// shown escaped as in MarkdownText, line by line.
func CodeBlock(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = acpproxy.Visible(line)
	}
	text = strings.Join(lines, "\n")
	fence := strings.Repeat("`", max(3, longestBackticks(text)+1))
	return fence + "\n" + text + "\n" + fence
}

func longestBackticks(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}
