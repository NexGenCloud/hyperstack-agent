package collectors

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/client"
	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
	"github.com/NexGenCloud/hyperstack-agent/internal/probes"
	"github.com/NexGenCloud/hyperstack-agent/internal/system"
)

type NodeCollector struct {
	Hub   *client.HubClient
	Probe probes.Probe
}

func (n *NodeCollector) Run(ctx context.Context) error {
	if n.Probe == nil {
		return errors.New("node collector missing probe implementation")
	}

	start := time.Now()
	slog.Debug("node probe: begin")
	samples, err := n.Probe.Collect(ctx)
	recordDuration := time.Since(start)
	system.SetLastScrapeMs(recordDuration.Milliseconds())
	if err != nil {
		system.RecordProbeFailure(n.Probe.Name(), err)
		slog.Warn("node probe failed", "error", err)
		return err
	}
	filtered := metrics.FilterSamples(samples)
	system.RecordProbeSuccess(n.Probe.Name())
	slog.Debug("node probe: samples", "count", len(filtered), "duration_ms", recordDuration.Milliseconds())

	if n.Hub != nil && len(filtered) > 0 {
		now := time.Now().UTC()
		measures := metrics.SamplesToMeasures(filtered, now, n.Probe.Name())
		n.Hub.EnqueueMetrics(measures)
		slog.Info(
			"metrics enqueued",
			"collector", n.Probe.Name(),
			"samples", len(filtered),
		)
	}
	return nil
}
