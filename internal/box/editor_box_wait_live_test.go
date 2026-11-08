//go:build networkruntimee2e

package box

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// The editor box wait on a real Docker: a real coop acp, driven over stdio as an editor drives it, through
// a project box that cannot start yet. These sign in with the developer's real Claude account, so they
// are live, but a prompt is cancelled the moment it reaches the box and spends almost nothing. The
// runs are recorded in the developer's own Coop state like any run: a filtered launch needs this
// host's network setup. Each scenario uses a new path, because approval is keyed by path and bytes.

var (
	liveCoopOnce sync.Once
	liveCoopPath string
	liveCoopErr  error
)

func liveCoop(t *testing.T) string {
	t.Helper()
	liveCoopOnce.Do(func() {
		dir, err := os.MkdirTemp(teststate.Dir(), "coop-box-wait-")
		if err != nil {
			liveCoopErr = err
			return
		}
		liveCoopPath = filepath.Join(dir, "coop")
		build := exec.Command("go", "build", "-o", liveCoopPath, "github.com/AndrewDryga/coop")
		build.Dir = "../.."
		if out, err := build.CombinedOutput(); err != nil {
			liveCoopErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if liveCoopErr != nil {
		t.Fatal(liveCoopErr)
	}
	return liveCoopPath
}

// liveCoopEnv is the environment a person's coop sees: their home and folders, not this test binary's.
func liveCoopEnv() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "HOME" || key == "XDG_CONFIG_HOME" || key == "XDG_STATE_HOME" || strings.HasPrefix(key, "COOP_") {
			continue
		}
		env = append(env, item)
	}
	return append(env, "HOME="+teststate.DeveloperHome(), "XDG_CONFIG_HOME="+teststate.DeveloperConfigHome(),
		"XDG_STATE_HOME="+teststate.DeveloperStateHome(), teststate.LiveEnv+"=1", "COOP_ACP_TRACE=1", "NO_COLOR=1")
}

// waitEditor is the editor's side of one session.
type waitEditor struct {
	t       *testing.T
	repo    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  string
	lines   chan map[string]any
	answers map[float64]map[string]any
	said    []string
	asked   []string // what the session asked the editor for, other than showing it something
}

func startWaitEditor(t *testing.T, repo string) *waitEditor {
	t.Helper()
	return startEditor(t, repo, "claude")
}

func startEditor(t *testing.T, repo string, args ...string) *waitEditor {
	t.Helper()
	e := &waitEditor{t: t, repo: repo, lines: make(chan map[string]any, 1024), answers: map[float64]map[string]any{},
		stderr: filepath.Join(t.TempDir(), "stderr.log")}
	e.cmd = exec.Command(liveCoop(t), append([]string{"acp"}, args...)...)
	e.cmd.Dir, e.cmd.Env = repo, liveCoopEnv()
	logFile, err := os.Create(e.stderr)
	if err != nil {
		t.Fatal(err)
	}
	e.cmd.Stderr = logFile
	if e.stdin, err = e.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := e.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(e.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		for scanner.Scan() {
			var message map[string]any
			if json.Unmarshal(scanner.Bytes(), &message) == nil {
				e.lines <- message
			}
		}
	}()
	t.Cleanup(func() {
		_ = e.stdin.Close()
		done := make(chan error, 1)
		go func() { done <- e.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the session did not end cleanly: %v\n%s", err, e.log())
			}
		case <-time.After(2 * time.Minute):
			_ = e.cmd.Process.Kill()
			t.Error("the session did not end within two minutes")
		}
		_ = logFile.Close()
	})
	return e
}

func (e *waitEditor) send(message map[string]any) {
	e.t.Helper()
	message["jsonrpc"] = "2.0"
	data, err := json.Marshal(message)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.stdin.Write(append(data, '\n')); err != nil {
		e.t.Fatalf("write to the session: %v\n%s", err, e.log())
	}
}

