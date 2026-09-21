//go:build boxruntimee2e

package box

// Codex is the one pinned client with no offline CLI for what the conformance probes need to ask:
// its skill catalog and its MCP server status are both model-facing, and only its app-server answers
// them over stdio JSON-RPC. Two probes drive it, so the driving lives here once.
//
// The delicate part is when to close stdin. App-server exits the moment its stdin closes, taking any
// MCP server it spawned with it, so the feeder has to stay open until the answer is in. Both probes
// used to hold it open for a FIXED sleep, which is wrong in both directions: too short and a slow or
// freshly-built host closes stdin mid-handshake, which reads as "the client never launched the
// server" — a false red that aborts `provider-qualify` and looks exactly like a real regression;
// too long and every green run pays the worst case whether it needs to or not.
//
// So the feeder waits for the REPLY instead, and the fixed number left is only a give-up bound.

// codexAppServerLog is where the container keeps app-server's output: the feeder polls it for the
// reply, and it is printed afterwards so a failure shows what the client actually said.
const codexAppServerLog = "/tmp/appserver.out"

// codexAppServerDriver renders the shell that asks codex's app-server one question and holds stdin
// open exactly until it answers.
func codexAppServerDriver(method string) string {
	const (
		initialize  = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"coop-probe","title":"coop-probe","version":"1"}}}`
		initialized = `{"jsonrpc":"2.0","method":"initialized","params":{}}`
		// 0.2s × 450 ≈ 90s of patience — the same bound as the timeout below, so whichever notices
		// first, an app-server that never answers still ends the run instead of hanging it.
		await = `i=0; while [ $i -lt 450 ] && ! grep -q '"id":2' ` + codexAppServerLog + ` 2>/dev/null; do sleep 0.2; i=$((i+1)); done`
	)
	query := `{"jsonrpc":"2.0","id":2,"method":"` + method + `","params":{}}`
	return `export CODEX_HOME="$HOME/.codex"; { printf '%s\n' '` + initialize + `' '` + initialized + `' '` + query + `'; ` +
		await + `; } | timeout 90 codex app-server > ` + codexAppServerLog + ` 2>&1; cat ` + codexAppServerLog
}
