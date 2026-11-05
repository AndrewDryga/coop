package acpctl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/threadtext"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

type agentMessage struct {
	Method string `json:"method"`
	Params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			MessageID string `json:"messageId"`
		} `json:"update"`
	} `json:"params"`
}

func sessionResult(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"` + id + `","configOptions":[{"id":"model"}]}}` + "\n")
}

// startText is the text of a thread's start message, or "" when there is none.
func startText(t *testing.T, c *Control, session string) string {
	t.Helper()
	line := c.startNoticeFor(sessionResult(session))
	if line == nil {
		return ""
	}
	var msg agentMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		t.Fatalf("start message is not JSON: %v: %s", err, line)
	}
	return msg.Params.Update.Content.Text
}

// A thread's start message goes out once, on the result that established its session (anything
// carrying a sessionId, with or without configOptions), as one framed agent message in that
// session — not on a ConfigOptionUpdate or a session/load answer, not twice, and not when there
// is nothing to say.
func TestACPStartNotice(t *testing.T) {
	c := newTestControl(t)
	c.serve = []ServePort{{Box: 5173, Host: 24187}}

	line := c.startNoticeFor(sessionResult("S"))
	var msg agentMessage
	if err := json.Unmarshal(line, &msg); err != nil || msg.Method != "session/update" || msg.Params.SessionID != "S" ||
		msg.Params.Update.SessionUpdate != "agent_message_chunk" || msg.Params.Update.Content.Type != "text" ||
		!strings.HasPrefix(msg.Params.Update.MessageID, "coop-notice-") {
		t.Fatalf("expected a start message for the new session, got %s (decode error: %v)", line, err)
	}
	if !bytes.HasSuffix(line, []byte("\n")) {
		t.Errorf("the message must end its line, or the editor waits for the rest of it: %q", line)
	}
	want := "\n\n**Open the box's port in your browser**\n\n- Port 5173 → [localhost:24187](http://localhost:24187)\n\n"
	if got := msg.Params.Update.Content.Text; got != want {
		t.Errorf("start message:\n got %q\nwant %q", got, want)
	}
	if again := c.startNoticeFor(sessionResult("S")); again != nil {
		t.Errorf("the start message must go out once per session, got %s", again)
	}

	// Gemini and Grok answer session/new with models and no configOptions; their threads hear it too.
	modelsOnly := []byte(`{"jsonrpc":"2.0","id":3,"result":{"sessionId":"G","models":{"currentModelId":"auto","availableModels":[]}}}` + "\n")
	if n := c.startNoticeFor(modelsOnly); n == nil || !bytes.Contains(n, []byte(`"sessionId":"G"`)) {
		t.Errorf("a session/new result without configOptions must get the message, got %s", n)
	}
	// A ConfigOptionUpdate (configOptions in params, not result) establishes no session, and neither
	// does a session/load answer, which carries no sessionId.
	upd := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S2","update":{"sessionUpdate":"config_option_update","configOptions":[]}}}` + "\n")
	if n := c.startNoticeFor(upd); n != nil {
		t.Errorf("only a session/new result gets the message, got %s", n)
	}
	if n := c.startNoticeFor([]byte(`{"jsonrpc":"2.0","id":4,"result":{"configOptions":[{"id":"model"}]}}` + "\n")); n != nil {
		t.Errorf("a result without a sessionId must not get the message, got %s", n)
	}

	if text := startText(t, newTestControl(t), "S3"); text != "" {
		t.Errorf("nothing to say → no message, got %q", text)
	}
}

