package box

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
)

const (
	// LabelBroker marks an open run's MCP credential broker helper (coop=broker). The helper carries
	// its box's supervisor, run and execution labels too, so whatever reaps an orphaned box reaps it.
	LabelBroker = "broker"
	// labelBrokerID names one helper exactly, so its removal needs nothing the runtime printed.
	labelBrokerID = "coop.broker"
	// openBrokerReadyTimeout bounds the wait for a started helper to report every listener bound.
	openBrokerReadyTimeout = 30 * time.Second
)

// openBroker keeps an open run's MCP secrets outside its box. An open box reaches the internet
// directly, so nothing on its path can hold a secret for it: Coop starts the gateway image's
// `coop-net broker` beside it — its own unprivileged container on the box's network — which holds
// each secret-bearing server's credential, answers only that server's stand-in and dials only that
// server. The box reaches it as networkgateway.OpenBrokerHost, through a hosts entry.
type openBroker struct {
	plan        *credentialPlan // MCP routes only: a provider key stays a filtered run's
	scrub       []string        // the brokered secrets' variables, which the box never carries
	substitutes []string        // route i's stand-in
	runID       string          // the helper generation its secrets file is bound to
	epoch       string
	dir         string // its private configuration directory
	address     netip.Addr
	helper      *runtime.Helper
	stderr      *tailBuffer
}

// planOpenBroker plans an open run's broker: a route for every secret-bearing server a fixed route
// can carry. A server it cannot carry — SSE, a secret in two places, a URL other than plain https on
// 443, or any server when the box cannot reach a helper — keeps its secret in the box as before, and
// kept says which and why for the launch to show. Nil without a route.
func planOpenBroker(cfg *config.Config, rt runtime.Runtime, spec RunSpec, snapshot []byte) (broker *openBroker, kept []string, err error) {
	servers, unbrokerable, err := mcp.SecretServers(snapshot)
	if err != nil {
		return nil, nil, err
	}
	stay := map[string]bool{} // variables a kept server still reads in the box
	for _, server := range unbrokerable {
		kept = append(kept, server.Reason)
		for _, name := range server.Variables {
			stay[name] = true
		}
	}
	if len(servers) == 0 {
		return nil, kept, nil
	}
	if reason := openBrokerUnreachable(rt, cfg.ExtraRunArgs, spec.ExtraArgs); reason != "" {
		for _, server := range servers {
			kept = append(kept, fmt.Sprintf("MCP server %q keeps its secret in the box: %s", server.Name, reason))
		}
		return nil, kept, nil
	}
	values := effectiveMCPEnv(cfg, spec)
	plan := &credentialPlan{}
	for _, server := range servers {
		route, reason, err := mcpRouteFor(server, values)
		if err != nil {
			// Even a secret with no value is not this run's to refuse: without a broker that
			// server simply failed to authenticate, and it still does. A filtered run refuses it
			// (planMCPRoutes), because there the box would otherwise reach nothing at all.
			reason, err = fmt.Sprintf("MCP server %q keeps its secret in the box: %s", server.Name, err), nil
		}
		if reason != "" {
			kept = append(kept, reason)
			stay[server.Variable] = true
			continue
		}
		plan.mcp = append(plan.mcp, route)
	}
	if len(plan.mcp) == 0 {
		return nil, kept, nil
	}
	b := &openBroker{plan: plan, runID: randomHex(16), epoch: randomHex(16)}
	for _, route := range plan.mcp {
		if !stay[route.variable] && !slices.Contains(b.scrub, route.variable) {
			b.scrub = append(b.scrub, route.variable)
		}
		b.substitutes = append(b.substitutes, randomHex(32))
	}
	for _, name := range b.scrub {
		if extraEnvAssigns(cfg.ExtraRunArgs, name) || extraEnvAssigns(spec.ExtraArgs, name) {
			return nil, nil, fmt.Errorf("%s holds an MCP server's token, which cannot enter an agent box through -e", name)
		}
	}
	// The hosts entry is how the box finds its helper, and the first match wins: an entry for the
	// same name in the run's own arguments would take the box's stand-ins somewhere else, where
	// they could be replayed against the real helper.
	if extraHostsEntry(cfg.ExtraRunArgs) || extraHostsEntry(spec.ExtraArgs) {
		return nil, nil, fmt.Errorf("%s is the name Coop's MCP credential broker answers to; remove its --add-host from the runtime arguments", networkgateway.OpenBrokerHost)
	}
	return b, kept, nil
}

