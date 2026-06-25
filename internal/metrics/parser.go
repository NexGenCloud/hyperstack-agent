package metrics

import (
	"bufio"
	"strings"
)

// Sample represents a parsed Prometheus metric sample.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  string
}

// ParsePrometheusText parses a subset of Prometheus text exposition format into samples.
// This is a minimal parser; enhancement for full parity will follow.
func ParsePrometheusText(input string) []Sample {
	var out []Sample
	scanner := bufio.NewScanner(strings.NewReader(input))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value := splitMetricLine(line)
		if name == "" || value == "" {
			continue
		}
		out = append(out, Sample{Name: name, Labels: labels, Value: value})
	}
	return out
}

func splitMetricLine(line string) (string, map[string]string, string) {
	// metric_name{label="value"} 123
	var name, value string
	labels := map[string]string{}

	// value is after last space
	idx := strings.LastIndexByte(line, ' ')
	if idx <= 0 || idx == len(line)-1 {
		return "", nil, ""
	}
	value = strings.TrimSpace(line[idx+1:])
	metricPart := strings.TrimSpace(line[:idx])

	// labels block optional
	if lb := strings.IndexByte(metricPart, '{'); lb != -1 {
		rb := strings.LastIndexByte(metricPart, '}')
		if rb > lb {
			name = metricPart[:lb]
			labelsStr := metricPart[lb+1 : rb]
			for _, kv := range strings.Split(labelsStr, ",") {
				kv = strings.TrimSpace(kv)
				if kv == "" {
					continue
				}
				parts := strings.SplitN(kv, "=", 2)
				if len(parts) != 2 {
					continue
				}
				k := strings.TrimSpace(parts[0])
				v := strings.Trim(parts[1], "\"")
				labels[k] = v
			}
		} else {
			name = metricPart
		}
	} else {
		name = metricPart
	}
	name = strings.TrimSpace(name)
	return name, labels, value
}
