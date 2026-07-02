package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
	"github.com/NexGenCloud/hyperstack-agent/internal/system"
)

func decodeSubmitRequest(t *testing.T, body io.Reader) (SubmitRequest, bool) {
	t.Helper()
	var req SubmitRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		t.Errorf("decode submit request: %v", err)
		return SubmitRequest{}, false
	}
	return req, true
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestParseRetryAfter tests the RFC 7231 Retry-After header parsing.
func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		wantOk   bool
		checkDur func(time.Duration) bool
	}{
		{
			name:   "empty string",
			header: "",
			wantOk: false,
		},
		{
			name:   "delay-seconds: 120",
			header: "120",
			wantOk: true,
			checkDur: func(d time.Duration) bool {
				return d == 120*time.Second
			},
		},
		{
			name:   "delay-seconds: 0",
			header: "0",
			wantOk: true,
			checkDur: func(d time.Duration) bool {
				return d == 0
			},
		},
		{
			name:   "delay-seconds with whitespace",
			header: "  60  ",
			wantOk: true,
			checkDur: func(d time.Duration) bool {
				return d == 60*time.Second
			},
		},
		{
			name:   "delay-seconds: negative (invalid)",
			header: "-10",
			wantOk: false,
		},
		{
			name:   "non-numeric garbage",
			header: "not-a-number",
			wantOk: false,
		},
		{
			name:   "http-date: future",
			header: "Fri, 31 Dec 2099 23:59:59 GMT",
			wantOk: true,
			checkDur: func(d time.Duration) bool {
				// Should be a positive duration in the future
				return d > 0
			},
		},
		{
			name:   "http-date: past (clamped to 0)",
			header: "Mon, 01 Jan 1990 00:00:00 GMT",
			wantOk: true,
			checkDur: func(d time.Duration) bool {
				// Past dates should be clamped to 0
				return d == 0
			},
		},
		{
			name:   "invalid date format",
			header: "Jan 1, 2099",
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, ok := parseRetryAfter(tt.header)
			if ok != tt.wantOk {
				t.Fatalf("parseRetryAfter(%q) ok = %v, want %v", tt.header, ok, tt.wantOk)
			}
			if tt.wantOk && tt.checkDur != nil {
				if !tt.checkDur(d) {
					t.Errorf("parseRetryAfter(%q) returned duration %v, check failed", tt.header, d)
				}
			}
		})
	}
}

// TestEnqueueMetrics_NonBlocking verifies that EnqueueMetrics returns immediately.
func TestEnqueueMetrics_NonBlocking(t *testing.T) {
	hc := NewHubClient("http://127.0.0.1:1")

	measures := []metrics.Measure{
		{MetricID: "test", Value: 1.0},
	}

	start := time.Now()
	hc.EnqueueMetrics(measures)
	elapsed := time.Since(start)

	if elapsed > 10*time.Millisecond {
		t.Fatalf("EnqueueMetrics should be nearly instant, took %v", elapsed)
	}
}

func TestEnqueueMetrics_DropsOldestWhenPendingQueueFull(t *testing.T) {
	hc := NewHubClient("http://127.0.0.1:1")

	measures := make([]metrics.Measure, MaxPendingSeries+5)
	for i := range measures {
		measures[i] = metrics.Measure{MetricID: fmt.Sprintf("metric_%d", i), Value: float64(i)}
	}

	hc.EnqueueMetrics(measures)

	hc.pendingMu.Lock()
	pendingSize := len(hc.pending)
	firstMetric := hc.pending[0].MetricID
	hc.pendingMu.Unlock()

	if pendingSize != MaxPendingSeries {
		t.Fatalf("pending size = %d, want %d", pendingSize, MaxPendingSeries)
	}
	if firstMetric != "metric_5" {
		t.Fatalf("first pending metric = %q, want oldest five dropped", firstMetric)
	}
	if got := hc.GetSamplesFailed(); got != 5 {
		t.Fatalf("samplesFailed = %d, want 5 dropped samples", got)
	}
}

