package system

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StartHealthServer starts a minimal health server on the given address.
// It returns a function to gracefully shutdown.
var startTimeUnix atomic.Int64
var buildVersion atomic.Value
var buildDate atomic.Value
var lastScrapeMs atomic.Int64
var lastSubmitMs atomic.Int64

// exporter metrics
type exporterState struct {
	up       int64
	restarts int64
	version  string
}

var (
	exporterMu    sync.RWMutex
	exporterStats = map[string]*exporterState{}

	probeMu          sync.RWMutex
	probeLastSuccess = map[string]time.Time{}
	probeLastFailure = map[string]time.Time{}
	probeLastError   = map[string]string{}
)

func StartHealthServer(ctx context.Context, addr string) (func(context.Context) error, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		// Minimal self-metrics in Prometheus text format
		if startTimeUnix.Load() == 0 {
			startTimeUnix.Store(time.Now().Unix())
		}
		uptime := time.Now().Unix() - startTimeUnix.Load()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// build info as a const gauge with labels
		v, _ := buildVersion.Load().(string)
		d, _ := buildDate.Load().(string)
		metricsText := "# HELP nagent_uptime_seconds Agent uptime in seconds\n" +
			"# TYPE nagent_uptime_seconds gauge\n" +
			"nagent_uptime_seconds " + fmtInt64(uptime) + "\n" +
			"# HELP nagent_build_info Agent build information\n" +
			"# TYPE nagent_build_info gauge\n" +
			"nagent_build_info{version=\"" + v + "\",date=\"" + d + "\"} 1\n" +
			"# HELP nagent_last_scrape_ms Duration of last metrics scrape in milliseconds\n" +
			"# TYPE nagent_last_scrape_ms gauge\n" +
			"nagent_last_scrape_ms " + fmtInt64(lastScrapeMs.Load()) + "\n" +
			"# HELP nagent_last_submit_ms Duration of last hub submission in milliseconds\n" +
			"# TYPE nagent_last_submit_ms gauge\n" +
			"nagent_last_submit_ms " + fmtInt64(lastSubmitMs.Load()) + "\n"

		// exporter health metrics
		metricsText += "# HELP nagent_exporter_up Exporter liveness (1=up)\n"
		metricsText += "# TYPE nagent_exporter_up gauge\n"
		metricsText += "# HELP nagent_exporter_restarts_total Exporter restarts since agent start\n"
		metricsText += "# TYPE nagent_exporter_restarts_total counter\n"
		metricsText += "# HELP nagent_exporter_info Exporter build information\n"
		metricsText += "# TYPE nagent_exporter_info gauge\n"
		exporterMu.RLock()
		names := make([]string, 0, len(exporterStats))
		for name := range exporterStats {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			st := exporterStats[name]
			metricsText += "nagent_exporter_up{name=\"" + sanitizeLabel(name) + "\"} " + fmtInt64(st.up) + "\n"
			metricsText += "nagent_exporter_restarts_total{name=\"" + sanitizeLabel(name) + "\"} " + fmtInt64(st.restarts) + "\n"
			metricsText += "nagent_exporter_info{name=\"" + sanitizeLabel(name) + "\",version=\"" + sanitizeLabel(st.version) + "\"} 1\n"
		}
		exporterMu.RUnlock()

		metricsText += "# HELP nagent_probe_last_success_timestamp_seconds Last successful probe timestamp\n"
		metricsText += "# TYPE nagent_probe_last_success_timestamp_seconds gauge\n"
		metricsText += "# HELP nagent_probe_last_failure_timestamp_seconds Last failed probe timestamp\n"
		metricsText += "# TYPE nagent_probe_last_failure_timestamp_seconds gauge\n"

		probeMu.RLock()
		probeNames := map[string]struct{}{}
		for name := range probeLastSuccess {
			probeNames[name] = struct{}{}
		}
		for name := range probeLastFailure {
			probeNames[name] = struct{}{}
		}
		if len(probeNames) > 0 {
			names = names[:0]
			for name := range probeNames {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if ts := probeLastSuccess[name]; !ts.IsZero() {
					metricsText += "nagent_probe_last_success_timestamp_seconds{name=\"" + sanitizeLabel(name) + "\"} " + strconv.FormatInt(ts.Unix(), 10) + "\n"
				}
				if ts := probeLastFailure[name]; !ts.IsZero() {
					metricsText += "nagent_probe_last_failure_timestamp_seconds{name=\"" + sanitizeLabel(name) + "\"} " + strconv.FormatInt(ts.Unix(), 10) + "\n"
				}
			}
		}
		probeMu.RUnlock()
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
		// Prometheus metrics are returned as text/plain and all label values are escaped.
		_, _ = w.Write([]byte(metricsText))
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("health server stopped unexpectedly", "addr", addr, "error", err)
		}
	}()

	shutdown := func(c context.Context) error { return srv.Shutdown(c) }
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return shutdown, nil
}

