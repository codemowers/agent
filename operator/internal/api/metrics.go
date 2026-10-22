package api

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Histograms of the requests the operator serves. Each replica observes its
// own; sum across replicas. Workers export their own metrics.
var (
	mcpProbeRoundtrip = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "agent_operator_mcp_probe_roundtrip_seconds",
		Help:    "Time from a dashboard MCP connectivity check request to the worker's report.",
		Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30},
	})
	agentMCPCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_operator_mcp_call_duration_seconds",
		Help:    "Time the operator took to serve an Agent MCP tool call, by tool and outcome.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tool", "outcome"})
)

// RegisterMetrics adds the API histograms to the operator's registry.
func RegisterMetrics(registry prometheus.Registerer) error {
	for _, collector := range []prometheus.Collector{mcpProbeRoundtrip, agentMCPCallDuration} {
		if err := registry.Register(collector); err != nil {
			return err
		}
	}
	return nil
}

func observeSince(observer prometheus.Observer, start time.Time) {
	observer.Observe(time.Since(start).Seconds())
}