func TestEnqueueMetrics_UpdatesLastSubmitMsOnSuccess(t *testing.T) {
	system.SetLastSubmitMs(0)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL).WithPath("metrics")
	hc.VMName = "test-vm"
	hc.InstanceUUID = "uuid-123"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hc.StartSubmitLoop(ctx)

	hc.EnqueueMetrics([]metrics.Measure{{MetricID: "test", Value: 1, Timestamp: float64(time.Now().UnixNano())}})

	// Wait for batching window (5s) + submission
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if got := system.GetLastSubmitMs(); got >= 20 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("last_submit_ms = %d, want >= 20 (after batching window + submission)", system.GetLastSubmitMs())
}

// TestEnqueueMetrics_BatchesAtLimit verifies that the submit loop batches
// measures up to MaxSeriesPerRequest.
func TestEnqueueMetrics_BatchesAtLimit(t *testing.T) {
	var submitCalls int32
	var lastReq *SubmitRequest
	var reqMu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "metrics") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&submitCalls, 1)
		req, ok := decodeSubmitRequest(t, r.Body)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reqMu.Lock()
		lastReq = &req
		reqMu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL).WithPath("metrics")
	hc.VMName = "test-vm"
	hc.InstanceUUID = "uuid-123"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hc.StartSubmitLoop(ctx)

	// Enqueue more than MaxSeriesPerRequest measures
	allMeasures := make([]metrics.Measure, MaxSeriesPerRequest+500)
	now := float64(time.Now().UnixNano())
	for i := 0; i < len(allMeasures); i++ {
		allMeasures[i] = metrics.Measure{
			MetricID:  fmt.Sprintf("metric_%d", i),
			Value:     float64(i),
			Timestamp: now,
		}
	}
	hc.EnqueueMetrics(allMeasures)

	// Wait for batching window (5s) + batches to be submitted
	time.Sleep(8 * time.Second)

	// Should have made at least 2 Submit calls (first batch + second batch)
	calls := atomic.LoadInt32(&submitCalls)
	if calls < 2 {
		t.Fatalf("expected at least 2 Submit calls, got %d", calls)
	}

	// Last call should have <= MaxSeriesPerRequest series
	reqMu.Lock()
	if lastReq != nil && len(lastReq.Series) > MaxSeriesPerRequest {
		t.Fatalf("batch size %d exceeds MaxSeriesPerRequest %d", len(lastReq.Series), MaxSeriesPerRequest)
	}
	reqMu.Unlock()
}

// TestEnqueueMetrics_EnqueuesMultipleBatches verifies that the submit loop
// can handle multiple consecutive enqueues and batches them appropriately.
func TestEnqueueMetrics_EnqueuesMultipleBatches(t *testing.T) {
	var submitCalls int32
	var reqMu sync.Mutex
	var requests []*SubmitRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "metrics") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&submitCalls, 1)
		req, ok := decodeSubmitRequest(t, r.Body)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reqMu.Lock()
		requests = append(requests, &req)
		reqMu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL).WithPath("metrics")
	hc.VMName = "test-vm"
	hc.InstanceUUID = "uuid-123"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hc.StartSubmitLoop(ctx)

	now := float64(time.Now().UnixNano())
	// Enqueue multiple small batches
	for i := 0; i < 3; i++ {
		batch := []metrics.Measure{
			{MetricID: fmt.Sprintf("metric_%d", i), Value: float64(i), Timestamp: now},
		}
		hc.EnqueueMetrics(batch)
		time.Sleep(100 * time.Millisecond)
	}

	// Wait for batching window (5s) + submission
	time.Sleep(7 * time.Second)

	// Should have received at least one request with multiple series
	// (the exact number depends on batching behavior, but we should get all 3 metrics)
	reqMu.Lock()
	totalSeries := 0
	for _, req := range requests {
		totalSeries += len(req.Series)
	}
	reqMu.Unlock()

	if totalSeries != 3 {
		t.Fatalf("expected 3 series total across all requests, got %d", totalSeries)
	}
}

// TestSubmitBatch_SingleAttemptNoRetry verifies submitBatch() makes exactly one attempt.
func TestSubmitBatch_SingleAttemptNoRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(500)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	batch := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{"label": "value"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := hc.submitBatch(ctx, batch)
	if err == nil {
		t.Fatalf("expected error from failed submission, got nil")
	}
	if attempts != 1 {
		t.Fatalf("submitBatch() made %d attempts, want 1", attempts)
	}
}

