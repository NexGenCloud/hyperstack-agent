package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/jitter"
	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
	"github.com/NexGenCloud/hyperstack-agent/internal/system"
)

const (
	MaxSeriesPerRequest = 1000
	MaxPendingSeries    = 10000
	MaxRetryAfter       = 60 * time.Second
	maxErrorBodyBytes   = 4096
)

var ErrMetricsDisabled = errors.New("metrics disabled for virtual machine")

// KeyRefresher returns a fresh raw Hyperstack key (no "VM " prefix) by re-fetching
// it from the source of truth (typically the OpenStack metadata service).
// Implementations should be safe to call concurrently.
type KeyRefresher func(ctx context.Context) (string, error)

type HubClient struct {
	HTTP    *http.Client
	BaseURL string
	Path    string

	// APIKey holds the full header value (including any "VM " prefix).
	// Direct access is retained for backward compatibility with existing
	// call sites; concurrent reads/writes go through apiKeyMu.
	apiKeyMu sync.RWMutex
	APIKey   string

	// rawHyperstackKey caches the un-prefixed key so we can detect when a
	// refresh returned the same (still-bad) credential and avoid hot loops.
	rawHyperstackKey string

	// KeyRefresher, when non-nil, is invoked on a 401 response. If it
	// returns a new key, the request is retried once without consuming a
	// normal retry slot.
	KeyRefresher KeyRefresher
	// refreshMu serializes refresh attempts so concurrent collectors
	// don't all hammer the metadata service on a flood of 401s.
	refreshMu       sync.Mutex
	lastRefreshAt   time.Time
	refreshCooldown time.Duration

	// VM identity for batch request construction (set once at startup)
	VMName       string
	InstanceUUID string

	// metrics queue for batching submissions
	pendingMu         sync.Mutex
	pending           []metrics.Measure
	notifyC           chan struct{}
	samplesSent       int64
	samplesFailed     int64
	collectorsRunning int64
}

func NewHubClient(baseURL string) *HubClient {
	// Disable keep-alive: agents push at ~0.13 req/s, so the per-request
	// TCP/TLS setup cost is negligible (~1-2 ms in-cluster). Keep-alive
	// pins each agent's connection through both haproxy (mode tcp,
	// roundrobin) and kube-proxy iptables for ~90 s, which causes
	// gateway-pod load imbalance and amplifies retry storms onto the
	// already-slow backend. Forcing a fresh connection per request lets
	// each request (and each retry) re-hash through both L4 LBs.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	// Use a longer dial timeout to handle occasional slow hubs gracefully.
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	// Cap time spent waiting for response headers from a slow gateway pod;
	// the surrounding http.Client.Timeout=30s only covers the full response.
	transport.ResponseHeaderTimeout = 15 * time.Second

	return &HubClient{
		HTTP: &http.Client{
			Timeout:       30 * time.Second,
			Transport:     transport,
			CheckRedirect: stripAPIKeyOnCrossHostRedirect,
		},
		BaseURL:         baseURL,
		refreshCooldown: 30 * time.Second,
		notifyC:         make(chan struct{}, 1),
	}
}

func (h *HubClient) WithPath(path string) *HubClient {
	h.Path = path
	return h
}

// SetHyperstackKey stores the raw Hyperstack key and updates the header value
// (prefixed with "VM ") used for outgoing requests. Safe for concurrent use.
// Passing an empty string clears the key.
func (h *HubClient) SetHyperstackKey(rawKey string) {
	h.apiKeyMu.Lock()
	defer h.apiKeyMu.Unlock()
	h.rawHyperstackKey = rawKey
	if rawKey == "" {
		h.APIKey = ""
		return
	}
	h.APIKey = "VM " + rawKey
}

// getAPIKey returns the current header value under read lock.
func (h *HubClient) getAPIKey() string {
	h.apiKeyMu.RLock()
	defer h.apiKeyMu.RUnlock()
	return h.APIKey
}

// getRawHyperstackKey returns the most recently stored raw key.
func (h *HubClient) getRawHyperstackKey() string {
	h.apiKeyMu.RLock()
	defer h.apiKeyMu.RUnlock()
	return h.rawHyperstackKey
}

