package probes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sampleVLLMMetrics is a trimmed excerpt of a real vLLM /metrics response,
// covering the shapes that need to be dropped (python_*, process_*, http_*,
// vllm:*_created) alongside the ones that should survive renaming.
const sampleVLLMMetrics = `# HELP python_gc_objects_collected_total Objects collected during gc
# TYPE python_gc_objects_collected_total counter
python_gc_objects_collected_total{generation="0"} 15020.0
# HELP process_resident_memory_bytes Resident memory size in bytes.
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 7.74701056e+08
# HELP http_request_duration_highr_seconds_bucket Latency histogram
# TYPE http_request_duration_highr_seconds_bucket histogram
http_request_duration_highr_seconds_bucket{le="0.01"} 0.0
# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="tiny"} 0.0
# HELP vllm:estimated_flops_per_gpu_created Estimated number of floating point operations per GPU.
# TYPE vllm:estimated_flops_per_gpu_created gauge
vllm:estimated_flops_per_gpu_created{engine="0",model_name="tiny"} 1.788e+09
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{engine="0",finished_reason="stop",model_name="tiny"} 4.0
# HELP vllm:time_to_first_token_seconds_bucket Time to first token histogram
# TYPE vllm:time_to_first_token_seconds_bucket histogram
vllm:time_to_first_token_seconds_bucket{engine="0",le="0.01",model_name="tiny"} 2.0
`

func TestVLLMProbeRenamesAndFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleVLLMMetrics))
	}))
	defer srv.Close()

	probe := VLLMProbe{Endpoint: srv.URL}
	samples, err := probe.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	got := map[string]bool{}
	for _, s := range samples {
		got[s.Name] = true
	}

	wantPresent := []string{
		"hyperstack_dedicated_inference_num_requests_running",
		"hyperstack_dedicated_inference_request_success_total",
		"hyperstack_dedicated_inference_time_to_first_token_seconds_bucket",
	}
	for _, name := range wantPresent {
		if !got[name] {
			t.Errorf("expected sample %q to be present, got %v", name, got)
		}
	}

	wantAbsent := []string{
		"python_gc_objects_collected_total",
		"process_resident_memory_bytes",
		"http_request_duration_highr_seconds_bucket",
		"vllm:estimated_flops_per_gpu_created",
		"hyperstack_dedicated_inference_estimated_flops_per_gpu_created",
	}
	for _, name := range wantAbsent {
		if got[name] {
			t.Errorf("expected sample %q to be dropped, but it was present", name)
		}
	}

	if len(samples) != len(wantPresent) {
		t.Errorf("len(samples) = %d, want %d (%v)", len(samples), len(wantPresent), got)
	}
}

func TestVLLMProbeName(t *testing.T) {
	if got := (VLLMProbe{}).Name(); got != "dedicated_inference" {
		t.Fatalf("Name() = %q, want %q", got, "dedicated_inference")
	}
}

func TestVLLMProbeCollectNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := VLLMProbe{Endpoint: srv.URL}
	if _, err := probe.Collect(context.Background()); err == nil {
		t.Fatal("expected error for non-200 response, got nil")
	}
}
