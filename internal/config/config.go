package config

import (
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// HubConfig groups hub connection settings.
type HubConfig struct {
	URL    string
	APIKey string
}

const (
	// Hardcoded gateway endpoint paths
	AgentPushPath = "api/v1/agent/metrics"
)

// ExporterConfig groups collector-level settings (metadata can override enable/interval).
type ExporterConfig struct {
	Enable         bool
	ScrapeInterval time.Duration
}

// Config holds environment-driven settings for the agent.
type Config struct {
	Hub             HubConfig
	DefaultInterval time.Duration

	Node ExporterConfig
	GPU  ExporterConfig
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	return Config{
		Hub: HubConfig{
			URL:    getEnv("HYPERSTACK_URL", "http://localhost:8000"),
			APIKey: "",
		},
		DefaultInterval: getEnvDuration("HYPERSTACK_INTERVAL", 15*time.Second),
		// Exporter configs default enabled.
		Node: ExporterConfig{
			Enable:         getEnvBool("HYPERSTACK_ENABLE_NODE", true),
			ScrapeInterval: 15 * time.Second,
		},
		GPU: ExporterConfig{
			Enable:         getEnvBool("HYPERSTACK_ENABLE_GPU", true),
			ScrapeInterval: 30 * time.Second,
		},
	}
}

// SecurityWarning describes a potentially unsafe-but-still-supported runtime
// configuration. These are warnings only to preserve deployment compatibility.
type SecurityWarning struct {
	Code    string
	Message string
}

// SecurityWarnings returns compatibility-preserving hardening warnings for
// configs that can expose credentials or metadata if misconfigured.
func SecurityWarnings(cfg Config) []SecurityWarning {
	var warnings []SecurityWarning

	if isNonLoopbackHTTP(cfg.Hub.URL) {
		warnings = append(warnings, SecurityWarning{
			Code:    "insecure_hub_url",
			Message: "HYPERSTACK_URL uses plain HTTP for a non-loopback host; use HTTPS in production",
		})
	}

	if metadataURL := strings.TrimSpace(os.Getenv("METADATA_URL")); metadataURL != "" && !strings.EqualFold(metadataURL, "default") {
		warnings = append(warnings, SecurityWarning{
			Code:    "custom_metadata_url",
			Message: "METADATA_URL override is enabled; only use trusted metadata endpoints",
		})
	}

	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			warnings = append(warnings, SecurityWarning{
				Code:    "proxy_env",
				Message: key + " is set; ensure proxies are trusted before sending hub traffic",
			})
		}
	}

	return warnings
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if v == "0" || v == "false" || v == "False" {
			return false
		}
		return true
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func isNonLoopbackHTTP(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return true
}
