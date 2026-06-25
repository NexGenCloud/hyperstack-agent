package system

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/host"
)

const metadataBaseURL = "api/v1/metadata"
const (
	defaultOpenStackMetadataURL     = "http://169.254.169.254/openstack/latest/meta_data.json"
	openStackMetadataRetryAttempts  = 3
	openStackMetadataInitialBackoff = 200 * time.Millisecond
	defaultCloudInitPath            = "/run/cloud-init/instance-data.json"
)

var cloudInitDataPath = defaultCloudInitPath

var (
	cachedInstanceUUIDMu sync.RWMutex
	cachedInstanceUUID   string
	cachedVMNameMu       sync.RWMutex
	cachedVMName         string
)

// GetCachedInstanceUUID returns the most recently resolved OpenStack instance
// UUID, or an empty string if no UUID has been fetched yet.
func GetCachedInstanceUUID() string {
	cachedInstanceUUIDMu.RLock()
	defer cachedInstanceUUIDMu.RUnlock()
	return cachedInstanceUUID
}

func setCachedInstanceUUID(uuid string) {
	cachedInstanceUUIDMu.Lock()
	defer cachedInstanceUUIDMu.Unlock()
	cachedInstanceUUID = uuid
}

func GetCachedVMName() string {
	cachedVMNameMu.RLock()
	defer cachedVMNameMu.RUnlock()
	return cachedVMName
}

func setCachedVMName(name string) {
	cachedVMNameMu.Lock()
	defer cachedVMNameMu.Unlock()
	cachedVMName = name
}

func resolveVMName() string {
	if cached := GetCachedVMName(); cached != "" {
		return cached
	}
	return fallbackVMName()
}

// ResolveVMName returns the VM name via metadata first, then fallback.
func ResolveVMName() string {
	return resolveVMName()
}

// FetchInstanceMetadata fetches instance metadata, preferring the cloud-init file
// if available, then falling back to METADATA_URL or the standard OpenStack endpoint.
// The configured endpoint is expected to serve OpenStack-shaped metadata.
func FetchInstanceMetadata() (map[string]any, error) {
	return fetchInstanceMetadata(3*time.Second, false)
}

// fetchInstanceMetadataHTTP fetches metadata from HTTP only, skipping the cloud-init file.
// Used for key refresh (401 paths) where the file would be stale.
func fetchInstanceMetadataHTTP(timeout time.Duration) (map[string]any, error) {
	return fetchInstanceMetadata(timeout, true)
}

func fetchInstanceMetadata(timeout time.Duration, skipFile bool) (map[string]any, error) {
	if !skipFile {
		if data, err := readCloudInitMetadata(); err == nil {
			return data, nil
		}
	}
	envURL := configuredMetadataURL()
	for _, target := range metadataSources(envURL) {
		data, err := fetchMetadata(target, timeout)
		if err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("metadata unavailable")
}

func readCloudInitMetadata() (map[string]any, error) {
	data, err := os.ReadFile(cloudInitDataPath)
	if err != nil {
		return nil, err
	}
	var cloudInit map[string]any
	if err := json.Unmarshal(data, &cloudInit); err != nil {
		return nil, err
	}
	dsData := metadataMap(cloudInit, "ds")
	metaData := metadataMap(dsData, "meta_data")
	if metaData == nil {
		return nil, fmt.Errorf("no ds.meta_data in cloud-init file")
	}
	slog.Debug("loaded metadata from cloud-init file", "path", cloudInitDataPath)
	return metaData, nil
}

func fetchMetadata(target string, timeout time.Duration) (map[string]any, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("metadata url empty")
	}
	if target == defaultOpenStackMetadataURL {
		return fetchMetadataWithRetry(target, timeout)
	}
	return fetchMetadataOnce(target, timeout)
}

func fetchMetadataWithRetry(target string, timeout time.Duration) (map[string]any, error) {
	perAttemptTimeout := timeout / openStackMetadataRetryAttempts
	if perAttemptTimeout <= 0 {
		perAttemptTimeout = timeout
	}

	backoff := openStackMetadataInitialBackoff
	var lastErr error
	for attempt := 1; attempt <= openStackMetadataRetryAttempts; attempt++ {
		data, err := fetchMetadataOnce(target, perAttemptTimeout)
		if err == nil {
			slog.Info("openstack metadata fetch succeeded", "url", target, "attempt", attempt, "max_attempts", openStackMetadataRetryAttempts)
			return data, nil
		}
		lastErr = err
		if attempt == openStackMetadataRetryAttempts {
			break
		}
		slog.Debug("unable to reach openstack metadata; retrying",
			"url", target,
			"attempt", attempt,
			"max_attempts", openStackMetadataRetryAttempts,
			"backoff", backoff,
			"error", err,
		)
		time.Sleep(backoff)
		backoff *= 2
	}
	slog.Info("openstack metadata fetch failed", "url", target, "attempts", openStackMetadataRetryAttempts, "error", lastErr)
	return nil, fmt.Errorf("openstack metadata fetch failed after %d attempts: %w", openStackMetadataRetryAttempts, lastErr)
}