// Each block appears only when it has something to say, in order: the box's ports, the project's
// services, a filtered session's limits, problems.
func TestStartNoticeText(t *testing.T) {
	// The network section's parts are paragraphs: a rule only sets sections apart.
	limits := "To allow another site, add it to `.agent/project.yaml`, then run this in this project, on the host:\n\n" +
		"```\ncoop approve\n```"
	grok := Problem{Title: "Grok isn't available.", Detail: "Its saved sign-in no longer works.", Command: "coop login grok@default", Purpose: "on the host"}
	grokText := "⚠ **Grok isn't available.**\n\nIts saved sign-in no longer works.\n\nRun this on the host:\n\n```\ncoop login grok@default\n```"
	for _, tc := range []struct {
		name     string
		ports    []ServePort
		services []ServiceLink
		network  *NetworkLimit
		problems []Problem
		want     string
	}{
		{name: "nothing"},
		{
			name:  "ports",
			ports: []ServePort{{Box: 4000, Host: 43659}, {Box: 9091, Host: 28617}},
			want: "**Open the box's ports in your browser**\n\n" +
				"- Port 4000 → [localhost:43659](http://localhost:43659)\n- Port 9091 → [localhost:28617](http://localhost:28617)",
		},
		{
			// Browser schemes are links; a database address is code a client can take.
			name:     "services",
			services: []ServiceLink{{Name: "keycloak", Scheme: "https", Host: 30344}, {Name: "db", Scheme: "postgresql", Host: 31372}},
			want:     "**Project services**\n\n- keycloak → [localhost:30344](https://localhost:30344)\n- db → `postgresql://localhost:31372`",
		},
		{
			name:    "a short network list is named",
			network: &NetworkLimit{Sites: []string{"proxy.golang.org", "sum.golang.org"}, Servers: []string{"emisar_admin"}},
			want:    "**Internet is filtered.**\n\nAllowed:\n\n- the agent's provider\n- Sites: proxy\\.golang\\.org, sum\\.golang\\.org\n- MCP server: emisar\\_admin\n\n" + limits,
		},
		{
			name:    "a long network list names three and counts the rest",
			network: &NetworkLimit{Sites: []string{"a.example", "b.example", "c.example", "d.example", "e.example"}, Servers: []string{"emisar", "immersive", "ryker", "blitz", "docs", "grafana"}, Other: 1},
			want: "**Internet is filtered.**\n\nAllowed:\n\n- the agent's provider\n- Sites: a\\.example, b\\.example, c\\.example and 2 more\n- MCP servers: emisar, immersive, ryker and 3 more\n- 1 other destination\n\n" +
				"To see them all, run this on the host:\n\n```\ncoop net\n```\n\n" + strings.Replace(limits, "in this project, on the host", "in this project", 1),
		},
		{
			name:     "a problem with its fix",
			problems: []Problem{grok, {Title: `The "review" preset is missing from the toolbar.`, Detail: "It could not be loaded.", Command: "coop presets", Purpose: "on the host, to see why"}},
			want:     grokText + "\n\n---\n\n⚠ **The \"review\" preset is missing from the toolbar.**\n\nIt could not be loaded.\n\nRun this on the host, to see why:\n\n```\ncoop presets\n```",
		},
		{
			name:     "everything, in order",
			ports:    []ServePort{{Box: 4000, Host: 43659}},
			services: []ServiceLink{{Name: "db", Scheme: "postgresql", Host: 31372}},
			// Nothing approved: the provider is still reachable, never "nothing is allowed".
			network:  &NetworkLimit{},
			problems: []Problem{grok},
			want: "**Open the box's port in your browser**\n\n- Port 4000 → [localhost:43659](http://localhost:43659)\n\n---\n\n" +
				"**Project services**\n\n- db → `postgresql://localhost:31372`\n\n---\n\n" +
				"**Internet is filtered.**\n\nAllowed: the agent's provider.\n\n" + limits + "\n\n---\n\n" + grokText,
		},
	} {
		if got := startNoticeText(tc.ports, tc.services, tc.network, tc.problems); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// Text Coop did not write — a preset, account, service or MCP server name, an approved domain, a
// command built from them — reaches the editor literally: rendered as Markdown it makes no link,
// image, emphasis, heading or HTML, and it cannot leave its line.
func TestStartNoticeKeepsForeignTextLiteral(t *testing.T) {
	hostile := "[click](https://evil.example) x](y) ![i](https://evil.example/i.png) *b* _e_ <b>h</b> &lt; www.evil.example me@evil.example https://evil.example/x ev\u202eil\n# heading"
	text := startNoticeText(nil,
		[]ServiceLink{{Name: hostile, Scheme: "postgresql", Host: 1}},
		&NetworkLimit{Sites: []string{hostile}},
		[]Problem{{Title: hostile, Detail: hostile, Command: "coop presets `" + hostile + "`", Purpose: "to see why"}})
	var html bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert([]byte(text), &html); err != nil {
		t.Fatal(err)
	}
	rendered := html.String()
	for _, forbidden := range []string{"evil.example\"", "<img", "<em>", "<h1>", "<b>"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("foreign text rendered as %s:\n%s", forbidden, rendered)
		}
	}
	// The only links are the ones Coop builds; the hostile text adds none.
	if links := strings.Count(rendered, "<a href="); links != 0 {
		t.Errorf("foreign text made %d links:\n%s", links, rendered)
	}
	if strings.Count(text, "\n# ") != 0 {
		t.Errorf("foreign text started its own line:\n%s", text)
	}
	if strings.ContainsRune(rendered, '\u202e') || !strings.Contains(rendered, `\u202E`) {
		t.Errorf("a character that turns text around must show escaped, as in the terminal:\n%s", rendered)
	}
}

// Problems found while the session started go to the first thread alone, and the same problem is
// never told twice. One added after the first thread opened still reaches the editor, with the
// next session update.
func TestACPProblemsAreToldOnce(t *testing.T) {
	c := newTestControl(t)
	grok := Problem{Title: "Grok isn't available.", Detail: "Its saved sign-in no longer works.", Command: "coop login grok@default", Purpose: "on the host"}
	c.AddStartProblems(grok, grok)
	if first := startText(t, c, "S1"); strings.Count(first, "Grok isn't available") != 1 {
		t.Fatalf("the first thread must hear the start problems once, got %q", first)
	}
	if second := startText(t, c, "S2"); second != "" {
		t.Fatalf("a later thread must not hear them again, got %q", second)
	}
	chunk := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S2","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}` + "\n")
	c.AddStartProblems(grok)
	if out := toEd(c, chunk); bytes.Contains(out, []byte("Grok")) {
		t.Fatalf("a problem the editor already heard was repeated: %s", out)
	}
	late := Problem{Title: "Late", Detail: "found after the first thread opened."}
	c.AddStartProblems(late)
	out := toEd(c, chunk)
	if bytes.Count(out, []byte("found after the first thread opened")) != 1 || !bytes.HasSuffix(bytes.TrimSuffix(out, []byte("\n")), []byte(`"text":"hello"}}}}`)) {
		t.Fatalf("want the late problem once, ahead of the agent's own update, got:\n%s", out)
	}
	if again := toEd(c, chunk); bytes.Contains(again, []byte("Late")) {
		t.Fatalf("a late problem was shown twice: %s", again)
	}
}

// Listing project services asks the container runtime, so it may finish after the first thread
// opened: the next thread's start message carries them.
func TestACPServicesReachLaterThreads(t *testing.T) {
	c := newTestControl(t)
	c.serve = []ServePort{{Box: 4000, Host: 43659}}
	if first := startText(t, c, "S1"); strings.Contains(first, "Project services") {
		t.Fatalf("no services were listed yet, got %q", first)
	}
	c.SetServices([]ServiceLink{{Name: "db", Scheme: "postgresql", Host: 31372}})
	if second := startText(t, c, "S2"); !strings.Contains(second, "- db → `postgresql://localhost:31372`") {
		t.Fatalf("a later thread must list the services, got %q", second)
	}
}

