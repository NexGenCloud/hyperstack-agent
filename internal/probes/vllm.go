package probes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
)

const diMetricPrefix = "hyperstack_dedicated_inference_"

// vllmDropSuffixes excludes prometheus_client's auto-generated "_created"
// gauges, which track counter/histogram creation time and carry no signal.
var vllmDropSuffixes = []string{"_created"}

var defaultVLLMHTTPClient = &http.Client{Timeout: 5 * time.Second}

// VLLMProbe scrapes a vLLM OpenAI-compatible server's /metrics endpoint and
// re-namespaces its "vllm:" metrics to match the Hyperstack Dedicated
// Inference product naming.
type VLLMProbe struct {
	Endpoint string
	Client   *http.Client
}

func (VLLMProbe) Name() string { return "dedicated_inference" }

func (p VLLMProbe) Collect(ctx context.Context) ([]metrics.Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dedicated inference scrape status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	parsed := metrics.ParsePrometheusText(string(body))
	return renameVLLMSamples(parsed), nil
}

func (p VLLMProbe) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return defaultVLLMHTTPClient
}

// renameVLLMSamples keeps only vLLM's own "vllm:"-namespaced series (dropping
// the python_*/process_*/http_* metrics that ship with every prometheus_client
// app) and maps them onto the hyperstack_dedicated_inference_ prefix.
func renameVLLMSamples(samples []metrics.Sample) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(samples))
	for _, s := range samples {
		if !strings.HasPrefix(s.Name, "vllm:") {
			continue
		}
		if hasAnySuffix(s.Name, vllmDropSuffixes) {
			continue
		}
		out = append(out, metrics.Sample{
			Name:   diMetricPrefix + strings.TrimPrefix(s.Name, "vllm:"),
			Labels: s.Labels,
			Value:  s.Value,
		})
	}
	return out
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