func fetchMetadataOnce(target string, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := metadataHTTPClient(timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("metadata response body close failed", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata status %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

func metadataHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func configuredMetadataURL() string {
	raw := strings.TrimSpace(os.Getenv("METADATA_URL"))
	if raw == "" || strings.EqualFold(raw, "default") {
		return raw
	}

	hostname := metadataHostname()
	if hostname == "" {
		return raw
	}

	return strings.NewReplacer("${HOSTNAME}", hostname, "$HOSTNAME", hostname).Replace(raw)
}

func metadataSources(envURL string) []string {
	if target := strings.TrimSpace(envURL); target != "" && !strings.EqualFold(target, "default") {
		return []string{target}
	}
	return []string{defaultOpenStackMetadataURL}
}

func metadataHostname() string {
	if env := strings.TrimSpace(os.Getenv("HOSTNAME")); env != "" {
		return env
	}
	host, _ := os.Hostname()
	return strings.TrimSpace(host)
}

func fallbackVMName() string {
	if env := strings.TrimSpace(os.Getenv("HYPERSTACK_VM_NAME")); env != "" {
		return env
	}
	host, _ := os.Hostname()
	return host
}

func ResolveHubPath(template, vmName string) string {
	template = strings.TrimSpace(template)
	vmName = strings.TrimSpace(vmName)
	if vmName == "" {
		if template == "" {
			return "api/v1/metrics"
		}
		return template
	}
	encoded := url.PathEscape(vmName)
	if template == "" {
		return fmt.Sprintf("api/v1/%s/metrics", encoded)
	}
	if strings.Contains(template, "{{vm}}") {
		return strings.ReplaceAll(template, "{{vm}}", encoded)
	}
	if strings.Contains(template, "{vm}") {
		// Maintain existing braces while substituting path-safe value.
		return strings.ReplaceAll(template, "{vm}", encoded)
	}
	if strings.Contains(template, "%s") {
		return fmt.Sprintf(template, encoded)
	}
	if strings.Contains(template, "%VM%") {
		return strings.ReplaceAll(template, "%VM%", encoded)
	}
	// Allow templates like /api/v1/metrics/vm-name
	if !strings.Contains(template, "?") {
		trim := strings.Trim(template, "/")
		parts := strings.Split(trim, "/")
		if len(parts) >= 2 && parts[len(parts)-1] == "metrics" {
			idx := len(parts) - 2
			if idx >= 0 {
				parts[idx] = encoded
				return strings.Join(parts, "/")
			}
		}
	}
	return template
}

func metadataMap(data map[string]any, key string) map[string]any {
	if data == nil {
		return nil
	}
	value, ok := data[key].(map[string]any)
	if !ok {
		return nil
	}
	return value
}

func metadataString(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	value, ok := data[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func metadataMetaString(data map[string]any, key string) string {
	return metadataString(metadataMap(data, "meta"), key)
}

// StartupMetadata holds essential identity and auth data loaded once at startup.
type StartupMetadata struct {
	UUID        string
	InfrahubKey string
	VMName      string
	Cluster     string
	Role        string
}

// FetchInfrahubKey re-fetches the metadata from HTTP (skipping the cloud-init file)
// and returns the `meta.infrahub_key` value. Used to recover from gateway 401s when
// the originally-cached key has been rotated. The returned key is the raw value
// (no "VM " prefix).
func FetchInfrahubKey(ctx context.Context) (string, error) {
	// Honor caller cancellation by running the (synchronous) fetch in a
	// goroutine and selecting on ctx.Done(). The underlying fetch already
	// imposes its own per-request timeout.
	type result struct {
		data map[string]any
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := fetchInstanceMetadataHTTP(3 * time.Second)
		ch <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		key := metadataMetaString(r.data, "infrahub_key")
		if key == "" {
			return "", fmt.Errorf("metadata returned empty infrahub_key")
		}
		return key, nil
	}
}

// LoadStartupMetadata fetches instance metadata once and extracts identity/auth fields.
// Caches UUID and VMName for later use. Returns the full metadata bundle.
func LoadStartupMetadata() (StartupMetadata, error) {
	metadata, err := FetchInstanceMetadata()
	if err != nil {
		return StartupMetadata{}, err
	}

	meta := StartupMetadata{
		UUID:        metadataString(metadata, "uuid"),
		InfrahubKey: metadataMetaString(metadata, "infrahub_key"),
		VMName:      metadataString(metadata, "name"),
		Cluster:     metadataMetaString(metadata, "cluster"),
		Role:        metadataMetaString(metadata, "role"),
	}

	if meta.VMName == "" {
		meta.VMName = fallbackVMName()
	}
	if meta.UUID != "" {
		setCachedInstanceUUID(meta.UUID)
	}
	setCachedVMName(meta.VMName)

	slog.Info("startup metadata loaded", "uuid", meta.UUID, "vm_name", meta.VMName, "cluster", meta.Cluster, "role", meta.Role)
	return meta, nil
}

// SubmitStartupMetadata submits agent metadata using pre-fetched startup data.
// No internal metadata fetches; uses data from LoadStartupMetadata().
func SubmitStartupMetadata(ctx context.Context, hubClient interface {
	Submit(context.Context, string, any) error
}, meta StartupMetadata) {
	if meta.UUID == "" {
		slog.Warn("startup metadata submission skipped", "reason", "uuid not set")
		return
	}

	vmName := meta.VMName
	if vmName == "" {
		vmName = fallbackVMName()
	}

	payload := map[string]any{
		"uuid": meta.UUID,
		"name": vmName,
	}

	if meta.Cluster != "" || meta.Role != "" {
		payload["cluster"] = map[string]string{
			"name": meta.Cluster,
			"role": meta.Role,
		}
	}

	hostInfo, err := host.InfoWithContext(ctx)
	hostname := ""
	osName := ""
	if err == nil {
		hostname = hostInfo.Hostname
		osName = hostInfo.OS
	}

	metadataPath := fmt.Sprintf("%s/%s", metadataBaseURL, url.PathEscape(meta.UUID))
	if err := hubClient.Submit(ctx, metadataPath, payload); err != nil {
		slog.Warn("startup metadata submission failed", "error", err)
		return
	}

	slog.Info("startup metadata submitted", "uuid", meta.UUID, "vm_name", vmName, "hostname", hostname, "os", osName)
}
