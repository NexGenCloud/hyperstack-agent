package metrics

import (
	"testing"
	"time"
)

func TestFilterSamples_Prefixes(t *testing.T) {
	in := []Sample{
		{Name: "hyperstack_node_cpu_seconds_total"},
		{Name: "hyperstack_nvidia_gpu_utilization_gpu_percent"},
		{Name: "random_metric"},
	}
	out := FilterSamples(in)
	if len(out) != 2 {
		t.Fatalf("unexpected filtered length: %+v", out)
	}
	if out[0].Name != "hyperstack_node_cpu_seconds_total" {
		t.Fatalf("expected node metric to pass filter: %+v", out)
	}
	if out[1].Name != "hyperstack_nvidia_gpu_utilization_gpu_percent" {
		t.Fatalf("filtering failed: %+v", out)
	}
}

func TestSamplesToMeasures_CollectorLabel(t *testing.T) {
	samples := []Sample{
		{
			Name:   "hyperstack_agent_up",
			Labels: map[string]string{"foo": "bar"},
			Value:  "1",
		},
	}

	measures := SamplesToMeasures(samples, time.Unix(123, 0), "agent")
	if len(measures) != 1 {
		t.Fatalf("unexpected measures length: %+v", measures)
	}
	if got := measures[0].Labels["collector"]; got != "agent" {
		t.Fatalf("collector label = %q, want %q", got, "agent")
	}
	if _, ok := measures[0].Labels["exporter"]; ok {
		t.Fatalf("unexpected exporter label present: %+v", measures[0].Labels)
	}
}