// openBrokerUnreachable says why a box could not reach a helper beside it, or "" when it can: the
// helper is a Docker container, and a box sharing another container's network, or with none, has no
// route to a sibling. The runtime arguments are the operator's own; Coop leaves them as they are.
func openBrokerUnreachable(rt runtime.Runtime, argSets ...[]string) string {
	if !rt.SupportsFilteredNetwork() {
		return "Coop's credential broker runs on Docker, not " + filepath.Base(rt.Name)
	}
	if network := boxNetworkArg(argSets...); network == "none" || strings.HasPrefix(network, "container:") {
		return "the box's --network " + network + " cannot reach Coop's credential broker"
	}
	return ""
}

// extraHostsEntry reports whether runtime arguments bind the broker's own name themselves.
func extraHostsEntry(args []string) bool {
	for i := 0; i < len(args); i++ {
		flag, value, inline := strings.Cut(args[i], "=")
		if flag != "--add-host" {
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return false
			}
			i++
			value = args[i]
		}
		if name, _, _ := strings.Cut(value, ":"); name == networkgateway.OpenBrokerHost {
			return true
		}
	}
	return false
}

// boxNetworkArg is the last --network (or --net) the runtime arguments give the box, "" for none.
func boxNetworkArg(argSets ...[]string) string {
	network := ""
	for _, args := range argSets {
		for i := 0; i < len(args); i++ {
			flag, value, inline := strings.Cut(args[i], "=")
			if flag != "--network" && flag != "--net" {
				continue
			}
			if !inline {
				if i+1 >= len(args) {
					break
				}
				i++
				value = args[i]
			}
			network = value
		}
	}
	return network
}

// openBrokerNetwork is the network the helper joins: the one Coop gives the box, else the one the
// runtime arguments name, else the default bridge — which a box on the host's network reaches too.
func openBrokerNetwork(joined string, argSets ...[]string) string {
	if joined != "" {
		return joined
	}
	switch network := boxNetworkArg(argSets...); network {
	case "", "host", "default":
		return "bridge"
	default:
		return network
	}
}

// openBrokerListener is route i's listener as an open box names it.
func openBrokerListener(i int) string {
	return net.JoinHostPort(networkgateway.OpenBrokerHost, strconv.Itoa(networkgateway.CredentialBrokerPort+i))
}

// mcpStandIns is each brokered server's stand-in by the variable the box reads it from, and every
// other value of the box's MCP environment — what a server Coop could not broker still reads there.
func (b *openBroker) mcpStandIns(cfg *config.Config, spec RunSpec) mcpStandIns {
	standIns := mcpStandIns{epoch: b.epoch, values: make(map[string]string, len(b.plan.mcp)), kept: effectiveMCPEnv(cfg, spec)}
	for _, name := range b.scrub {
		delete(standIns.kept, name)
	}
	for j := range b.plan.mcp {
		standIns.values[b.plan.mcpTokenEnv(j)] = b.substitutes[j]
	}
	return standIns
}

// env is the box's environment without the brokered secrets, with each route's stand-in added.
func (b *openBroker) env(artifacts compositionArtifactOps, source string) (string, error) {
	content := ""
	if source != "" {
		data, err := os.ReadFile(source)
		if err != nil {
			return "", fmt.Errorf("read environment for the MCP credential broker: %w", err)
		}
		drop := make(map[string]bool, len(b.scrub))
		for _, name := range b.scrub {
			drop[name] = true
		}
		if content = strings.TrimRight(filteredEnvContent(data, drop), "\n"); content != "" {
			content += "\n"
		}
	}
	for j := range b.plan.mcp {
		content += b.plan.mcpTokenEnv(j) + "=" + b.substitutes[j] + "\n"
	}
	path, err := artifacts.writeFile(artifacts.parent, content)
	if err != nil {
		return "", fmt.Errorf("prepare the MCP credential broker environment: %w", err)
	}
	return path, nil
}

