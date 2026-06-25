package system

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchMetadataWithRetryEventuallySucceeds(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := attempts.Add(1)
		if current < 3 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		if _, err := fmt.Fprint(w, `{"name":"vm-a","uuid":"uuid-1"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	data, err := fetchMetadataWithRetry(server.URL, 900*time.Millisecond)
	if err != nil {
		t.Fatalf("fetchMetadataWithRetry() error = %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempt count = %d, want 3", got)
	}
	if got := data["name"]; got != "vm-a" {
		t.Fatalf("name = %v, want vm-a", got)
	}
}

func TestFetchMetadataWithRetryReturnsLastError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := fetchMetadataWithRetry(server.URL, 900*time.Millisecond)
	if err == nil {
		t.Fatal("fetchMetadataWithRetry() error = nil, want error")
	}
	if got := attempts.Load(); got != openStackMetadataRetryAttempts {
		t.Fatalf("attempt count = %d, want %d", got, openStackMetadataRetryAttempts)
	}
}

func TestResolveVMNameUsesCachedValue(t *testing.T) {
	// Cache should be populated by LoadStartupMetadata() before ResolveVMName() is called.
	// This test verifies that cached value is returned.
	setCachedVMName("cached-vm-name")
	if got := ResolveVMName(); got != "cached-vm-name" {
		t.Fatalf("ResolveVMName() = %q, want %q", got, "cached-vm-name")
	}
}

func TestResolveVMNameFallsBackToHostname(t *testing.T) {
	// If cache is empty and no HYPERSTACK_VM_NAME env var, should return hostname.
	// This handles startup edge case where metadata fetch failed.
	t.Setenv("HYPERSTACK_VM_NAME", "")
	// Cache is empty by default in tests
	got := ResolveVMName()
	if got == "" {
		t.Fatalf("ResolveVMName() returned empty string")
	}
}

func TestMetadataSourcesUsesConfiguredURLOnly(t *testing.T) {
	t.Setenv("METADATA_URL", "http://metadata.example/openstack/latest/meta_data.json")

	got := metadataSources("http://metadata.example/openstack/latest/meta_data.json")
	if len(got) != 1 {
		t.Fatalf("len(metadataSources()) = %d, want 1", len(got))
	}
	if got[0] != "http://metadata.example/openstack/latest/meta_data.json" {
		t.Fatalf("metadataSources()[0] = %q", got[0])
	}
}

func TestConfiguredMetadataURLExpandsBracedHostnamePlaceholder(t *testing.T) {
	t.Setenv("HOSTNAME", "agent-e2e")
	t.Setenv("METADATA_URL", "http://metadata.example/openstack/latest/meta_data.json?hostname=${HOSTNAME}")

	got := configuredMetadataURL()
	want := "http://metadata.example/openstack/latest/meta_data.json?hostname=agent-e2e"
	if got != want {
		t.Fatalf("configuredMetadataURL() = %q, want %q", got, want)
	}
}

func TestMetadataSourcesUsesDefaultOpenStackWhenDefault(t *testing.T) {
	t.Setenv("METADATA_URL", "default")

	got := metadataSources("default")
	if len(got) != 1 {
		t.Fatalf("len(metadataSources()) = %d, want 1", len(got))
	}
	if got[0] != defaultOpenStackMetadataURL {
		t.Fatalf("metadataSources()[0] = %q, want %q", got[0], defaultOpenStackMetadataURL)
	}
}

func TestReadCloudInitMetadataExtractsMetaData(t *testing.T) {
	tmpDir := t.TempDir()
	cloudInitFile := filepath.Join(tmpDir, "instance-data.json")

	cloudInitJSON := `{
		"ds": {
			"meta_data": {
				"uuid": "test-uuid-123",
				"name": "test-vm",
				"meta": {
					"infrahub_key": "test-key-456",
					"cluster": "prod",
					"role": "agent"
				}
			}
		}
	}`
	if err := os.WriteFile(cloudInitFile, []byte(cloudInitJSON), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Temporarily patch the constant for testing
	originalPath := cloudInitDataPath
	cloudInitDataPath = cloudInitFile
	defer func() { cloudInitDataPath = originalPath }()

	data, err := readCloudInitMetadata()
	if err != nil {
		t.Fatalf("readCloudInitMetadata() error = %v", err)
	}

	if got := metadataString(data, "uuid"); got != "test-uuid-123" {
		t.Fatalf("uuid = %q, want test-uuid-123", got)
	}
	if got := metadataString(data, "name"); got != "test-vm" {
		t.Fatalf("name = %q, want test-vm", got)
	}
	if got := metadataMetaString(data, "infrahub_key"); got != "test-key-456" {
		t.Fatalf("infrahub_key = %q, want test-key-456", got)
	}
	if got := metadataMetaString(data, "cluster"); got != "prod" {
		t.Fatalf("cluster = %q, want prod", got)
	}
	if got := metadataMetaString(data, "role"); got != "agent" {
		t.Fatalf("role = %q, want agent", got)
	}
}

func TestReadCloudInitMetadataReturnErrorWhenFileNotFound(t *testing.T) {
	originalPath := cloudInitDataPath
	cloudInitDataPath = "/nonexistent/path/instance-data.json"
	defer func() { cloudInitDataPath = originalPath }()

	_, err := readCloudInitMetadata()
	if err == nil {
		t.Fatalf("readCloudInitMetadata() error = nil, want error")
	}
}

func TestReadCloudInitMetadataReturnErrorWhenMetaDataMissing(t *testing.T) {
	tmpDir := t.TempDir()
	cloudInitFile := filepath.Join(tmpDir, "instance-data.json")

	cloudInitJSON := `{
		"ds": {
			"other_data": {}
		}
	}`
	if err := os.WriteFile(cloudInitFile, []byte(cloudInitJSON), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	originalPath := cloudInitDataPath
	cloudInitDataPath = cloudInitFile
	defer func() { cloudInitDataPath = originalPath }()

	_, err := readCloudInitMetadata()
	if err == nil {
		t.Fatalf("readCloudInitMetadata() error = nil, want error")
	}
}

func TestFetchInstanceMetadataTriesCloudInitFileFirst(t *testing.T) {
	tmpDir := t.TempDir()
	cloudInitFile := filepath.Join(tmpDir, "instance-data.json")

	cloudInitJSON := `{
		"ds": {
			"meta_data": {
				"uuid": "file-uuid",
				"name": "file-vm"
			}
		}
	}`
	if err := os.WriteFile(cloudInitFile, []byte(cloudInitJSON), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	originalPath := cloudInitDataPath
	cloudInitDataPath = cloudInitFile
	defer func() { cloudInitDataPath = originalPath }()

	data, err := fetchInstanceMetadata(3*time.Second, false)
	if err != nil {
		t.Fatalf("fetchInstanceMetadata() error = %v", err)
	}

	if got := metadataString(data, "uuid"); got != "file-uuid" {
		t.Fatalf("uuid = %q, want file-uuid", got)
	}
}

func TestFetchInstanceMetadataSkipsCloudInitFileWhenSkipFileTrue(t *testing.T) {
	tmpDir := t.TempDir()
	cloudInitFile := filepath.Join(tmpDir, "instance-data.json")

	cloudInitJSON := `{
		"ds": {
			"meta_data": {
				"uuid": "file-uuid"
			}
		}
	}`
	if err := os.WriteFile(cloudInitFile, []byte(cloudInitJSON), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	originalPath := cloudInitDataPath
	cloudInitDataPath = cloudInitFile
	defer func() { cloudInitDataPath = originalPath }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, `{"uuid":"http-uuid","name":"http-vm"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	// When skipFile=true, should skip the file and fetch from HTTP.
	t.Setenv("METADATA_URL", server.URL)

	data, err := fetchInstanceMetadata(3*time.Second, true)
	if err != nil {
		t.Fatalf("fetchInstanceMetadata() error = %v", err)
	}

	if got := metadataString(data, "uuid"); got != "http-uuid" {
		t.Fatalf("uuid = %q, want http-uuid (from HTTP)", got)
	}
}

func TestFetchMetadataOnceDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("metadata client followed redirect to target")
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := fetchMetadataOnce(redirector.URL, time.Second)
	if err == nil {
		t.Fatal("fetchMetadataOnce() error = nil, want redirect status error")
	}
}
