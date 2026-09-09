package collectors

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/client"
	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
	"github.com/NexGenCloud/hyperstack-agent/internal/probes"
	"github.com/NexGenCloud/hyperstack-agent/internal/system"
)

// VLLMCollector scrapes a local vLLM (Hyperstack Dedicated Inference) server.
// Unlike GPU/Node, whether this VM should scrape at all is decided by the
// gateway at runtime (not local hardware detection), so it carries its own
// gate defaulting to disabled until the gateway confirms this is a
// Dedicated Inference VM.
type VLLMCollector struct {
	Hub   *client.HubClient
	Probe probes.Probe

	gate atomic.Bool
}

func (v *VLLMCollector) SetEnabled(enabled bool) { v.gate.Store(enabled) }

func (v *VLLMCollector) IsEnabled() bool { return v.gate.Load() }

func (v *VLLMCollector) Run(ctx context.Context) error {
	if v.Probe == nil {
		return errors.New("vllm collector missing probe implementation")
	}

	if !v.IsEnabled() {
		slog.Debug("dedicated inference collector skipped", "reason", "gate disabled")
		return nil
	}

	start := time.Now()
	slog.Debug("dedicated inference probe: begin")
	samples, err := v.Probe.Collect(ctx)
	recordDuration := time.Since(start)
	system.SetLastScrapeMs(recordDuration.Milliseconds())
	if err != nil {
		system.RecordProbeFailure(v.Probe.Name(), err)
		slog.Warn("dedicated inference probe failed", "error", err)
		return err
	}
	filtered := metrics.FilterSamples(samples)
	if len(filtered) == 0 {
		system.RecordProbeFailure(v.Probe.Name(), errors.New("no dedicated inference samples collected"))
		return nil
	}
	system.RecordProbeSuccess(v.Probe.Name())
	slog.Debug("dedicated inference probe: samples", "count", len(filtered), "duration_ms", recordDuration.Milliseconds())

	if v.Hub != nil {
		now := time.Now().UTC()
		measures := metrics.SamplesToMeasures(filtered, now, v.Probe.Name())
		v.Hub.EnqueueMetrics(measures)
		slog.Info(
			"metrics enqueued",
			"collector", v.Probe.Name(),
			"samples", len(filtered),
		)
	}
	return nil
}
