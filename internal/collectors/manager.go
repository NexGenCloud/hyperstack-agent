package collectors

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/jitter"
)

type Collector interface {
	Run(ctx context.Context) error
}

type ScheduledCollector struct {
	Collector Collector
	Interval  time.Duration
}

type Manager struct {
	Scheduled []ScheduledCollector
	// JitterFraction, if >0, adds up to Interval*JitterFraction random delay per tick
	JitterFraction float64
	disabled       atomic.Bool
}

func (m *Manager) SetEnabled(enabled bool) {
	m.disabled.Store(!enabled)
}

func (m *Manager) IsEnabled() bool {
	return !m.disabled.Load()
}

func (m *Manager) Run(ctx context.Context) error {
	if m.JitterFraction <= 0 {
		m.JitterFraction = 0.10
	}
	// Start each scheduled collector in its own loop
	for _, sc := range m.Scheduled {
		interval := sc.Interval
		if interval <= 0 {
			interval = 15 * time.Second
		}
		// capture
		scLocal := sc
		// derive collector name for logging
		collectorName := "unknown"
		switch scLocal.Collector.(type) {
		case *NodeCollector:
			collectorName = "node"
		case *GPUCollector:
			collectorName = "gpu"
		case *AgentCollector:
			collectorName = "agent"
		}
		go func() {
			slog.Debug("collector starting", "collector", collectorName, "interval", interval.String())
			initialJitter := jitter.Duration(time.Duration(float64(interval) * m.JitterFraction))
			select {
			case <-ctx.Done():
				return
			case <-time.After(initialJitter):
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					perTickJitter := jitter.Duration(time.Duration(float64(interval) * m.JitterFraction))
					select {
					case <-ctx.Done():
						return
					case <-time.After(perTickJitter):
					}
				start := time.Now()
				slog.Debug("collector tick", "collector", collectorName, "jitter_ms", int(perTickJitter/time.Millisecond))
				if !m.IsEnabled() {
					slog.Debug("collector skipped; metrics disabled", "collector", collectorName)
					continue
				}
				if err := scLocal.Collector.Run(ctx); err != nil && ctx.Err() == nil {
						slog.Error("collector run error", "collector", collectorName, "error", err)
					} else {
						slog.Debug("collector run ok", "collector", collectorName, "duration_ms", int(time.Since(start)/time.Millisecond))
					}
				}
			}
		}()
	}
	<-ctx.Done()
	return ctx.Err()
}
