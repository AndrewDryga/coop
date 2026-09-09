package networkgateway

import (
	"context"
	"strings"
	"testing"
)

func TestCollectorTerminalRequiresPerFlowEndingEvenWhenLogPipeDrained(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		c, now := collectorFixture(t)
		flow := strings.Repeat("f", 32)
		c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{proxyEvent(1, *now, flow, "TcpUpstreamConnected", 10, 20, 50)}, EnvoyTotals{Sequence: 1, Stopped: stopped})
		c.terminal = true
		publishFixture(c, *now, nil)
		s := c.Snapshot()
		if !s.Terminal || !s.Loss.Unknown || s.Coverage.ProxyBytes.Status != "lower-bound" || len(c.flows) != 0 || len(s.Connections) != 1 || !s.Connections[0].Partial || s.Connections[0].Rate != nil || s.Connections[0].Reason != "proxy_end_missing" {
			t.Fatal("terminal sample invented final meters or left a live flow")
		}
		if stopped && (s.Connections[0].State != "closed" || s.Health.Gateway.Status != "stopped") {
			t.Fatal("reaped proxy not distinguished from failed shutdown")
		}
		if !stopped && s.Connections[0].State != "unknown" {
			t.Fatal("unconfirmed shutdown claimed stream closed")
		}
	}
}

func TestCollectorInvalidFinalCutoffCannotRevertToLiveObservation(t *testing.T) {
	c, _ := collectorFixture(t)
	c.inventory = func() ([]SocketRow, error) { return nil, nil }
	c.envoyTotals.Stopped = true
	c.envoy.Finish(nil)
	c.sample(context.Background(), false, true, 0)
	s := c.Snapshot()
	if !s.Terminal || s.Coverage.KernelPackets.Status != "unavailable" || s.Sources[2].Status != "unavailable" || s.Health.Gateway.Status != "stopped" {
		t.Fatal("invalid final clock silently used nonterminal cached counters")
	}
}

func TestCollectorNormalFinalSnapshotIsStoppedNotNewGatewayFailure(t *testing.T) {
	c, now := collectorFixture(t)
	c.envoyTotals.Stopped, c.terminal = true, true
	publishFixture(c, *now, nil)
	s := c.Snapshot()
	if !s.Terminal || s.Health.Gateway.Status != "stopped" || s.Availability != "available" || s.Coverage.ProxyBytes.Status != "exact" || s.Loss.Unknown || len(s.Alerts) != 0 {
		t.Fatal("clean terminal observation became outage or partial traffic")
	}
}
