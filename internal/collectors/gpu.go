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

type GPUCollector struct {
	Hub   *client.HubClient
	Probe probes.Probe
}

func (g *GPUCollector) Run(ctx context.Context) error {
	if g.Probe == nil {
		return errors.New("gpu collector missing probe implementation")
	}

	start := time.Now()
	slog.Debug("gpu probe: begin")
	samples, err := g.Probe.Collect(ctx)
	recordDuration := time.Since(start)
	system.SetLastScrapeMs(recordDuration.Milliseconds())
	if err != nil {
		system.RecordProbeFailure(g.Probe.Name(), err)
		slog.Warn("gpu probe failed", "error", err)
		return err
	}
	filtered := metrics.FilterSamples(samples)
	if len(filtered) == 0 {
		system.RecordProbeFailure(g.Probe.Name(), errors.New("no gpu samples collected"))
		return nil
	}
	system.RecordProbeSuccess(g.Probe.Name())
	slog.Debug("gpu probe: samples", "count", len(filtered), "duration_ms", recordDuration.Milliseconds())

	if g.Hub != nil {
		now := time.Now().UTC()
		measures := metrics.SamplesToMeasures(filtered, now, g.Probe.Name())
		g.Hub.EnqueueMetrics(measures)
		slog.Info(
			"metrics enqueued",
			"collector", g.Probe.Name(),
			"samples", len(filtered),
		)
	}
	return nil
}