// refreshHyperstackKey invokes KeyRefresher with cooldown + single-flight
// semantics. It returns true when the stored key changed as a result of the
// call, false otherwise (no refresher configured, cooldown active, refresher
// returned the same key, or refresher errored).
func (h *HubClient) refreshHyperstackKey(ctx context.Context, observedRaw string) bool {
	if h.KeyRefresher == nil {
		return false
	}
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	// If another goroutine already refreshed since we observed the bad key,
	// adopt its result instead of calling the refresher again.
	if current := h.getRawHyperstackKey(); current != "" && current != observedRaw {
		return true
	}

	if h.refreshCooldown > 0 && !h.lastRefreshAt.IsZero() {
		if time.Since(h.lastRefreshAt) < h.refreshCooldown {
			return false
		}
	}

	newKey, err := h.KeyRefresher(ctx)
	h.lastRefreshAt = time.Now()
	if err != nil {
		slog.Warn("Hyperstack key refresh failed", "error", err)
		return false
	}
	if newKey == "" {
		slog.Warn("Hyperstack key refresh returned empty key")
		return false
	}
	if newKey == observedRaw {
		slog.Warn("Hyperstack key refresh returned the same key; not retrying")
		return false
	}
	h.SetHyperstackKey(newKey)
	slog.Info("Hyperstack key refreshed after 401")
	return true
}

