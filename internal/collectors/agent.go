package collectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/client"
	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
	"github.com/NexGenCloud/hyperstack-agent/internal/system"
)

const agentMetricPrefix = "hyperstack_agent_"

type AgentCollector struct {
	Hub               *client.HubClient
	PathTemplate      string
	EnabledCollectors map[string]bool
}

func (a *AgentCollector) Run(ctx context.Context) error {
	if a.Hub == nil {
		return errors.New("agent collector missing hub client")
	}

	now := time.Now().UTC()
	measures := metrics.SamplesToMeasures(a.samples(), now, "agent")
	if len(measures) == 0 {
		return nil
	}

	a.Hub.EnqueueMetrics(measures)
	slog.Info(
		"metrics enqueued",
		"collector", "agent",
		"samples", len(measures),
	)
	return nil
}

func (a *AgentCollector) samples() []metrics.Sample {
	version, date := system.GetBuildInfo()
	samples := []metrics.Sample{
		agentSample(agentMetricPrefix+"up", nil, 1),
		agentSample(agentMetricPrefix+"build_info", map[string]string{
			"version": version,
			"date":    date,
		}, 1),
		agentSample(agentMetricPrefix+"collectors_running", nil, float64(a.Hub.GetCollectorsRunning())),
		agentSample(agentMetricPrefix+"samples_submitted_total", nil, float64(a.Hub.GetSamplesSent())),
		agentSample(agentMetricPrefix+"samples_failed_total", nil, float64(a.Hub.GetSamplesFailed())),
		agentSample(agentMetricPrefix+"last_scrape_ms", nil, float64(system.GetLastScrapeMs())),
		agentSample(agentMetricPrefix+"last_submit_ms", nil, float64(system.GetLastSubmitMs())),
	}

	collectorNames := make([]string, 0, len(a.EnabledCollectors))
	for name := range a.EnabledCollectors {
		collectorNames = append(collectorNames, name)
	}
	sort.Strings(collectorNames)
	for _, name := range collectorNames {
		value := 0.0
		if a.EnabledCollectors[name] {
			value = 1.0
		}
		samples = append(samples, agentSample(
			agentMetricPrefix+"collector_enabled",
			map[string]string{"name": name},
			value,
		))
	}

	return samples
}

func agentSample(name string, labels map[string]string, value float64) metrics.Sample {
	return metrics.Sample{
		Name:   name,
		Labels: labels,
		Value:  fmt.Sprintf("%.6f", value),
	}
}