// take reads one message and records what it says, or reports that the time ran out.
func (e *waitEditor) take(until time.Time) (map[string]any, bool) {
	select {
	case message, open := <-e.lines:
		if !open {
			e.t.Fatalf("the session ended early:\n%s", e.log())
		}
		if id, ok := message["id"].(float64); ok {
			if _, isResult := message["method"]; !isResult {
				e.answers[id] = message
			}
		}
		if method, ok := message["method"].(string); ok && method != "session/update" {
			e.asked = append(e.asked, method)
		}
		if params, ok := message["params"].(map[string]any); ok {
			if update, ok := params["update"].(map[string]any); ok && update["sessionUpdate"] == "agent_message_chunk" {
				if content, ok := update["content"].(map[string]any); ok {
					if text, ok := content["text"].(string); ok {
						e.said = append(e.said, text)
					}
				}
			}
		}
		return message, true
	case <-time.After(time.Until(until)):
		return nil, false
	}
}

func (e *waitEditor) answer(id float64, within time.Duration) map[string]any {
	e.t.Helper()
	for until := time.Now().Add(within); ; {
		if answer, ok := e.answers[id]; ok {
			return answer
		}
		if _, ok := e.take(until); !ok {
			e.t.Fatalf("no answer to request %v within %s:\n%s", id, within, e.log())
		}
	}
}

func (e *waitEditor) hear(text string, within time.Duration) {
	e.t.Helper()
	for until := time.Now().Add(within); ; {
		if slices.ContainsFunc(e.said, func(s string) bool { return strings.Contains(s, text) }) {
			return
		}
		if _, ok := e.take(until); !ok {
			e.t.Fatalf("the thread never said %q within %s; it said %q:\n%s", text, within, e.said, e.log())
		}
	}
}

// log is the session's stderr and the trace it names there.
func (e *waitEditor) log() string {
	data, _ := os.ReadFile(e.stderr)
	text := string(data)
	if path := regexp.MustCompile(`Recording an ACP trace\n\s*(\S+)`).FindStringSubmatch(text); path != nil {
		if trace, err := os.ReadFile(path[1]); err == nil {
			text += string(trace)
		}
	}
	return text
}

func (e *waitEditor) waitLog(text string, within time.Duration) {
	e.t.Helper()
	for until := time.Now().Add(within); time.Now().Before(until); time.Sleep(200 * time.Millisecond) {
		if strings.Contains(e.log(), text) {
			return
		}
		e.drain()
	}
	e.t.Fatalf("the session never logged %q within %s:\n%s", text, within, e.log())
}

func (e *waitEditor) drain() {
	for {
		select {
		case <-e.lines:
		default:
			return
		}
	}
}

func (e *waitEditor) connect() string {
	e.t.Helper()
	return e.connectOffering(false)
}

// connectOffering opens a session, offering it the editor's computer or not.
func (e *waitEditor) connectOffering(host bool) string {
	e.t.Helper()
	e.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": host, "writeTextFile": host}, "terminal": host}}})
	e.answer(1, 5*time.Minute)
	e.send(map[string]any{"id": 2, "method": "session/new", "params": map[string]any{"cwd": e.repo, "mcpServers": []any{}}})
	answer := e.answer(2, 5*time.Minute)
	result, _ := answer["result"].(map[string]any)
	id, _ := result["sessionId"].(string)
	if id == "" {
		e.t.Fatalf("session/new = %v", answer)
	}
	return id
}

func (e *waitEditor) prompt(session string) {
	e.t.Helper()
	e.send(map[string]any{"id": 10, "method": "session/prompt", "params": map[string]any{"sessionId": session,
		"prompt": []any{map[string]any{"type": "text", "text": "Reply with the single word ok."}}}})
}

func (e *waitEditor) cancel(session string) string {
	e.t.Helper()
	e.send(map[string]any{"method": "session/cancel", "params": map[string]any{"sessionId": session}})
	answer := e.answer(10, 4*time.Minute)
	result, _ := answer["result"].(map[string]any)
	reason, _ := result["stopReason"].(string)
	return reason
}

// reachedTheBox waits for the move off the holding child and for the held prompt to be forwarded, then
// cancels it so the turn costs almost nothing.
func (e *waitEditor) reachedTheBox(session string) {
	e.t.Helper()
	e.waitLog("moving off the child", 4*time.Minute)
	for until := time.Now().Add(4 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
		log := e.log()
		if moved := strings.Index(log, "moving off the child"); moved >= 0 && strings.Contains(log[moved:], "session/prompt") {
			break
		}
		if time.Now().After(until) {
			e.t.Fatalf("the held prompt never reached the replacement box:\n%s", log)
		}
		e.drain()
	}
	if reason := e.cancel(session); reason != "cancelled" && reason != "end_turn" {
		e.t.Fatalf("the held prompt ended with %q, want it answered:\n%s", reason, e.log())
	}
}