// doWithKeyRefresh executes an HTTP request and, if the response is 401 and a
// key refresh succeeds, rebuilds and retries the request exactly once.
// buildReq is called up to twice so that POST bodies (which are not replayable
// from a one-shot reader) can be reconstructed fresh for the retry.
func (h *HubClient) doWithKeyRefresh(
	ctx context.Context,
	buildReq func() (*http.Request, error),
	operation string,
) (*http.Response, error) {
	req, err := buildReq()
	if err != nil {
		return nil, err
	}
	observedRawKey := h.getRawHyperstackKey()
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && h.refreshHyperstackKey(ctx, observedRawKey) {
		closeResponseBody(resp, operation+" 401")
		req, err = buildReq()
		if err != nil {
			return nil, err
		}
		resp, err = h.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (h *HubClient) SetCollectorsRunning(count int64) {
	atomic.StoreInt64(&h.collectorsRunning, count)
}

func (h *HubClient) GetCollectorsRunning() int64 {
	return atomic.LoadInt64(&h.collectorsRunning)
}

func (h *HubClient) GetSamplesSent() int64 {
	return atomic.LoadInt64(&h.samplesSent)
}

func (h *HubClient) GetSamplesFailed() int64 {
	return atomic.LoadInt64(&h.samplesFailed)
}

// EnqueueMetrics adds measures to the submission queue for async batching.
// This is non-blocking; the actual submission is handled by the submit goroutine.
func (h *HubClient) EnqueueMetrics(measures []metrics.Measure) {
	if len(measures) == 0 {
		return
	}
	dropped := 0
	h.pendingMu.Lock()
	h.pending = append(h.pending, measures...)
	if len(h.pending) > MaxPendingSeries {
		dropped = len(h.pending) - MaxPendingSeries
		h.pending = append([]metrics.Measure(nil), h.pending[dropped:]...)
	}
	h.pendingMu.Unlock()
	if dropped > 0 {
		atomic.AddInt64(&h.samplesFailed, int64(dropped))
		slog.Warn("dropped oldest pending samples", "count", dropped, "max_pending_series", MaxPendingSeries)
	}
	select {
	case h.notifyC <- struct{}{}:
	default:
	}
}

// StartSubmitLoop starts the background goroutine that handles batching and
// submission of enqueued metrics. Should be called once during initialization.
// Returns a channel that closes when the submit loop exits.
func (h *HubClient) StartSubmitLoop(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.runSubmitLoop(ctx)
	}()
	return done
}

const (
	batchingWindow  = 5 * time.Second
	minRetryBackoff = 1 * time.Second
	maxRetryBackoff = 60 * time.Second
	// samples older than this are never submitted
	// aligns with gateway's max sample age and prom out-of-order window
	maxSampleAge = 5 * time.Minute
)

// filterStaleMetrics removes samples older than maxSampleAge from the batch.
// Returns the filtered batch and the count of dropped stale samples.
func filterStaleMetrics(batch []metrics.Measure) ([]metrics.Measure, int) {
	cutoffSec := float64(time.Now().Add(-maxSampleAge).Unix())

	kept := 0
	for i := range batch {
		if batch[i].Timestamp >= cutoffSec {
			batch[kept] = batch[i]
			kept++
		}
	}
	dropped := len(batch) - kept
	return batch[:kept], dropped
}

// runSubmitLoop batches and submits metrics with single-path retry logic.
// All retries happen in this loop; submitBatch() makes only one attempt per call.
func (h *HubClient) runSubmitLoop(ctx context.Context) {
	retryAttempts := 0
	successesSinceFail := 0 // require 3 successes in a row to fully reset backoff

	for {
		// Wait until there is something to send
		h.pendingMu.Lock()
		hasPending := len(h.pending) > 0
		h.pendingMu.Unlock()

		if !hasPending {
			select {
			case <-ctx.Done():
				return
			case <-h.notifyC:
			}
			continue
		}

		// Batching window: allow metrics from other collectors to accumulate
		// before submitting. Break early if batch reaches 1000 samples or deadline expires.
		batchingDeadline := time.Now().Add(batchingWindow)

	batchingLoop:
		for {
			h.pendingMu.Lock()
			pendingSize := len(h.pending)
			h.pendingMu.Unlock()

			// Break if batch is full (1000 samples)
			if pendingSize >= MaxSeriesPerRequest {
				break batchingLoop
			}

			// Check time remaining in window
			timeRemaining := time.Until(batchingDeadline)
			if timeRemaining <= 0 {
				break batchingLoop
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(timeRemaining):
				break batchingLoop
			case <-h.notifyC:
				continue
			}
		}

		// Snapshot and deep-copy batch to prevent mutations
		h.pendingMu.Lock()
		if len(h.pending) == 0 {
			h.pendingMu.Unlock()
			continue
		}
		batchSize := len(h.pending)
		if batchSize > MaxSeriesPerRequest {
			batchSize = MaxSeriesPerRequest
		}
		batch := metrics.DeepCopyMeasures(h.pending[:batchSize])
		h.pendingMu.Unlock()

		// Sort batch by timestamp (oldest first)
		sort.Slice(batch, func(i, j int) bool {
			return batch[i].Timestamp < batch[j].Timestamp
		})

		// Filter out samples older than maxSampleAge
		batch, staleCount := filterStaleMetrics(batch)
		if staleCount > 0 {
			slog.Info("dropped stale samples", "count", staleCount)
			atomic.AddInt64(&h.samplesFailed, int64(staleCount))
		}

		// If all samples were stale, remove batch from queue and continue
		if len(batch) == 0 {
			h.pendingMu.Lock()
			h.pending = h.pending[batchSize:]
			h.pendingMu.Unlock()
			continue
		}

		// Submit once (no internal retries)
		submitStart := time.Now()
		retryAfter, err := h.submitBatch(ctx, batch)
		submitDuration := time.Since(submitStart)

		if err == nil {
			// Success: remove batch from queue, track success streak
			h.pendingMu.Lock()
			h.pending = h.pending[batchSize:]
			h.pendingMu.Unlock()

			system.SetLastSubmitMs(submitDuration.Milliseconds())
			successesSinceFail++
			// Only fully reset backoff after 3 consecutive successes (guards against flakiness)
			if successesSinceFail >= 3 {
				retryAttempts = 0
				successesSinceFail = 0
			}

			// Drain stale notify so next idle wait is clean
			select {
			case <-h.notifyC:
			default:
			}

			slog.Info("hub batch submitted", "series", batchSize, "duration_ms", int(submitDuration/time.Millisecond))
		} else if errors.Is(err, ErrMetricsDisabled) {
			h.pendingMu.Lock()
			h.pending = h.pending[batchSize:]
			h.pendingMu.Unlock()

			successesSinceFail = 0
			retryAttempts = 0

			slog.Info("hub batch dropped; metrics disabled for vm", "size", batchSize, "error", err)
		} else {
			// Failure: increment failed counter (once per batch, not per retry attempt),
			// calculate backoff, and retry next iteration. Batch stays in pending queue automatically.
			atomic.AddInt64(&h.samplesFailed, int64(batchSize))
			successesSinceFail = 0 // reset success streak on any failure

			var backoff time.Duration
			if retryAfter > 0 {
				// Gateway said: wait this long; cap it to avoid remote sleep amplification.
				backoff = capRetryAfter(retryAfter)
				slog.Warn("hub batch submit failed, respecting Retry-After",
					"size", batchSize, "retry_after_sec", int(retryAfter.Seconds()),
					"capped_backoff_sec", int(backoff.Seconds()),
					"error", err)
			} else {
				// Exponential backoff with jitter: 1s, 2s, 4s, 8s, 16s, 32s, 60s, 60s, ...
				delay := minRetryBackoff * (1 << retryAttempts)
				if delay > maxRetryBackoff {
					delay = maxRetryBackoff
				}
				// Add random jitter: [0, delay/2].
				jitterDelay := jitter.Duration(delay / 2)
				backoff = delay/2 + jitterDelay
				retryAttempts++

				slog.Warn("hub batch submit failed, backing off",
					"size", batchSize, "backoff_sec", int(backoff.Seconds()),
					"attempt", retryAttempts, "error", err)
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				// Retry next iteration
			}
		}
	}
}

type SubmitRequest struct {
	Series []metrics.Measure `json:"series"`
	Name   string            `json:"name,omitempty"`
	// Keep the wire field as "uuid" for gateway compatibility.
	InstanceUUID string `json:"uuid,omitempty"`
}

// parseRetryAfter parses a Retry-After header value per RFC 7231.
// Returns (duration, true) if parsed successfully, (0, false) otherwise.
// Supports two formats: delay-seconds (integer) and HTTP-date.
func parseRetryAfter(header string) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	// Try delay-seconds first (integer number of seconds)
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	// Try HTTP-date format
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func capRetryAfter(d time.Duration) time.Duration {
	if d > MaxRetryAfter {
		return MaxRetryAfter
	}
	return d
}

// DrainPending attempts to flush all pending metrics before shutdown.
// Submits batches with short timeouts (no retries), exits when queue empty or context deadline exceeded.
func (h *HubClient) DrainPending(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}

	for {
		h.pendingMu.Lock()
		if len(h.pending) == 0 {
			h.pendingMu.Unlock()
			slog.Info("pending queue drained successfully")
			return nil
		}

		batchSize := len(h.pending)
		if batchSize > MaxSeriesPerRequest {
			batchSize = MaxSeriesPerRequest
		}
		batch := metrics.DeepCopyMeasures(h.pending[:batchSize])
		h.pendingMu.Unlock()

		// Sort batch by timestamp (oldest first)
		sort.Slice(batch, func(i, j int) bool {
			return batch[i].Timestamp < batch[j].Timestamp
		})

		// Submit with per-batch timeout (don't retry on drain)
		submitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := h.submitBatch(submitCtx, batch)
		cancel()

		if err == nil {
			// Success: remove from queue
			h.pendingMu.Lock()
			h.pending = h.pending[batchSize:]
			h.pendingMu.Unlock()
			slog.Debug("drain: batch submitted", "size", batchSize)
		} else {
			// Failure: log and skip (don't retry during drain)
			slog.Warn("drain: batch submit failed, skipping", "size", batchSize, "error", err)
			h.pendingMu.Lock()
			h.pending = h.pending[batchSize:]
			h.pendingMu.Unlock()
		}

		// Check deadline
		if time.Now().After(deadline) {
			h.pendingMu.Lock()
			remaining := len(h.pending)
			h.pendingMu.Unlock()
			if remaining > 0 {
				slog.Warn("drain deadline exceeded", "remaining_metrics", remaining)
				return fmt.Errorf("drain timeout: %d metrics not submitted", remaining)
			}
			return nil
		}
	}
}

