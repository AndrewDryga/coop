//go:build networkruntimee2e

package box

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// This opt-in test is the release-level runtime matrix, not the per-host
// preflight: it drives the private smoke seam against an explicitly constructed
// image pair and publishes nothing. `coop net setup` records the host proof.
func TestRestrictedNetworkRuntime(t *testing.T) {
	root := os.Getenv("COOP_NETWORK_TRIAL_STATE")
	if root == "" {
		t.Skip("requires COOP_NETWORK_TRIAL_STATE and a local Docker daemon")
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	build, cancelBuild := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancelBuild()
	docker, err := runtime.BindDocker(build, runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := BuildNetworkCandidate(build, docker, nil, nil)
	_ = docker.Close()
	if err != nil {
		t.Fatal(err)
	}
	clients := qualifiedClients(t, candidate)
	for _, name := range []string{"enforcement", "guard-loss"} {
		t.Run(name, func(t *testing.T) {
			smoke, err := store.BeginQualification(candidate, clients)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "filtered", Memory: "256m", Pids: "128", CPUs: "1", NoNewPrivileges: true}
			mode := egress.Filtered
			policy, err := store.Admit(repo, networkstate.Admission{InvocationMode: &mode, Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator"}, Rules: []egress.Rule{{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			var registered networkstate.Execution
			registeredCh := make(chan string, 1)
			permit := &networkSmokeLaunch{authority: smoke, registered: func(r networkstate.Execution) { registered = r; registeredCh <- r.ID }}
			faultDone := make(chan error, 1)
			if name == "guard-loss" {
				go func() {
					select {
					case runID := <-registeredCh:
						faultDone <- injectQualifiedGuardLoss(ctx, store, runID)
					case <-ctx.Done():
						faultDone <- ctx.Err()
					}
				}()
			}
			script := `set -eu
test "$COOP_BOX" = 1
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
curl -q --proxy '' --noproxy '*' --fail --silent --show-error --max-time 15 https://example.com -o /tmp/allowed.html
test -s /tmp/allowed.html
if curl -q --proxy '' --noproxy '*' --silent --max-time 3 https://example.org -o /dev/null; then exit 31; fi
if curl -q --proxy '' --noproxy '*' --silent --max-time 3 --resolve example.org:443:1.1.1.1 https://example.org -o /dev/null; then exit 32; fi
`
			if name == "guard-loss" {
				script = "sleep 60"
			}
			var output bytes.Buffer
			beforeTopology, err := filteredHostAddresses()
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			code, runErr := runWithNetworkSmoke(cfg, runtime.Runtime{Name: "docker"}, RunSpec{
				Repo: repo, Workdir: "/workspace", Batch: true, Quiet: true, Ctx: ctx, Stdout: &output, Stderr: io.Discard,
				CapturedEgress: &CapturedEgress{Store: store, Project: repo, Fingerprint: policy.Fingerprint}, Cmd: []string{"sh", "-c", script},
			}, defaultCompositionArtifactOps(), permit)
			if runErr != nil {
				afterTopology, topologyErr := filteredHostAddresses()
				if topologyErr != nil || !slices.Equal(beforeTopology, afterTopology) {
					t.Logf("host topology observation before=%v after=%v error=%v", beforeTopology, afterTopology, topologyErr)
				}
			}
			if name == "guard-loss" {
				if err := <-faultDone; err != nil {
					t.Fatal("intended guard fault was not injected", err)
				}
				if runErr == nil || ctx.Err() != nil {
					t.Fatal("unrelated timeout or normal exit substituted for intended fault", runErr, ctx.Err())
				}
			} else if runErr != nil || code != 0 {
				t.Fatal("proxy-free traffic fixture failed", code, runErr)
			}
			r, err := store.Execution(registered.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Purpose != "qualification" || r.ClientImage != candidate.ClientImage || r.GatewayImage != candidate.GatewayImage ||
				r.Receipt == nil || r.Receipt.Finality != "final" || !r.WorkloadStarted || r.ReadySequence == 0 {
				t.Fatal("lost bound runtime/startup evidence")
			}
			for _, resource := range r.Resources {
				if resource.State != "gone" {
					t.Fatalf("resource cleanup pending: %s", resource.Role)
				}
			}
			if r.Artifact.State != "gone" {
				t.Fatal("launch artifact cleanup pending")
			}
			if name == "guard-loss" {
				if r.Receipt.Completeness != "partial" || r.Receipt.Workload != "runtime_failed" || !r.Receipt.Snapshot.Loss.Unknown ||
					!slices.Contains(r.Receipt.Snapshot.Loss.Reasons, "terminal_observation_unavailable") ||
					!slices.Contains(r.Receipt.Snapshot.Loss.Reasons, "observer_end_after_workload_unproven") {
					t.Fatal("guard loss invented complete evidence")
				}
			} else {
				if r.Receipt.Completeness != "complete" || r.Receipt.Workload != "exited" {
					t.Fatalf("normal traffic receipt incomplete: completeness=%s workload=%s loss=%v unknown=%t availability=%s",
						r.Receipt.Completeness, r.Receipt.Workload, r.Receipt.Snapshot.Loss.Reasons, r.Receipt.Snapshot.Loss.Unknown, r.Receipt.Snapshot.Availability)
				}
				checkQualifiedTraffic(t, r.Receipt.Snapshot, policy.Grants[0].ID)
			}
			t.Logf("case=%s run=%s image=%s duration_ms=%d receipt=%s cleanup=complete", name, r.ID, candidate.ClientImage, time.Since(started).Milliseconds(), r.Receipt.Completeness)
		})
	}
}

func injectQualifiedGuardLoss(ctx context.Context, store *networkstate.Store, runID string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		r, err := store.Execution(runID)
		if err != nil {
			return err
		}
		if r.WorkloadStarted && r.ReadySequence != 0 && r.Snapshot.Availability == "available" {
			control, stop := context.WithTimeout(ctx, 15*time.Second)
			defer stop()
			docker, err := runtime.BindDocker(control, runtime.Runtime{Name: "docker"}, r.Endpoint, r.DaemonID)
			if err != nil {
				return err
			}
			defer docker.Close()
			var guard runtime.DockerRef
			for _, role := range []string{"agent", "guard"} {
				i := slices.IndexFunc(r.Resources, func(resource networkstate.Resource) bool { return resource.Role == role })
				resource := r.Resources[i]
				ref := runtime.DockerRef{Name: resource.Name, ID: resource.ID, Labels: map[string]string{"coop.network.run": r.ID, "coop.network.epoch": r.Epoch, "coop.network.scope": r.Scope, "coop.network.role": role}}
				value, exists, err := docker.InspectContainer(control, ref)
				if err != nil || !exists || !value.State.Running || value.State.Paused || value.State.StartedAt.IsZero() {
					return errors.New("fault target not confirmed running")
				}
				if role == "guard" {
					guard = ref
				}
			}
			// A live response from the exact guard establishes readiness now,
			// without comparing the Docker VM's wall clock with the host's.
			if _, err := docker.ExecRead(control, guard, 4096, "/usr/local/bin/coop-net", "probe"); err != nil {
				return errors.Join(errors.New("fault target not confirmed ready"), err)
			}
			return docker.RemoveContainer(control, guard)
		}
		if r.Receipt != nil {
			return errors.New("trial ended before intended fault")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func checkQualifiedTraffic(t *testing.T, snapshot networkview.Snapshot, ruleID string) {
	t.Helper()
	allowed := slices.ContainsFunc(snapshot.Connections, func(c networkview.Connection) bool {
		return c.Name == "example.com" && c.NameSource == "sni" && c.Transport == "tls" && c.RuleID == ruleID && c.State == "closed" && !c.Partial &&
			c.SentBytes != nil && *c.SentBytes > 0 && c.ReceivedBytes != nil && *c.ReceivedBytes > 0
	})
	if !allowed {
		t.Fatal("allowed request lacks matching measured connection evidence")
	}
	for _, kind := range []string{"dns_denied", "tls_denied"} {
		if !slices.ContainsFunc(snapshot.Denials, func(d networkview.Denial) bool {
			return d.Name == "example.org" && d.Source == "guard" && d.Basis == "observed" && d.Kind == kind && d.Reason == "unapproved_name" &&
				(kind == "dns_denied" && d.Port == nil || kind == "tls_denied" && d.Port != nil && *d.Port == 443)
		}) {
			t.Fatalf("missing separately correlated %s", kind)
		}
	}
	if snapshot.Counters == nil || snapshot.Counters.DeniedDNSQueries == nil || *snapshot.Counters.DeniedDNSQueries == 0 || snapshot.Counters.DeniedTLS == nil || *snapshot.Counters.DeniedTLS == 0 {
		t.Fatal("denial counters did not increase")
	}
}