// waitRepo is a new project at a new path with the given files, committed.
func waitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo, git := gitrepo.New(t)
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "fixture")
	return repo
}

func waitRun(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, liveCoopEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// projectImages is what this project has built, by image id.
func projectImages(t *testing.T, repo string) []string {
	t.Helper()
	out, err := exec.Command("docker", "images", "--no-trunc", "--format", "{{.Repository}} {{.ID}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	var images []string
	for _, line := range strings.Split(string(out), "\n") {
		if name, id, ok := strings.Cut(line, " "); ok && strings.HasPrefix(name, ComposeProject(repo)) && strings.HasSuffix(name, "-filtered") {
			images = append(images, id)
		}
	}
	slices.Sort(images)
	return images
}

func removeProjectImages(t *testing.T, repo string) {
	t.Cleanup(func() {
		for _, image := range projectImages(t, repo) {
			_ = exec.Command("docker", "rmi", "-f", image).Run()
		}
	})
}

func waitDockerfile() string {
	return "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\nUSER root\nRUN echo " + randid.Hex(8) + " > /coop-box-wait\nUSER node\n"
}

func requireLiveBoxWait(t *testing.T) {
	t.Helper()
	if os.Getenv("COOP_NETWORK_TRIAL_STATE") == "" {
		t.Skip("requires make network-runtime-e2e: a local Docker daemon, this host's network setup and a signed-in Claude")
	}
	cmd := exec.Command(liveCoop(t), "credentials")
	cmd.Env = liveCoopEnv()
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), "Claude\n") {
		t.Skip("this suite's editor box wait needs a signed-in Claude on this host")
	}
}

// New build files: the thread says what to run, a prompt waits with no answer, coop build lets it move.
func TestEditorBoxWaitHoldsAPromptUntilTheProjectIsBuilt(t *testing.T) {
	requireLiveBoxWait(t)
	repo := waitRepo(t, map[string]string{".agent/Dockerfile": waitDockerfile()})
	removeProjectImages(t, repo)
	editor := startWaitEditor(t, repo)
	session := editor.connect()
	editor.hear("needs a build", time.Minute)
	editor.prompt(session)
	editor.waitLog("holding editor prompt", time.Minute)
	if _, answered := editor.answers[10]; answered {
		t.Fatal("the held prompt was answered while the box could not start")
	}
	waitRun(t, repo, liveCoop(t), "build", "--egress", "filtered")
	editor.reachedTheBox(session)
}

// Changed build files after an approved build: a prompt waits, Coop builds nothing by itself, and
// cancelling the prompt ends it.
func TestEditorBoxWaitBuildsNothingForChangedFiles(t *testing.T) {
	requireLiveBoxWait(t)
	repo := waitRepo(t, map[string]string{".agent/Dockerfile": waitDockerfile()})
	removeProjectImages(t, repo)
	waitRun(t, repo, liveCoop(t), "build", "--egress", "filtered")
	before := projectImages(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte(waitDockerfile()), 0o644); err != nil {
		t.Fatal(err)
	}
	editor := startWaitEditor(t, repo)
	session := editor.connect()
	editor.hear("needs a build", time.Minute)
	editor.prompt(session)
	editor.waitLog("holding editor prompt", time.Minute)
	time.Sleep(35 * time.Second) // past the wait's recheck
	if after := projectImages(t, repo); !slices.Equal(after, before) {
		t.Fatalf("an image was built for changed files: %v, was %v", after, before)
	}
	if strings.Contains(editor.log(), "Rebuilding") {
		t.Fatalf("the session tried to rebuild changed files:\n%s", editor.log())
	}
	if reason := editor.cancel(session); reason != "cancelled" {
		t.Fatalf("cancelling the held prompt ended it with %q", reason)
	}
}

