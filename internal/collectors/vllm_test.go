package collectors

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
)

type stubProbe struct {
	name    string
	calls   atomic.Int64
	samples []metrics.Sample
	err     error
}

func (p *stubProbe) Name() string { return p.name }

func (p *stubProbe) Collect(context.Context) ([]metrics.Sample, error) {
	p.calls.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	return p.samples, nil
}

func TestVLLMCollectorSkipsProbeWhenGateDisabled(t *testing.T) {
	probe := &stubProbe{name: "dedicated_inference"}
	c := &VLLMCollector{Probe: probe}

	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := probe.calls.Load(); got != 0 {
		t.Fatalf("probe calls = %d, want 0 when gate disabled", got)
	}
}

func TestVLLMCollectorRunsProbeWhenGateEnabled(t *testing.T) {
	probe := &stubProbe{
		name: "dedicated_inference",
		samples: []metrics.Sample{
			{Name: "hyperstack_dedicated_inference_num_requests_running", Value: "1"},
		},
	}
	c := &VLLMCollector{Probe: probe}
	c.SetEnabled(true)

	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := probe.calls.Load(); got != 1 {
		t.Fatalf("probe calls = %d, want 1 when gate enabled", got)
	}
}

func TestVLLMCollectorPropagatesProbeError(t *testing.T) {
	probe := &stubProbe{name: "dedicated_inference", err: errors.New("scrape failed")}
	c := &VLLMCollector{Probe: probe}
	c.SetEnabled(true)

	if err := c.Run(context.Background()); err == nil {
		t.Fatal("expected error to propagate from probe, got nil")
	}
}

func TestVLLMCollectorGateDefaultsDisabled(t *testing.T) {
	c := &VLLMCollector{}
	if c.IsEnabled() {
		t.Fatal("IsEnabled() = true, want false by default")
	}
}
