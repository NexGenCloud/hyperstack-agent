package probes

import (
	"context"

	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
)

// Probe represents an in-process collector that returns Prometheus-style samples.
type Probe interface {
	Name() string
	Collect(ctx context.Context) ([]metrics.Sample, error)
}