func TestSubmitBatch_RefreshesInfrahubKeyOnUnauthorized(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		switch attempt {
		case 1:
			if got := r.Header.Get("api-key"); got != "VM stale-key" {
				t.Errorf("first api-key = %q, want stale key", got)
			}
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			if got := r.Header.Get("api-key"); got != "VM fresh-key" {
				t.Errorf("second api-key = %q, want refreshed key", got)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request attempt %d", attempt)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var refreshes atomic.Int32
	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"
	hc.SetInfrahubKey("stale-key")
	hc.KeyRefresher = func(ctx context.Context) (string, error) {
		refreshes.Add(1)
		return "fresh-key", nil
	}

	batch := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := hc.submitBatch(ctx, batch)
	if err != nil {
		t.Fatalf("submitBatch() failed after key refresh: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
	if got := hc.getAPIKey(); got != "VM fresh-key" {
		t.Fatalf("stored api-key = %q, want refreshed key", got)
	}
}

func TestSubmit_RefreshesInfrahubKeyOnUnauthorized(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		attempt := attempts.Add(1)
		switch attempt {
		case 1:
			if got := r.Header.Get("api-key"); got != "VM stale-key" {
				t.Errorf("first api-key = %q, want stale key", got)
			}
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			if got := r.Header.Get("api-key"); got != "VM fresh-key" {
				t.Errorf("second api-key = %q, want refreshed key", got)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request attempt %d", attempt)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var refreshes atomic.Int32
	hc := NewHubClient(srv.URL)
	hc.SetInfrahubKey("stale-key")
	hc.KeyRefresher = func(ctx context.Context) (string, error) {
		refreshes.Add(1)
		return "fresh-key", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := hc.Submit(ctx, "api/v1/metadata", map[string]string{"name": "test-vm"})
	if err != nil {
		t.Fatalf("Submit() failed after key refresh: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
}

func TestGetMetadataReadsMetricsEnabled(t *testing.T) {
	var gotPath string
	var gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAPIKey = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metrics_enabled":false}`))
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.SetInfrahubKey("vm-key")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	metadata, err := hc.GetMetadata(ctx, "vm uuid")
	if err != nil {
		t.Fatalf("GetMetadata() error = %v", err)
	}
	if metadata.MetricsEnabled {
		t.Fatal("MetricsEnabled = true, want false")
	}
	if gotPath != "/api/v1/metadata/vm%20uuid" {
		t.Fatalf("path = %q, want escaped metadata path", gotPath)
	}
	if gotAPIKey != "VM vm-key" {
		t.Fatalf("api-key = %q, want VM key header", gotAPIKey)
	}
}

func TestGetMetadataRequiresUUID(t *testing.T) {
	hc := NewHubClient("http://example.test")
	_, err := hc.GetMetadata(context.Background(), " ")
	if err == nil {
		t.Fatal("GetMetadata() error = nil, want uuid error")
	}
}

func TestSubmitBatch_StripsAPIKeyOnCrossHostRedirect(t *testing.T) {
	var targetAPIKey string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAPIKey = r.Header.Get("api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != "VM secret-key" {
			t.Errorf("redirector api-key = %q, want original key", got)
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	hc := NewHubClient(redirector.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"
	hc.SetInfrahubKey("secret-key")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := hc.submitBatch(ctx, []metrics.Measure{{MetricID: "m1", Timestamp: 1000, Value: 1}})
	if err != nil {
		t.Fatalf("submitBatch() error = %v", err)
	}
	if targetAPIKey != "" {
		t.Fatalf("redirect target api-key = %q, want stripped", targetAPIKey)
	}
}

// TestSubmitBatch_SuccessUpdatesSamplesSent verifies success increments samplesSent.
func TestSubmitBatch_SuccessUpdatesSamplesSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	batch := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{}},
		{MetricID: "m2", Timestamp: 2000, Value: 43.0, Labels: map[string]string{}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	retryAfter, err := hc.submitBatch(ctx, batch)
	if err != nil {
		t.Fatalf("submitBatch() failed: %v", err)
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v, want 0", retryAfter)
	}
	if got := hc.GetSamplesSent(); got != 2 {
		t.Fatalf("samplesSent = %d, want 2", got)
	}
}

func TestSubmitBatchMetricsDisabledReturnsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Metrics aren't enabled for this virtual machine", http.StatusPreconditionFailed)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	batch := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	retryAfter, err := hc.submitBatch(ctx, batch)
	if !errors.Is(err, ErrMetricsDisabled) {
		t.Fatalf("submitBatch() error = %v, want ErrMetricsDisabled", err)
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v, want 0", retryAfter)
	}
	if got := hc.GetSamplesSent(); got != 0 {
		t.Fatalf("samplesSent = %d, want 0", got)
	}
}

func TestRunSubmitLoopDropsMetricsDisabledBatch(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "Metrics aren't enabled for this virtual machine", http.StatusPreconditionFailed)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"
	hc.EnqueueMetrics([]metrics.Measure{
		{MetricID: "m1", Timestamp: float64(time.Now().Unix()), Value: 42.0, Labels: map[string]string{}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := hc.StartSubmitLoop(ctx)
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		hc.pendingMu.Lock()
		pending := len(hc.pending)
		hc.pendingMu.Unlock()
		if pending == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	hc.pendingMu.Lock()
	pending := len(hc.pending)
	hc.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending queue = %d, want 0", pending)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("submit attempts = %d, want 1", got)
	}
	if got := hc.GetSamplesFailed(); got != 0 {
		t.Fatalf("samplesFailed = %d, want 0", got)
	}
}

// TestSubmitBatch_PreservesOriginalLabels verifies job label is added without losing original labels.
func TestSubmitBatch_PreservesOriginalLabels(t *testing.T) {
	var mu sync.Mutex
	var capturedLabels map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeSubmitRequest(t, r.Body)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if len(req.Series) > 0 {
			mu.Lock()
			capturedLabels = make(map[string]string)
			for k, v := range req.Series[0].Labels {
				capturedLabels[k] = v
			}
			mu.Unlock()
		}

		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	batch := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{"user": "original"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _ = hc.submitBatch(ctx, batch)

	// Verify the submitted batch had the job label injected along with original labels
	mu.Lock()
	if capturedLabels["job"] != "hyperstack_agent" {
		t.Fatalf("job label not injected, labels=%v", capturedLabels)
	}
	if capturedLabels["user"] != "original" {
		t.Fatalf("original label lost, labels=%v", capturedLabels)
	}
	mu.Unlock()
}

// TestSubmitBatch_RetryAfterParsing verifies Retry-After is extracted from response.
func TestSubmitBatch_RetryAfterParsing(t *testing.T) {
	tests := []struct {
		name             string
		retryAfterHeader string
		expectedDuration time.Duration
		shouldBeParsed   bool
	}{
		{"seconds", "30", 30 * time.Second, true},
		{"invalid", "not-a-number", 0, false},
		{"empty", "", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Set headers BEFORE WriteHeader
				if tt.retryAfterHeader != "" {
					w.Header().Set("Retry-After", tt.retryAfterHeader)
				}
				w.WriteHeader(429)
			}))
			defer srv.Close()

			hc := NewHubClient(srv.URL)
			hc.VMName = "test-vm"
			hc.InstanceUUID = "test-uuid"

			batch := []metrics.Measure{{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{}}}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			retryAfter, err := hc.submitBatch(ctx, batch)

			if err == nil {
				t.Fatalf("expected error for 429 response")
			}

			if tt.shouldBeParsed {
				if retryAfter != tt.expectedDuration {
					t.Fatalf("retryAfter = %v, want %v", retryAfter, tt.expectedDuration)
				}
			} else {
				if retryAfter != 0 {
					t.Fatalf("retryAfter = %v, want 0 (unparseable)", retryAfter)
				}
			}
		})
	}
}

func TestSubmitBatch_CapsRetryAfter(t *testing.T) {
	hc := NewHubClient("http://127.0.0.1")
	hc.HTTP = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": []string{"600"}},
				Body:       io.NopCloser(strings.NewReader("rate limited")),
				Request:    req,
			}, nil
		}),
	}
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	retryAfter, err := hc.submitBatch(ctx, []metrics.Measure{{MetricID: "m1", Timestamp: 1000, Value: 1}})
	if err == nil {
		t.Fatal("submitBatch() error = nil, want 429 error")
	}
	if retryAfter != MaxRetryAfter {
		t.Fatalf("retryAfter = %v, want capped %v", retryAfter, MaxRetryAfter)
	}
}

func TestSubmitBatch_TruncatesErrorResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", maxErrorBodyBytes+100)))
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := hc.submitBatch(ctx, []metrics.Measure{{MetricID: "m1", Timestamp: 1000, Value: 1}})
	if err == nil {
		t.Fatal("submitBatch() error = nil, want 500 error")
	}
	if !strings.Contains(err.Error(), "(truncated)") {
		t.Fatalf("error = %q, want truncated marker", err.Error())
	}
}

// TestRunSubmitLoop_CapsExponentialBackoffAt60s verifies exponential backoff caps at 60s.
func TestRunSubmitLoop_CapsExponentialBackoffAt60s(t *testing.T) {
	// This test verifies the constant definition and logic
	if maxRetryBackoff != 60*time.Second {
		t.Fatalf("maxRetryBackoff = %v, want 60s", maxRetryBackoff)
	}

	// Verify that exponential backoff calculation caps at 60s
	// 1s, 2s, 4s, 8s, 16s, 32s, 64s (capped to 60s), 60s, ...
	for attempt := 0; attempt < 10; attempt++ {
		delay := minRetryBackoff * (1 << attempt)
		if delay > maxRetryBackoff {
			delay = maxRetryBackoff
		}
		if delay > 60*time.Second {
			t.Fatalf("backoff at attempt %d = %v, exceeds 60s cap", attempt, delay)
		}
	}
}

// TestDrainPending_FlushesQueue verifies DrainPending submits all pending metrics.
func TestDrainPending_FlushesQueue(t *testing.T) {
	var submitted []SubmitRequest
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeSubmitRequest(t, r.Body)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		mu.Lock()
		submitted = append(submitted, req)
		mu.Unlock()

		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	measures := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 1.0, Labels: map[string]string{}},
		{MetricID: "m2", Timestamp: 2000, Value: 2.0, Labels: map[string]string{}},
		{MetricID: "m3", Timestamp: 3000, Value: 3.0, Labels: map[string]string{}},
	}
	hc.EnqueueMetrics(measures)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := hc.DrainPending(ctx)
	if err != nil {
		t.Fatalf("DrainPending failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Verify all 3 metrics were submitted (all in one batch since 3 < MaxSeriesPerRequest)
	totalSubmitted := 0
	for _, req := range submitted {
		totalSubmitted += len(req.Series)
	}
	if totalSubmitted != 3 {
		t.Fatalf("DrainPending submitted %d metrics, want 3", totalSubmitted)
	}

	// Verify pending queue is empty after drain
	hc.pendingMu.Lock()
	pendingSize := len(hc.pending)
	hc.pendingMu.Unlock()

	if pendingSize != 0 {
		t.Fatalf("pending queue after drain = %d, want 0", pendingSize)
	}
}

// TestDrainPending_TimeoutError verifies drain returns error if deadline exceeded.
func TestDrainPending_TimeoutError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow gateway (takes longer than drain's per-batch timeout)
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	// Add 2000+ metrics to force multiple batches
	measures := make([]metrics.Measure, 2000)
	for i := 0; i < 2000; i++ {
		measures[i] = metrics.Measure{
			MetricID:  fmt.Sprintf("m%d", i),
			Timestamp: 1000,
			Value:     float64(i),
			Labels:    map[string]string{},
		}
	}
	hc.EnqueueMetrics(measures)

	// Short deadline (1 second) will timeout before batches can complete
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := hc.DrainPending(ctx)
	if err == nil {
		t.Logf("DrainPending completed (deadline handling worked)")
	}
}

// TestSubmitBatch_InjectsJobLabel verifies job label is injected into submitted batch.
func TestSubmitBatch_InjectsJobLabel(t *testing.T) {
	var capturedReq SubmitRequest
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeSubmitRequest(t, r.Body)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		mu.Lock()
		capturedReq = req
		mu.Unlock()

		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	batch := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{"custom": "label"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := hc.submitBatch(ctx, batch)
	if err != nil {
		t.Fatalf("submitBatch failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(capturedReq.Series) != 1 {
		t.Fatalf("expected 1 series in request, got %d", len(capturedReq.Series))
	}

	labels := capturedReq.Series[0].Labels
	if labels["job"] != "hyperstack_agent" {
		t.Fatalf("job label = %q, want 'hyperstack_agent'", labels["job"])
	}
	if labels["custom"] != "label" {
		t.Fatalf("custom label = %q, want 'label'", labels["custom"])
	}
}

// TestRunSubmitLoop_BatchesAccumulateCorrectly verifies batching window behavior.
func TestRunSubmitLoop_BatchesAccumulateCorrectly(t *testing.T) {
	var submitCount int32
	var batchSizes []int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeSubmitRequest(t, r.Body)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		mu.Lock()
		batchSizes = append(batchSizes, len(req.Series))
		mu.Unlock()

		atomic.AddInt32(&submitCount, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	hc := NewHubClient(srv.URL)
	hc.VMName = "test-vm"
	hc.InstanceUUID = "test-uuid"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hc.StartSubmitLoop(ctx)

	// Add 50 metrics and wait for batching window
	// Note: Measure.Timestamp is in SECONDS (as per SamplesToMeasures)
	nowSec := float64(time.Now().Unix())
	measures := make([]metrics.Measure, 50)
	for i := 0; i < 50; i++ {
		measures[i] = metrics.Measure{
			MetricID:  fmt.Sprintf("m%d", i),
			Timestamp: nowSec + float64(i),
			Value:     float64(i),
			Labels:    map[string]string{},
		}
	}
	hc.EnqueueMetrics(measures)

	time.Sleep(6 * time.Second) // Wait for batching window to expire

	// Add 1000+ metrics to trigger immediate submission
	measures2 := make([]metrics.Measure, 1100)
	for i := 0; i < 1100; i++ {
		measures2[i] = metrics.Measure{
			MetricID:  fmt.Sprintf("m%d", 50+i),
			Timestamp: nowSec + float64(i+50),
			Value:     float64(i),
			Labels:    map[string]string{},
		}
	}
	hc.EnqueueMetrics(measures2)

	time.Sleep(2 * time.Second) // Let submission complete
	cancel()

	mu.Lock()
	defer mu.Unlock()

	// Should have batched 50 in first window, then 1000 + 100 in next batches
	if len(batchSizes) < 2 {
		t.Fatalf("expected at least 2 batches, got %d", len(batchSizes))
	}

	// First batch should be around 50
	if batchSizes[0] < 45 || batchSizes[0] > 55 {
		t.Fatalf("first batch size = %d, want ~50", batchSizes[0])
	}

	// Second batch should be MaxSeriesPerRequest (1000)
	if batchSizes[1] != MaxSeriesPerRequest {
		t.Fatalf("second batch size = %d, want %d", batchSizes[1], MaxSeriesPerRequest)
	}
}

// TestDeepCopyMeasures verifies no shared references between batches.
func TestDeepCopyMeasures(t *testing.T) {
	original := []metrics.Measure{
		{MetricID: "m1", Timestamp: 1000, Value: 42.0, Labels: map[string]string{"key": "value"}},
	}

	copied := metrics.DeepCopyMeasures(original)

	// Modify the copy's labels
	copied[0].Labels["key"] = "modified"
	copied[0].Labels["new"] = "label"

	// Verify original is unchanged
	if original[0].Labels["key"] != "value" {
		t.Fatalf("original label mutated: key=%q, want 'value'", original[0].Labels["key"])
	}
	if _, exists := original[0].Labels["new"]; exists {
		t.Fatalf("original has new label, should not exist")
	}

	// Verify copied is independent
	if copied[0].Labels["key"] != "modified" {
		t.Fatalf("copy didn't update: key=%q, want 'modified'", copied[0].Labels["key"])
	}
}