// submitBatch makes a single submission attempt with no retries.
// Returns (Retry-After duration if provided by server, error).
// Injects job label into batch copy (no mutation of pending queue).
func (h *HubClient) submitBatch(ctx context.Context, batch []metrics.Measure) (time.Duration, error) {
	if len(batch) == 0 {
		return 0, nil
	}

	req := SubmitRequest{
		Series:       batch,
		Name:         h.VMName,
		InstanceUUID: h.InstanceUUID,
	}

	// Inject job label into batch copy (no mutation of pending queue)
	for i := range req.Series {
		if req.Series[i].Labels == nil {
			req.Series[i].Labels = make(map[string]string)
		}
		req.Series[i].Labels["job"] = "hyperstack_agent"
	}

	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	if os.Getenv("HYPERSTACK_DEBUG_LOG_PAYLOAD") == "1" {
		slog.Info("hub submit payload", "bytes", len(body))
	}

	reqURL := fmt.Sprintf("%s/%s", h.BaseURL, h.Path)

	resp, err := h.doWithKeyRefresh(ctx, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		r.Header.Set("Content-Type", "application/json")
		if apiKey := h.getAPIKey(); apiKey != "" {
			r.Header.Set("api-key", apiKey)
		}
		return r, nil
	}, "submit batch")
	if err != nil {
		return 0, fmt.Errorf("http error: %w", err)
	}
	defer closeResponseBody(resp, "submit batch")

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Success: update samples sent counter
		atomic.AddInt64(&h.samplesSent, int64(len(batch)))
		return 0, nil
	}

	respBody, readErr := readErrorBody(resp.Body)

	if resp.StatusCode == http.StatusPreconditionFailed {
		return 0, fmt.Errorf("%w: %s", ErrMetricsDisabled, respBody)
	}

	// Failure: extract Retry-After if present
	retryAfterHeader := resp.Header.Get("Retry-After")
	retryAfter, _ := parseRetryAfter(retryAfterHeader)
	retryAfter = capRetryAfter(retryAfter)

	if readErr != nil {
		return retryAfter, fmt.Errorf("http %d; read response body: %w", resp.StatusCode, readErr)
	}
	return retryAfter, fmt.Errorf("http %d: %s", resp.StatusCode, respBody)
}

