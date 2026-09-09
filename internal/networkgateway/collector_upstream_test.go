package networkgateway

import (
	"strings"
	"testing"
	"time"
)

func TestCollectorUpstreamEstablishmentFailuresNeedPositiveEvidence(t *testing.T) {
	for _, flag := range []string{"UF", "URX", "DC", ""} {
		t.Run(flag, func(t *testing.T) {
			c, now := collectorFixture(t)
			flow := strings.Repeat("b", 32)
			c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, nil, EnvoyTotals{})
			periodic := proxyEvent(1, *now, flow, "TcpPeriodic", 0, 0, 1000)
			c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{periodic}, EnvoyTotals{Sequence: 1})
			*now = now.Add(4 * time.Second)
			publishFixture(c, *now, nil)
			if c.proxyPartial || c.connections != 0 || c.upstreamFailures != 0 {
				t.Fatal("slow unconnected socket became a lost meter or failed connection")
			}
			end := proxyEvent(2, *now, flow, "TcpConnectionEnd", 0, 0, 4000)
			if flag != "" {
				end.Flags = []string{flag}
			}
			c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{end}, EnvoyTotals{Sequence: 2})
			publishFixture(c, *now, nil)
			s := c.Snapshot()
			if flag == "UF" || flag == "URX" {
				if *s.Counters.UpstreamFailures != 1 || *s.Counters.Connections != 0 || s.Coverage.ProxyBytes.Status != "exact" || s.Coverage.UpstreamFailures.Status != "exact" || s.Connections[0].State != "failed" {
					t.Fatal("explicit establishment failure mistaken for lost events")
				}
			} else if *s.Counters.UpstreamFailures != 0 || s.Coverage.UpstreamFailures.Status != "lower-bound" || s.Connections[0].Reason != "upstream_outcome_unavailable" {
				t.Fatal("absence of connected event invented an upstream failure")
			}
			c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{end}, EnvoyTotals{Sequence: 2})
			if c.upstreamFailures > 1 {
				t.Fatal("replayed failure counted twice")
			}
		})
	}
}

func TestCollectorActualConnectTimingWitnessAndResetRemainDistinct(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("c", 32)
	connectMillis := uint64(0) // zero is a valid measured duration, unlike absent
	end := proxyEvent(1, *now, flow, "TcpConnectionEnd", 10, 20, 5000)
	end.ConnectMillis, end.CloseType = &connectMillis, "RemoteReset"
	end.Flags = []string{"UR"}
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{end}, EnvoyTotals{Sequence: 1})
	publishFixture(c, *now, nil)
	s := c.Snapshot()
	if *s.Counters.Connections != 1 || *s.Counters.UpstreamFailures != 0 || *s.Connections[0].ConnectMillis != 0 || s.Connections[0].Reason != "upstream_reset" || s.Connections[0].Rate != nil || s.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("measured connect timing replaced with total duration or reset counted as establishment failure")
	}
}

func TestEnvoyTypedFailureFieldsRefuseUnknownOrAmbiguousValues(t *testing.T) {
	for _, bad := range []string{
		strings.Replace(envoyLine(), `"flags":"-"`, `"flags":"UF,UF"`, 1),
		strings.Replace(envoyLine(), `"flags":"-"`, `"flags":"UF,-"`, 1),
		strings.Replace(envoyLine(), `"flags":"-"`, `"flags":"untrusted text"`, 1),
		strings.Replace(envoyLine(), `"flags":"-"`, `"flags":""`, 1),
		strings.Replace(envoyLine(), `"connect_ms":"10"`, `"connect_ms":10`, 1),
		strings.Replace(envoyLine(), `"connect_ms":"10"`, `"connect_ms":"01"`, 1),
		strings.Replace(envoyLine(), `"close_type":"Normal"`, `"close_type":"peer.example.com"`, 1),
	} {
		if _, err := parseEnvoyEvent([]byte(bad)); err == nil {
			t.Fatal("invalid typed lifecycle evidence accepted")
		}
	}
	event, err := parseEnvoyEvent([]byte(strings.Replace(envoyLine(), `"flags":"-"`, `"flags":"UF,URX"`, 1)))
	if err != nil || len(event.Flags) != 2 || event.ConnectMillis == nil || *event.ConnectMillis != 10 {
		t.Fatal("valid pinned lifecycle fields rejected")
	}
}