// legacySSEServers are the brokered servers still on the legacy transport; none without a broker.
func (b *openBroker) legacySSEServers() []string {
	if b == nil {
		return nil
	}
	return b.plan.legacySSEServers()
}

// servers are the MCP servers the helper keeps secrets for, in route order; none without a broker.
func (b *openBroker) servers() []string {
	if b == nil {
		return nil
	}
	return b.plan.mcpServerNames()
}

// name is the helper container's, unique to this helper generation.
func (b *openBroker) name() string { return "coop-broker-" + b.runID[:16] }

// labels are the helper's own: coop=broker, its exact generation, and the box's ownership labels.
func (b *openBroker) labels(owner []string) []string {
	return append([]string{"--label", LabelKey + "=" + LabelBroker, "--label", labelBrokerID + "=" + b.runID}, owner...)
}

// start writes the helper's configuration and secrets into a private directory, starts it on network
// and waits for it to report the address it listens on. It runs with an open stdin pipe, like the
// task channel: EOF — this Coop exiting, however it ends — stops it, and `--rm` removes it. Once
// start is called, the caller stops the helper on every path, whatever start returned.
func (b *openBroker) start(ctx context.Context, rt runtime.Runtime, repo, network string, owner []string, building func(), exposed ...string) error {
	image, err := ensureOpenBrokerImage(ctx, rt, building)
	if err != nil {
		return err
	}
	if b.dir, err = privateWorkspaceTempDir(repo, "coop-broker-", exposed...); err != nil {
		return fmt.Errorf("prepare the MCP credential broker: %w", err)
	}
	routes := b.plan.gatewayRoutes()
	secrets := networkgateway.CredentialBrokerSecrets{Version: 2, RunID: b.runID, Epoch: b.epoch}
	for i, route := range routes {
		secrets.Routes = append(secrets.Routes, networkgateway.CredentialBrokerSecret{Name: route.Name, Substitute: b.substitutes[i], Credential: b.plan.mcp[i].token})
		b.plan.mcp[i].token = ""
	}
	configData, configErr := json.Marshal(networkgateway.OpenBrokerConfig{Version: 1, RunID: b.runID, Epoch: b.epoch, Brokers: routes})
	secretData, secretErr := json.Marshal(secrets)
	secretData = append(secretData, '\n') // before the clear below, which must zero the buffer that is written
	for i := range secrets.Routes {
		secrets.Routes[i].Credential = ""
	}
	if err := errors.Join(configErr, secretErr); err != nil {
		return errors.New("encode the MCP credential broker configuration")
	}
	// Readable by the helper's own user, inside a directory only this user can enter.
	configPath, secretsPath := filepath.Join(b.dir, "broker.json"), filepath.Join(b.dir, "secrets.json")
	err = errors.Join(os.WriteFile(configPath, append(configData, '\n'), 0o444), os.WriteFile(secretsPath, secretData, 0o444))
	clear(secretData)
	if err != nil {
		return fmt.Errorf("prepare the MCP credential broker: %w", err)
	}
	args := []string{"run", "--rm", "-i", "--pull", "never", "--name", b.name(), "--user", "65532:65532", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--read-only", "--memory", "256m", "--pids-limit", "64", "--network", network}
	args = append(args, b.labels(owner)...)
	args = append(args, "--mount", "type=bind,source="+configPath+",target="+networkgateway.OpenBrokerConfigPath+",readonly",
		"--mount", "type=bind,source="+secretsPath+",target="+networkgateway.CredentialBrokerPath+",readonly", image, "broker")
	b.stderr = &tailBuffer{max: 8 << 10}
	if b.helper, err = rt.StartHelper(b.stderr, args...); err != nil {
		return fmt.Errorf("start the MCP credential broker: %w", err)
	}
	return b.waitReady()
}

// waitReady takes the one line the helper prints once every listener accepts: the address the box
// must be pointed at. A helper that exits instead fails at once, with what it printed.
func (b *openBroker) waitReady() error {
	ready := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(b.helper.Stdout).ReadString('\n')
		if err == nil {
			ready <- strings.TrimSpace(line)
		}
		close(ready)
	}()
	timer := time.NewTimer(openBrokerReadyTimeout)
	defer timer.Stop()
	select {
	case line := <-ready:
		address, err := netip.ParseAddr(line)
		if err != nil || !address.Is4() {
			return fmt.Errorf("the MCP credential broker named no usable address%s", b.stderrDetail())
		}
		b.address = address
		return nil
	case <-b.helper.Exited():
		return fmt.Errorf("the MCP credential broker stopped before it was ready%s", b.stderrDetail())
	case <-timer.C:
		_ = b.helper.Stdout.Close() // the line reader above has nothing left to wait for
		return fmt.Errorf("the MCP credential broker did not become ready within %s; no agent started%s", openBrokerReadyTimeout, b.stderrDetail())
	}
}