// Submit provides backward compatibility for callers (e.g., system.SubmitStartupMetadata)
// that expect the old Submit(context, path, payload) interface.
// For metadata submissions, it makes a single POST/PATCH attempt with no retry loop.
func (h *HubClient) Submit(ctx context.Context, path string, payload any) error {
	// Convert payload to JSON bytes
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// For metadata submissions, use the provided path; otherwise use h.Path
	usePath := path
	if !strings.Contains(path, "metadata") && h.Path != "" {
		usePath = h.Path
	}

	reqURL := fmt.Sprintf("%s/%s", h.BaseURL, usePath)

	// Use PATCH for metadata, POST for metrics
	method := http.MethodPost
	if strings.Contains(path, "metadata") {
		method = http.MethodPatch
	}

	resp, err := h.doWithKeyRefresh(ctx, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		if apiKey := h.getAPIKey(); apiKey != "" {
			r.Header.Set("api-key", apiKey)
		}
		return r, nil
	}, "submit metadata")
	if err != nil {
		return err
	}
	defer closeResponseBody(resp, "submit metadata")

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	respBody, err := readErrorBody(resp.Body)
	if err != nil {
		return fmt.Errorf("http %d; read response body: %w", resp.StatusCode, err)
	}
	return fmt.Errorf("http %d: %s", resp.StatusCode, respBody)
}

func readErrorBody(body io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxErrorBodyBytes {
		return string(data[:maxErrorBodyBytes]) + "... (truncated)", nil
	}
	return string(data), nil
}

func stripAPIKeyOnCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	previous := via[len(via)-1]
	if !sameURLAuthority(previous.URL, req.URL) {
		req.Header.Del("api-key")
	}
	return nil
}

func sameURLAuthority(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func closeResponseBody(resp *http.Response, operation string) {
	if resp == nil || resp.Body == nil {
		return
	}
	if err := resp.Body.Close(); err != nil {
		slog.Debug("response body close failed", "operation", operation, "error", err)
	}
}

// VMMetadata holds VM-level configuration returned by the gateway.
//
// Fix 7a: MetricsEnabled is a *bool rather than a plain bool so that a missing
// JSON field is distinguishable from an explicit false. A nil value means the
// gateway did not set the field (e.g. during a rollout or a response mismatch)
// and callers should treat it as "default-enabled". A plain bool would make a
// dropped field silently disable all collectors.
type VMMetadata struct {
	MetricsEnabled *bool `json:"metrics_enabled"`
}

func (h *HubClient) GetMetadata(ctx context.Context, uuid string) (*VMMetadata, error) {
	escapedUUID := url.PathEscape(strings.TrimSpace(uuid))
	if escapedUUID == "" {
		return nil, errors.New("metadata uuid is required")
	}

	reqURL := fmt.Sprintf("%s/api/v1/metadata/%s", strings.TrimRight(h.BaseURL, "/"), escapedUUID)

	// Fix 7b: mirror the refresh-and-retry pattern used by SubmitBatch and
	// Submit so that a rotated Hyperstack key does not leave the sync loop stuck
	// on 401 and permanently unable to re-enable collection.
	resp, err := h.doWithKeyRefresh(ctx, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		if apiKey := h.getAPIKey(); apiKey != "" {
			r.Header.Set("api-key", apiKey)
		}
		return r, nil
	}, "get metadata")
	if err != nil {
		return nil, err
	}
	defer closeResponseBody(resp, "get metadata")

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := readErrorBody(resp.Body)
		return nil, fmt.Errorf("metadata status %d: %s", resp.StatusCode, respBody)
	}

	var metadata VMMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}