func fmtInt64(v int64) string { return strconv.FormatInt(v, 10) }

// SetBuildInfo sets version/date labels for /metrics output.
func SetBuildInfo(version, date string) {
	buildVersion.Store(version)
	buildDate.Store(date)
}

func GetBuildInfo() (string, string) {
	version, _ := buildVersion.Load().(string)
	date, _ := buildDate.Load().(string)
	return version, date
}

// SetLastScrapeMs records the most recent scrape duration in milliseconds.
func SetLastScrapeMs(ms int64) { lastScrapeMs.Store(ms) }

// SetLastSubmitMs records the most recent hub submission duration in milliseconds.
func SetLastSubmitMs(ms int64) { lastSubmitMs.Store(ms) }

func GetLastScrapeMs() int64 { return lastScrapeMs.Load() }

func GetLastSubmitMs() int64 { return lastSubmitMs.Load() }

// Exporter health helpers
func ensureExporter(name string) *exporterState {
	exporterMu.Lock()
	st, ok := exporterStats[name]
	if !ok {
		st = &exporterState{}
		exporterStats[name] = st
	}
	exporterMu.Unlock()
	return st
}

// SetExporterUp sets the exporter up gauge to 1 or 0.
func SetExporterUp(name string, up bool) {
	st := ensureExporter(name)
	if up {
		atomic.StoreInt64(&st.up, 1)
	} else {
		atomic.StoreInt64(&st.up, 0)
	}
}

// IncExporterRestart increments the exporter restarts counter.
func IncExporterRestart(name string) {
	st := ensureExporter(name)
	atomic.AddInt64(&st.restarts, 1)
}

// SetExporterVersion records the exporter version label once known.
func SetExporterVersion(name, version string) {
	st := ensureExporter(name)
	exporterMu.Lock()
	st.version = version
	exporterMu.Unlock()
}

func sanitizeLabel(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}

// RecordProbeSuccess updates probe bookkeeping when a collection succeeded.
func RecordProbeSuccess(name string) {
	SetExporterUp(name, true)
	probeMu.Lock()
	probeLastSuccess[name] = time.Now()
	delete(probeLastError, name)
	probeMu.Unlock()
}

// RecordProbeFailure updates probe bookkeeping when a collection failed.
func RecordProbeFailure(name string, err error) {
	SetExporterUp(name, false)
	probeMu.Lock()
	probeLastFailure[name] = time.Now()
	if err != nil {
		probeLastError[name] = err.Error()
	}
	probeMu.Unlock()
}

// ProbeHealthyWithin reports whether the probe succeeded within the provided window.
func ProbeHealthyWithin(name string, window time.Duration) bool {
	if window <= 0 {
		window = 30 * time.Second
	}
	probeMu.RLock()
	last := probeLastSuccess[name]
	probeMu.RUnlock()
	if last.IsZero() {
		return false
	}
	return time.Since(last) <= window
}
