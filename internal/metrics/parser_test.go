package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePrometheusText_Golden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "node_metrics.txt"))
	if err != nil {
		t.Skip("missing golden file")
	}
	samples := ParsePrometheusText(string(data))
	if len(samples) == 0 {
		t.Fatalf("expected some samples")
	}
}
