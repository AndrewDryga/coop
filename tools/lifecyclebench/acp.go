package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// initializeParams is the editor's first request: it reads and writes no files through Coop.
var initializeParams = map[string]any{"protocolVersion": 1,
	"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}}}

// switchSettle is how long a switch case waits once the session is open. A person switches seconds
// after opening a session, not milliseconds: a parked box finishes starting, so the warm number is a
// switch, not the tail of a boot. Both cases wait, so the control runs on the same idle machine.
const switchSettle = 10 * time.Second

// The lines `coop acp` traces for a box it switches to (internal/cli/acp_cmd.go, internal/acpctl/warm.go,
// internal/acpproxy/proxy.go). The evidence of a warm hit is the product's own account of which box
// served the switch, never the timing.
var (
	spawnLine = regexp.MustCompile(`\| spawn: (warm|cold) box for ([\w.-]+)@(\S+)$`)
	liveLine  = regexp.MustCompile(`\| replay: ([\w.-]+)@(\S+) is live on (.+)$`)
	poolLine  = regexp.MustCompile(`\| warm pool: ([\w.-]+)@(\S+) parked$`)
)

// launchACP starts `coop acp` with the bench as its editor on stdio.
func (b *bench) launchACP(ours *ownership, extra, env []string) (*process, *acpClient, error) {
	cmd := b.coopCommand(ours.token, env, slices.Concat([]string{"acp"}, extra)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = writer
	p, err := b.begin(cmd, ours)
	writer.Close() // coop holds its own copy
	if err != nil {
		stdout.Close()
		return nil, nil, err
	}
	return p, newACPClient(stdin, stdout), nil
}

// finishACP ends an ACP launch the way an editor quits, by closing coop's input, then stops it.
// Coop's output is read until it is gone: closed earlier, a write to it would kill coop mid-teardown.
func (b *bench) finishACP(p *process, client *acpClient, ours *ownership, before set) {
	client.stdin.Close()
	b.finish(p, ours, before)
	client.stdout.Close()
}

// caseACPInitialize times how long an editor waits before Coop's ACP agent answers it.
//
// This is the editor-facing half of "start": the editor speaks ACP over stdio and cannot do anything
// until `initialize` comes back. It is measured with the real protocol rather than by watching for a
// process, because a process that exists has not answered anybody. Note what is inside the number:
// the proxy spawns the lead's box immediately, so this includes a real box and its ACP adapter, and
// the warm pool may be fanning other providers out concurrently.
func (b *bench) caseACPInitialize(ctx context.Context, extra []string) (sample, error) {
	before, err := b.owned()
	if err != nil {
		return sample{}, err
	}
	ours := b.newOwnership()
	started := time.Now()
	p, client, err := b.launchACP(ours, extra, nil)
	if err != nil {
		return sample{}, err
	}
	defer b.finishACP(p, client, ours, before)
	client.send("initialize", initializeParams)
	got, answered, err := client.wait(ctx, 0, b.caseTimeout, func(m map[string]any) bool { return initializeResult(m) != nil })
	if err != nil {
		return sample{}, err
	}
	if !answered {
		return failed("the ACP agent never answered initialize"), nil
	}
	answer := initializeResult(got.message)
	agent, found := answer["agentInfo"]
	if !found {
		if agent, found = answer["agent"]; !found {
			agent = map[string]any{}
		}
	}
	return measured(got.at.Sub(started), fact{"agent", agent}), nil
}

// initializeResult is the result of the answer to request 1, or nil. Parsed, not substring-matched:
// a notification or a log line that merely contains the right characters is not an answer.
func initializeResult(message map[string]any) map[string]any {
	if !hasID(message, 1) {
		return nil
	}
	result, _ := message["result"].(map[string]any)
	return result
}

func hasID(message map[string]any, id int) bool {
	value, ok := message["id"].(float64)
	return ok && value == float64(id)
}

func (b *bench) caseACPSwitchCold(ctx context.Context, extra []string) (sample, error) {
	return b.caseACPSwitch(ctx, extra, false)
}

func (b *bench) caseACPSwitchWarm(ctx context.Context, extra []string) (sample, error) {
	return b.caseACPSwitch(ctx, extra, true)
}

// caseACPSwitch times an editor's provider switch: from the Provider selector's request to the new
// provider's replayed session — the moment the editor can prompt it.
//
// Two boxes can serve it. A cold one is started for the switch; a warm one was started and parked in
// the background earlier, so the switch pays only the replay. COOP_ACP_WARM=0 turns the pool off,
// which makes every switch cold — the control. The case waits for the pool's own "parked" line
// before a warm switch, and a sample counts only when the trace says the box it measured is the kind
// it set out to measure.
//
// The evidence is coop's ACP trace, written under the user's Coop config directory. The case deletes
// its own trace file, but turning tracing on also applies Coop's trace retention there: only the
// newest five acp-trace logs are kept.
func (b *bench) caseACPSwitch(ctx context.Context, extra []string, warm bool) (sample, error) {
	before, err := b.owned()
	if err != nil {
		return sample{}, err
	}
	ours := b.newOwnership()
	env := []string{"COOP_ACP_TRACE=1"}
	if !warm {
		env = append(env, "COOP_ACP_WARM=0")
	}
	p, client, err := b.launchACP(ours, extra, env)
	if err != nil {
		return sample{}, err
	}
	trace := b.tracePath(p.cmd.Process.Pid)
	defer func() {
		b.finishACP(p, client, ours, before)
		_ = os.Remove(trace)
		_ = os.Remove(trace + ".1")
	}()
	greeting, answered, err := client.request(ctx, "initialize", initializeParams, b.caseTimeout)
	if err != nil {
		return sample{}, err
	}
	if _, ok := greeting.message["result"]; !answered || !ok {
		return failed("the ACP agent never answered initialize"), nil
	}
	cwd, _ := filepath.Abs(b.repo)
	opened, answered, err := client.request(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, b.caseTimeout)
	if err != nil {
		return sample{}, err
	}
	result, _ := opened.message["result"].(map[string]any)
	if !answered || result == nil {
		return failed("session/new failed"), nil
	}
	session, _ := result["sessionId"].(string)
	lead, choices := providerChoices(result)
	target := ""
	if i := slices.IndexFunc(choices, func(choice string) bool { return choice != lead }); i >= 0 {
		target = choices[i]
	}
	if session == "" || target == "" {
		return failed("no second signed-in provider to switch to"), nil
	}
	if warm {
		for deadline := time.Now().Add(b.caseTimeout); !poolReady(readTrace(trace, 0), target); {
			if time.Now().After(deadline) || p.ended(0) {
				return failed("the warm pool never parked " + target), nil
			}
			if err := sleep(ctx, 200*time.Millisecond); err != nil {
				return sample{}, err
			}
		}
	}
	if err := sleep(ctx, switchSettle); err != nil {
		return sample{}, err
	}
	var offset int64
	if info, err := os.Stat(trace); err == nil {
		offset = info.Size()
	}
	from := client.mark()
	requested := time.Now()
	ack, acked, err := client.request(ctx, "session/set_config_option",
		map[string]any{"sessionId": session, "configId": "coop_provider", "value": target}, b.caseTimeout)
	if err != nil {
		return sample{}, err
	}
	live, switched, err := client.wait(ctx, from, b.caseTimeout, func(m map[string]any) bool { return providerUpdate(m, session, target) })
	if err != nil {
		return sample{}, err
	}
	if _, ok := ack.message["result"]; !acked || !ok || !switched {
		return failed("the switch to " + target + " never completed"), nil
	}
	served := switchEvidence(readTrace(trace, offset), target)
	known := func(key string) string {
		if value := served[key]; value != "" {
			return value
		}
		return "unknown"
	}
	facts := []fact{{"lead", lead}, {"provider", target}, {"account", known("account")}, {"adapter", known("adapter")},
		{"box", known("box")}, {"ack_seconds", round(ack.at.Sub(requested).Seconds(), 3)}}
	seconds := live.at.Sub(requested)
	wanted := "cold"
	if warm {
		wanted = "warm"
	}
	if served["box"] != wanted {
		// Still a real switch, timed: kept as evidence of the miss, out of the case's statistics.
		return failed(fmt.Sprintf("wanted a %s box, but a %s box served the switch in %.3fs", wanted, known("box"),
			seconds.Seconds()), facts...), nil
	}
	return measured(seconds, facts...), nil
}

// providerChoices is the session's current provider and the providers its Provider selector offers
// (session/new).
func providerChoices(result map[string]any) (string, []string) {
	options, _ := result["configOptions"].([]any)
	for _, entry := range options {
		option, _ := entry.(map[string]any)
		if option["id"] != "coop_provider" {
			continue
		}
		current, _ := option["currentValue"].(string)
		var values []string
		choices, _ := option["options"].([]any)
		for _, entry := range choices {
			choice, _ := entry.(map[string]any)
			if value, _ := choice["value"].(string); value != "" {
				values = append(values, value)
			}
		}
		return current, values
	}
	return "", nil
}

// providerUpdate is whether a message is the config_option_update that shows session running on
// provider — what the proxy sends once the new box has replayed the session.
func providerUpdate(message map[string]any, session, provider string) bool {
	params, _ := message["params"].(map[string]any)
	if message["method"] != "session/update" || params == nil || params["sessionId"] != session {
		return false
	}
	update, _ := params["update"].(map[string]any)
	if update == nil || update["sessionUpdate"] != "config_option_update" {
		return false
	}
	current, _ := providerChoices(update)
	return current == provider
}

// tracePath is where `coop acp` writes its trace when COOP_ACP_TRACE is set (internal/acpproxy/trace.go).
func (b *bench) tracePath(pid int) string {
	root := b.getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "coop", fmt.Sprintf("acp-trace-%d.log", pid))
}

// readTrace is the trace from offset on, empty when there is none.
func readTrace(path string, offset int64) string {
	data, err := os.ReadFile(path)
	if err != nil || offset > int64(len(data)) {
		return ""
	}
	return string(data[offset:])
}

// switchEvidence is which box served a switch to provider, on which account, running which adapter —
// read from the trace written after the switch was requested.
func switchEvidence(trace, provider string) map[string]string {
	evidence := map[string]string{}
	for line := range strings.Lines(trace) {
		line = strings.TrimRight(line, "\r\n")
		if spawn := spawnLine.FindStringSubmatch(line); spawn != nil && spawn[2] == provider && evidence["box"] == "" {
			evidence["box"], evidence["account"] = spawn[1], spawn[3]
		}
		if live := liveLine.FindStringSubmatch(line); live != nil && live[1] == provider && evidence["adapter"] == "" {
			evidence["adapter"], evidence["live_account"] = live[3], live[2]
		}
	}
	return evidence
}

func poolReady(trace, provider string) bool {
	for line := range strings.Lines(trace) {
		if pool := poolLine.FindStringSubmatch(strings.TrimRight(line, "\r\n")); pool != nil && pool[1] == provider {
			return true
		}
	}
	return false
}

// acpClient is just enough of an ACP client to drive `coop acp` over stdio: requests by id, and every
// message kept in arrival order with the host instant it arrived, so a case can wait for a
// notification that follows a mark. A goroutine reads coop's output, because a blocking read cannot
// time out.
type acpClient struct {
	stdin  io.WriteCloser
	stdout *os.File
	nextID int

	mu       sync.Mutex
	messages []arrival
	closed   bool
	changed  chan struct{} // closed and replaced whenever a message arrives or the output ends
}

type arrival struct {
	at      time.Time
	message map[string]any
}

func newACPClient(stdin io.WriteCloser, stdout *os.File) *acpClient {
	c := &acpClient{stdin: stdin, stdout: stdout, changed: make(chan struct{})}
	go c.read()
	return c
}

func (c *acpClient) read() {
	reader := bufio.NewReader(c.stdout)
	for {
		line, err := reader.ReadBytes('\n')
		at := time.Now()
		var message map[string]any
		if json.Unmarshal(line, &message) == nil && message != nil {
			c.publish(func() { c.messages = append(c.messages, arrival{at, message}) })
		}
		if err != nil {
			c.publish(func() { c.closed = true })
			return
		}
	}
}

func (c *acpClient) publish(change func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	change()
	close(c.changed)
	c.changed = make(chan struct{})
}

// mark is where a later wait starts looking.
func (c *acpClient) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

// send writes one request and returns its id. A write that fails means coop has gone, which the wait
// for its answer reports.
func (c *acpClient) send(method string, params any) int {
	c.nextID++
	data, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", c.nextID, method, params})
	_, _ = c.stdin.Write(append(data, '\n'))
	return c.nextID
}

// wait is the first message from index from on that matches, with the instant it arrived; false when
// none did before timeout or the end of coop's output.
func (c *acpClient) wait(ctx context.Context, from int, timeout time.Duration, match func(map[string]any) bool) (arrival, bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		for ; from < len(c.messages); from++ {
			if match(c.messages[from].message) {
				found := c.messages[from]
				c.mu.Unlock()
				return found, true, nil
			}
		}
		closed, changed := c.closed, c.changed
		c.mu.Unlock()
		if closed {
			return arrival{}, false, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return arrival{}, false, nil
		case <-ctx.Done():
			return arrival{}, false, errInterrupted
		}
	}
}

// request sends method and waits for its answer, a result or an error.
func (c *acpClient) request(ctx context.Context, method string, params any, timeout time.Duration) (arrival, bool, error) {
	from := c.mark()
	id := c.send(method, params)
	return c.wait(ctx, from, timeout, func(m map[string]any) bool {
		_, result := m["result"]
		_, failure := m["error"]
		return hasID(m, id) && (result || failure)
	})
}