// A port something else holds is said plainly and carries no link, because a link would open that
// other program. The port beside it keeps its own.
func TestStartNoticeSaysWhenAPortIsHeldElsewhere(t *testing.T) {
	text := startNoticeText([]ServePort{{Box: 4000, Host: 43659, Taken: true}, {Box: 9091, Host: 28617}}, nil, nil, nil)
	if strings.Contains(text, "(http://localhost:43659)") || !strings.Contains(text, "in use by another program") {
		t.Errorf("a held port must carry no link:\n%s", text)
	}
	if !strings.Contains(text, "[localhost:28617](http://localhost:28617)") {
		t.Errorf("a held port took the other's link with it:\n%s", text)
	}
}

// A thread reads top down: a title on its own line, short paragraphs, and every command to run as a code
// block the editor offers to copy.
func TestThreadMessagesAreStructured(t *testing.T) {
	for name, text := range map[string]string{
		"nothing approved": networkText(NetworkLimit{}),
		"named":            networkText(NetworkLimit{Sites: []string{"a.example"}, Servers: []string{"docs"}}),
		"counted":          networkText(NetworkLimit{Sites: []string{"a.example", "b.example", "c.example", "d.example", "e.example", "f.example"}, Other: 2}),
		"problem":          ProblemText(Problem{Title: "A title.", Detail: "A detail.", Command: "coop login grok@default", Purpose: "on the host"}),
		"problem, no fix":  ProblemText(Problem{Title: "A title.", Detail: "A detail."}),
		"everything": startNoticeText([]ServePort{{Box: 1, Host: 2}, {Box: 3, Host: 4, Taken: true}},
			[]ServiceLink{{Name: "db", Scheme: "postgresql", Host: 5}}, &NetworkLimit{}, []Problem{{Title: "A title.", Command: "coop presets", Purpose: "on the host"}}),
		"server up":       ServeUpText(ServePort{Box: 1, Host: 2}, "npm run dev"),
		"server taken":    ServeUpText(ServePort{Box: 1, Host: 2, Taken: true}, "npm run dev"),
		"server loopback": ServeLoopbackText(ServePort{Box: 1, Host: 2}, "npm run dev"),
		"sign in":         signInText("grok@default"),
	} {
		t.Run(name, func(t *testing.T) { threadtext.Check(t, text) })
	}
	var html bytes.Buffer
	markdown := startNoticeText(nil, nil, &NetworkLimit{}, []Problem{{Title: "A title.", Detail: "A detail.", Command: "coop login grok@default", Purpose: "on the host"}})
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert([]byte(markdown), &html); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<p><strong>Internet is filtered.</strong></p>\n<p>Allowed: the agent's provider.</p>",
		"<pre><code>coop approve\n</code></pre>",
		"<p>⚠ <strong>A title.</strong></p>\n<p>A detail.</p>\n<p>Run this on the host:</p>\n<pre><code>coop login grok@default\n</code></pre>",
	} {
		if !strings.Contains(strings.ReplaceAll(html.String(), "&rsquo;", "'"), want) {
			t.Errorf("it does not render as %q:\n%s", want, html.String())
		}
	}
	// Whatever comes next in the same message, the code block has closed by then.
	c := newTestControl(t)
	notice := noticeTextRaw(t, c.statusMessage("S", ProblemText(Problem{Title: "A title.", Command: "coop presets", Purpose: "on the host"})))
	threadtext.Check(t, "The agent said this."+notice+"And then this.")
}
