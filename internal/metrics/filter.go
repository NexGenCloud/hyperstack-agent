package metrics

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ImportantPrefixes mirrors Python agent's allowlist to control cardinality/payload.
var ImportantPrefixes = []string{
	"hyperstack_node_cpu_",
	"hyperstack_node_memory_",
	"hyperstack_node_filesystem_",
	"hyperstack_node_disk_",
	"hyperstack_node_network_",
	"hyperstack_node_load",
	"hyperstack_node_vmstat_",
	"hyperstack_node_netstat_",
	"hyperstack_node_sockstat_",
	"hyperstack_node_uname_info",
	"hyperstack_node_boot_time_seconds",
	"hyperstack_node_time_seconds",
	// Self-metrics
	"hyperstack_agent_",
	// GPU
	"hyperstack_nvidia_gpu_",
	"nvidia_gpu_",
	"nvidia_smi_",
	"gpu_",
}

// FilterSamples applies the important prefix filter and returns the subset of samples.
func FilterSamples(samples []Sample) []Sample {
	filtered := make([]Sample, 0, len(samples))
	for _, s := range samples {
		if hasAnyPrefix(s.Name, ImportantPrefixes) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// Measure represents a single timeseries datum ready for downstream ingestion.
type Measure struct {
	MetricID  string            `json:"metric_id"`
	Timestamp float64           `json:"timestamp"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels"`
}

// SamplesToMeasures converts filtered Prometheus samples into deterministic measures.
// Metric UUIDs are derived from metric name + sorted labels so the same series always maps to the same ID.
func SamplesToMeasures(samples []Sample, ts time.Time, collector string) []Measure {
	if len(samples) == 0 {
		return nil
	}
	baseTS := float64(ts.UTC().UnixNano()) / 1e9
	measures := make([]Measure, 0, len(samples))
	for _, s := range samples {
		val, err := strconv.ParseFloat(strings.TrimSpace(s.Value), 64)
		if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
			continue
		}
		baseLabels := make(map[string]string, len(s.Labels)+1)
		for k, v := range s.Labels {
			baseLabels[k] = v
		}
		baseLabels["metric"] = s.Name

		labels := make(map[string]string, len(baseLabels)+1)
		for k, v := range baseLabels {
			labels[k] = v
		}
		if collector != "" {
			labels["collector"] = collector
		}

		measures = append(measures, Measure{
			MetricID:  metricIdentifier(s.Name, baseLabels),
			Timestamp: baseTS,
			Value:     val,
			Labels:    labels,
		})
	}
	return measures
}

var sanitizePattern = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func metricIdentifier(metric string, labels map[string]string) string {
	if metric == "" {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := []string{sanitizeToken(metric)}
	for _, k := range keys {
		if k == "" {
			continue
		}
		v := strings.TrimSpace(labels[k])
		if v == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s_%s", sanitizeToken(k), sanitizeToken(v)))
	}
	base := strings.Trim(strings.Join(parts, "__"), "_")
	if base == "" {
		return metricUUID(metric, labels)
	}
	if len(base) <= 240 {
		return base
	}
	hash := metricUUID(metric, labels)
	return fmt.Sprintf("%s__%s", strings.TrimRight(base[:240], "_"), hash[:12])
}

func sanitizeToken(token string) string {
	token = sanitizePattern.ReplaceAllString(token, "_")
	return strings.Trim(token, "_")
}

func metricUUID(metric string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte(metric))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
	}
	sum := h.Sum(nil)
	u := make([]byte, 16)
	copy(u, sum[:16])
	// Set RFC 4122 variant (10xx) and version 8 (custom SHA-256-based).
	u[6] = (u[6] & 0x0f) | 0x80
	u[8] = (u[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%02x%02x%02x%02x%02x%02x",
		binary.BigEndian.Uint32(u[0:4]),
		binary.BigEndian.Uint16(u[4:6]),
		binary.BigEndian.Uint16(u[6:8]),
		binary.BigEndian.Uint16(u[8:10]),
		u[10], u[11], u[12], u[13], u[14], u[15],
	)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}

// DeepCopyMeasure creates a deep copy of a Measure, including its Labels map.
func DeepCopyMeasure(m Measure) Measure {
	labelsCopy := make(map[string]string, len(m.Labels))
	for k, v := range m.Labels {
		labelsCopy[k] = v
	}
	return Measure{
		MetricID:  m.MetricID,
		Timestamp: m.Timestamp,
		Value:     m.Value,
		Labels:    labelsCopy,
	}
}

// DeepCopyMeasures creates a deep copy of a slice of Measures.
func DeepCopyMeasures(measures []Measure) []Measure {
	copied := make([]Measure, len(measures))
	for i, m := range measures {
		copied[i] = DeepCopyMeasure(m)
	}
	return copied
}