func (b *openBroker) stderrDetail() string {
	if b.stderr == nil {
		return ""
	}
	if detail := strings.TrimSpace(b.stderr.String()); detail != "" {
		return ": " + detail
	}
	return ""
}

// hostArgs binds the name the box's MCP configuration uses to the helper's address.
func (b *openBroker) hostArgs() []string {
	return []string{"--add-host=" + networkgateway.OpenBrokerHost + ":" + b.address.String()}
}

// stop ends the helper — closing its stdin, which is how it exits and how `--rm` removes it — and
// removes its private directory. A runtime that would not let go leaves the container for the label
// removal, and the box sweep behind that. A broker that never started has nothing to stop.
func (b *openBroker) stop(rt runtime.Runtime) error {
	if b == nil {
		return nil
	}
	var err error
	if b.helper != nil {
		if err = b.helper.Close(); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, removeErr := rt.RemoveByLabels(ctx, map[string]string{LabelKey: LabelBroker, labelBrokerID: b.runID})
			cancel()
			err = errors.Join(err, removeErr)
		}
	}
	if b.dir != "" {
		err = errors.Join(err, os.RemoveAll(b.dir))
	}
	return err
}

// ensureOpenBrokerImage is the gateway image the helper runs, built on first use as network setup
// builds it: from the release's embedded recipe and sources, never from the workspace.
func ensureOpenBrokerImage(ctx context.Context, rt runtime.Runtime, building func()) (string, error) {
	tag := gatewayimage.Tag()
	var label bytes.Buffer
	format := `{{index .Config.Labels "` + gatewayimage.BuildLabel + `"}}`
	if code, err := rt.RunInterruptible(ctx, nil, &label, io.Discard, "image", "inspect", "--format", format, tag); err == nil && code == 0 &&
		strings.TrimSpace(label.String()) == gatewayimage.Fingerprint() {
		return tag, nil
	}
	if building != nil {
		building()
	}
	source, err := gatewayimage.Context()
	if err != nil {
		return "", err
	}
	_, flags, err := rt.BuildPlatform()
	if err != nil {
		return "", err
	}
	args := append([]string{"build", "--label", gatewayimage.BuildLabel + "=" + gatewayimage.Fingerprint(), "-t", tag}, flags...)
	var stderr bytes.Buffer
	code, err := rt.RunInterruptible(ctx, bytes.NewReader(source), io.Discard, &stderr, append(args, "-")...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("build the MCP credential broker image: %s", boundedCause(lastLines(stderr.String(), 4), exitError(code, err)))
	}
	return tag, nil
}

func exitError(code int, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("exit status %d", code)
}

// lastLines is the end of a tool's output, where a failed build says why.
func lastLines(output string, n int) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func randomHex(size int) string {
	random := make([]byte, size)
	_, _ = rand.Read(random) // crypto/rand.Read never fails on a supported platform
	return hex.EncodeToString(random)
}
