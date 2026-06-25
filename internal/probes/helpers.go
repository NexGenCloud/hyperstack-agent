package probes

import (
	"fmt"

	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
)

func newSample(name string, labels map[string]string, value float64) metrics.Sample {
	return metrics.Sample{
		Name:   name,
		Labels: labels,
		Value:  fmt.Sprintf("%.6f", value),
	}
}