// Project services with no rules: the thread names the remedies, a prompt waits, and turning the
// service network off lets it move.
func TestEditorBoxWaitHoldsAPromptUntilServicesAreSettled(t *testing.T) {
	requireLiveBoxWait(t)
	repo := waitRepo(t, map[string]string{
		".agent/compose.yml":  "services:\n  db:\n    image: postgres:17-alpine\n    ports:\n      - \"5432\"\n",
		".agent/project.yaml": "box:\n",
	})
	editor := startWaitEditor(t, repo)
	session := editor.connect()
	editor.hear("cannot start yet", time.Minute)
	editor.hear("coop approve", time.Minute)
	editor.prompt(session)
	editor.waitLog("holding editor prompt", time.Minute)
	file, err := os.OpenFile(filepath.Join(repo, ".agent", "project.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("  network: false\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	editor.reachedTheBox(session)
}

// A box that starts and never answers: every node process in this image blocks at startup, the adapter
// included, and the clients stay byte-identical, so the image's proofs accept it. The editor's first
// request is answered by a holding child in about a minute, the thread says why, a message waits, and a
// fixed Dockerfile moves the session onto a box that answers. Nothing ever reaches a model.
func TestEditorBoxWaitMovesOffABoxThatNeverAnswers(t *testing.T) {
	requireLiveBoxWait(t)
	hung := "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\nUSER root\nRUN echo " + randid.Hex(8) + " > /coop-box-wait\n" +
		"RUN printf 'Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0);\\n' > /opt/hang.js\nENV NODE_OPTIONS=--require=/opt/hang.js\nUSER node\n"
	repo := waitRepo(t, map[string]string{".agent/Dockerfile": hung})
	removeProjectImages(t, repo)
	waitRun(t, repo, liveCoop(t), "build", "--egress", "filtered")
	editor := startWaitEditor(t, repo)
	connecting := time.Now()
	session := editor.connect()
	if took := time.Since(connecting); took < time.Minute || took > 3*time.Minute {
		t.Fatalf("the editor was answered %s after connecting, want about a minute:\n%s", took, editor.log())
	}
	editor.hear("did not answer", time.Minute)
	if !strings.Contains(editor.log(), "so the proxy stopped it") {
		t.Fatalf("the trace does not say the box was stopped:\n%s", editor.log())
	}
	editor.prompt(session)
	editor.waitLog("holding editor prompt", time.Minute)
	if reason := editor.cancel(session); reason != "cancelled" {
		t.Fatalf("cancelling the held prompt ended it with %q", reason)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte(waitDockerfile()), 0o644); err != nil {
		t.Fatal(err)
	}
	waitRun(t, repo, liveCoop(t), "build", "--egress", "filtered")
	// The notice goes out with the next session update, and a new thread is one. A thread opened while a
	// box is being tried is refused and can ask again, as an editor's person can.
	heard := func() bool {
		return slices.ContainsFunc(editor.said, func(s string) bool { return strings.Contains(s, "is ready") })
	}
	for id, until := 20.0, time.Now().Add(5*time.Minute); !heard(); id++ {
		if time.Now().After(until) {
			t.Fatalf("the session never said its box is ready; it said %q:\n%s", editor.said, editor.log())
		}
		editor.send(map[string]any{"id": id, "method": "session/new", "params": map[string]any{"cwd": repo, "mcpServers": []any{}}})
		editor.answer(id, 3*time.Minute)
		for wait := time.Now().Add(5 * time.Second); !heard(); {
			if _, ok := editor.take(wait); !ok {
				break
			}
		}
	}
}

// A project file that does not load: the editor connects, the thread says what is wrong and where, a prompt
// waits, and the fixed file lets it move with nothing else to do. The session then publishes the port the
// file names, which it could not read when it connected.
func TestEditorBoxWaitHoldsAPromptUntilTheProjectFileIsFixed(t *testing.T) {
	requireLiveBoxWait(t)
	repo := waitRepo(t, map[string]string{".agent/project.yaml": "box: [broken\n"})
	editor := startWaitEditor(t, repo)
	session := editor.connect()
	editor.hear("has a problem", time.Minute)
	editor.hear("line 1", time.Minute)
	editor.prompt(session)
	editor.waitLog("holding editor prompt", time.Minute)
	if _, answered := editor.answers[10]; answered {
		t.Fatal("the held prompt was answered while the file did not load")
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("serve:\n  ports: [4173]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	editor.reachedTheBox(session)
	editor.hear("Port 4173 now reaches this box", time.Minute)
}

// writeOntoTheAdapter does what any program in the session's box can: write lines straight onto the
// adapter's stdout, as if the adapter had said them.
func writeOntoTheAdapter(t *testing.T, repo string, lines ...string) {
	t.Helper()
	var boxes []string
	for until := time.Now().Add(time.Minute); len(boxes) == 0; time.Sleep(time.Second) {
		if time.Now().After(until) {
			t.Fatal("no box has this project mounted")
		}
		out, err := exec.Command("docker", "ps", "--filter", "label="+LabelKey+"="+LabelBox, "--format", "{{.Names}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range strings.Fields(string(out)) {
			mounts, err := exec.Command("docker", "inspect", name, "--format", "{{range .Mounts}}{{.Source}} {{end}}").Output()
			if err == nil && strings.Contains(string(mounts), repo) {
				boxes = append(boxes, name)
			}
		}
	}
	const script = `for f in /proc/[0-9]*/cmdline; do
  d=${f%/cmdline}
  if tr '\000' ' ' <"$f" 2>/dev/null | grep -q acp && printf '%s\n' "$FORGED" 2>/dev/null >"$d/fd/1"; then exit 0; fi
done
exit 1`
	// A session can also hold parked boxes for a switch; only the live one's lines go anywhere.
	for _, name := range boxes {
		cmd := exec.Command("docker", "exec", "-e", "FORGED="+strings.Join(lines, "\n"), name, "sh", "-c", script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("write onto the adapter's stdout in %s: %v\n%s", name, err, out)
		}
	}
}

// The way out of a filtered box through the editor is closed: a program in the box can ask, and nothing
// reaches the editor, while what it may say still does. The agent still edits the project, which this
// suite pays one short prompt on the cheapest model to prove.
// No session's box reaches the host through the editor, whatever its network mode: an open box is still
// a box.
func TestRestrictedNetworkBoxCannotReachTheHostThroughTheEditor(t *testing.T) {
	requireLiveBoxWait(t)
	for _, mode := range []string{"filtered", "open"} {
		t.Run(mode, func(t *testing.T) {
			repo := waitRepo(t, map[string]string{"README.md": "# A small project\n"})
			editor := startEditor(t, repo, "claude:haiku", "--egress", mode)
			session := editor.connectOffering(true)
			marker := "said-from-the-box-" + randid.Hex(8)
			writeOntoTheAdapter(t, repo,
				`{"jsonrpc":"2.0","id":"forged-1","method":"terminal/create","params":{"sessionId":"`+session+`","command":"touch","args":["/tmp/forged"]}}`,
				`{"jsonrpc":"2.0","id":"forged-2","method":"fs/write_text_file","params":{"sessionId":"`+session+`","path":"/tmp/forged","content":"x"}}`,
				`{"jsonrpc":"2.0","id":"forged-3","method":"fs/read_text_file","params":{"sessionId":"`+session+`","path":"/etc/passwd"}}`,
				`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"`+session+`","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+marker+`"}}}}`)
			// Lines keep their order, so what the box asked for came before this if it came at all.
			editor.hear(marker, time.Minute)
			if len(editor.asked) != 0 {
				t.Fatalf("the box reached the editor: asked for %v", editor.asked)
			}
			// The guard hides from the editor whatever it refuses, so its trace tells whether the agent asked too.
			refused := regexp.MustCompile(`refused "(fs|terminal)/`)
			forged := len(refused.FindAllString(editor.log(), -1))
			if forged == 0 {
				t.Fatalf("nothing was refused, so nothing was forged:\n%s", editor.log())
			}
			if mode != "filtered" {
				return // one real turn proves the agent works without the editor's computer
			}
			editor.send(map[string]any{"id": 10, "method": "session/prompt", "params": map[string]any{"sessionId": session, "prompt": []any{
				map[string]any{"type": "text", "text": "Create a file named notes.txt in this project containing the single word done. Do nothing else."}}}})
			answer := editor.answer(10, 4*time.Minute)
			if result, _ := answer["result"].(map[string]any); result["stopReason"] != "end_turn" {
				t.Fatalf("the turn ended with %v:\n%s", answer, editor.log())
			}
			if asked := len(refused.FindAllString(editor.log(), -1)); len(editor.asked) != 0 || asked != forged {
				t.Fatalf("the agent asked the editor for %v, and %d requests were refused after the %d forged:\n%s", editor.asked, asked-forged, forged, editor.log())
			}
			body, err := os.ReadFile(filepath.Join(repo, "notes.txt"))
			if err != nil || !strings.Contains(string(body), "done") {
				t.Fatalf("the agent did not edit the project: %q, %v\n%s", body, err, editor.log())
			}
		})
	}
}
