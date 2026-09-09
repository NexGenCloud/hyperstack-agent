package config

import (
	"testing"
)

func TestLoad_APIKeyEmpty(t *testing.T) {
	// Load() no longer fetches API key; it's loaded at startup from metadata.
	// Verify that Load() returns empty API key in config.
	cfg := Load()
	if cfg.Hub.APIKey != "" {
		t.Fatalf("cfg.Hub.APIKey = %q, want empty (API key now from startup metadata)", cfg.Hub.APIKey)
	}
}

func TestLoad_HubURL(t *testing.T) {
	// Verify Load() reads hub URL from env with sensible default.
	t.Setenv("HYPERSTACK_URL", "http://example.com:8000")
	cfg := Load()
	if cfg.Hub.URL != "http://example.com:8000" {
		t.Fatalf("cfg.Hub.URL = %q, want %q", cfg.Hub.URL, "http://example.com:8000")
	}
}

func TestLoad_DefaultInterval(t *testing.T) {
	// Verify Load() reads default interval from env.
	t.Setenv("HYPERSTACK_INTERVAL", "30s")
	cfg := Load()
	if cfg.DefaultInterval.String() != "30s" {
		t.Fatalf("cfg.DefaultInterval = %v, want 30s", cfg.DefaultInterval)
	}
}

func TestLoad_DedicatedInferenceDefaults(t *testing.T) {
	cfg := Load()
	if cfg.DedicatedInference.Endpoint != "http://127.0.0.1:8000/metrics" {
		t.Fatalf("cfg.DedicatedInference.Endpoint = %q, want default 127.0.0.1:8000/metrics", cfg.DedicatedInference.Endpoint)
	}
	if cfg.DedicatedInference.ScrapeInterval.String() != "15s" {
		t.Fatalf("cfg.DedicatedInference.ScrapeInterval = %v, want 15s", cfg.DedicatedInference.ScrapeInterval)
	}
}

func TestLoad_DedicatedInferenceEndpointOverride(t *testing.T) {
	t.Setenv("HYPERSTACK_DEDICATED_INFERENCE_URL", "http://localhost:9000/metrics")
	cfg := Load()
	if cfg.DedicatedInference.Endpoint != "http://localhost:9000/metrics" {
		t.Fatalf("cfg.DedicatedInference.Endpoint = %q, want override", cfg.DedicatedInference.Endpoint)
	}
}

func TestSecurityWarningsWarnsForNonLoopbackHTTPHub(t *testing.T) {
	cfg := Config{Hub: HubConfig{URL: "http://gateway.example.com"}}

	warnings := SecurityWarnings(cfg)

	if !hasWarning(warnings, "insecure_hub_url") {
		t.Fatalf("SecurityWarnings() = %#v, want insecure_hub_url", warnings)
	}
}

func TestSecurityWarningsAllowsLoopbackHTTPHub(t *testing.T) {
	cfg := Config{Hub: HubConfig{URL: "http://127.0.0.1:8000"}}

	warnings := SecurityWarnings(cfg)

	if hasWarning(warnings, "insecure_hub_url") {
		t.Fatalf("SecurityWarnings() = %#v, did not want insecure_hub_url", warnings)
	}
}

func TestSecurityWarningsWarnsForCustomMetadataURLAndProxy(t *testing.T) {
	t.Setenv("METADATA_URL", "http://metadata.example/openstack/latest/meta_data.json")
	t.Setenv("HTTP_PROXY", "http://proxy.example:8080")

	cfg := Config{Hub: HubConfig{URL: "https://gateway.example.com"}}

	warnings := SecurityWarnings(cfg)

	if !hasWarning(warnings, "custom_metadata_url") {
		t.Fatalf("SecurityWarnings() = %#v, want custom_metadata_url", warnings)
	}
	if !hasWarning(warnings, "proxy_env") {
		t.Fatalf("SecurityWarnings() = %#v, want proxy_env", warnings)
	}
}

func hasWarning(warnings []SecurityWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}
