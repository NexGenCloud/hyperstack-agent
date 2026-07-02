package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/client"
	"github.com/NexGenCloud/hyperstack-agent/internal/collectors"
	"github.com/NexGenCloud/hyperstack-agent/internal/config"
	"github.com/NexGenCloud/hyperstack-agent/internal/probes"
	"github.com/NexGenCloud/hyperstack-agent/internal/system"
	"github.com/NexGenCloud/hyperstack-agent/internal/update"
)

var (
	version = "dev"
	date    = "unknown"
)

const (
	autoUpdateInterval            = time.Hour
	autoUpdateValidationTimeout   = 2 * time.Minute
	metricsConfigSyncInterval     = time.Minute
	metricsConfigSyncTimeout      = 10 * time.Second
	startupMetadataInitialBackoff = 500 * time.Millisecond
	startupMetadataMaxBackoff     = 1 * time.Minute
	defaultHealthAddr             = "127.0.0.1:9100"
)

func main() {
	if handled, code := runDiagnosticCommand(os.Args[1:], os.Stdout); handled {
		os.Exit(code)
	}

	// Configure log level from environment (HYPERSTACK_LOG_LEVEL or LOG_LEVEL)
	logLevel := slog.LevelInfo
	for _, key := range []string{"HYPERSTACK_LOG_LEVEL", "LOG_LEVEL"} {
		if lvl := os.Getenv(key); lvl != "" {
			switch lvl {
			case "debug":
				logLevel = slog.LevelDebug
			case "warn":
				logLevel = slog.LevelWarn
			case "error":
				logLevel = slog.LevelError
			}
			if logLevel != slog.LevelInfo {
				break
			}
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	system.SetBuildInfo(version, date)

	slog.Info("Hyperstack agent starting", "version", version)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalCh)

	// Load configuration from environment
	cfg := config.Load()
	slog.Info("Hyperstack agent config loaded", "hub_url", cfg.Hub.URL)
	for _, warning := range config.SecurityWarnings(cfg) {
		slog.Warn("security configuration warning", "code", warning.Code, "message", warning.Message)
	}

	// Load startup metadata (uuid, infrahub_key, vm name, etc.) from metadata service once
	// Retry with exponential backoff if metadata service is unavailable
	var meta system.StartupMetadata
	var err error
	backoff := startupMetadataInitialBackoff
	attempt := 0

	for {
		attempt++
		meta, err = system.LoadStartupMetadata()
		if err == nil {
			break
		}
		slog.Warn("metadata service unavailable", "attempt", attempt, "backoff", backoff, "error", err)
		select {
		case <-ctx.Done():
			slog.Error("startup cancelled while waiting for metadata service", "attempts", attempt)
			os.Exit(1)
		case <-time.After(backoff):
			if backoff < startupMetadataMaxBackoff {
				backoff *= 2
				if backoff > startupMetadataMaxBackoff {
					backoff = startupMetadataMaxBackoff
				}
			}
		}
	}

	// Health server
	healthAddr := os.Getenv("HYPERSTACK_HEALTH_ADDR")
	if healthAddr == "" {
		healthAddr = defaultHealthAddr
	}
	if _, err := system.StartHealthServer(ctx, healthAddr); err != nil {
		slog.Warn("health server failed to start", "addr", healthAddr, "error", err)
	} else {
		slog.Info("health server listening", "addr", healthAddr)
	}

	// Hub client with API key from startup metadata. A KeyRefresher is
	// installed so that gateway 401 responses (e.g. after key rotation in
	// infrahub) trigger a one-shot re-fetch from the metadata service
	// rather than burning the agent's retry budget on a stale credential.
	hub := client.NewHubClient(cfg.Hub.URL).
		WithPath(config.AgentPushPath)
	hub.KeyRefresher = system.FetchInfrahubKey
	if meta.InfrahubKey != "" {
		hub.SetInfrahubKey(meta.InfrahubKey)
	}

	var scheduled []collectors.ScheduledCollector
	gpuCollectorEnabled := false

	if cfg.Node.Enable {
		scheduled = append(scheduled, collectors.ScheduledCollector{
			Collector: &collectors.NodeCollector{
				Hub:   hub,
				Probe: probes.NodeProbe{},
			},
			Interval: cfg.Node.ScrapeInterval,
		})
		slog.Info("node collector enabled", "interval", cfg.Node.ScrapeInterval)
	} else {
		slog.Info("node collector skipped", "reason", "disabled")
	}

	if cfg.GPU.Enable {
		if system.HasNvidiaGPU() {
			gpuCollectorEnabled = true
			scheduled = append(scheduled, collectors.ScheduledCollector{
				Collector: &collectors.GPUCollector{
					Hub:   hub,
					Probe: probes.GPUProbe{},
				},
				Interval: cfg.GPU.ScrapeInterval,
			})
			slog.Info("gpu collector enabled", "interval", cfg.GPU.ScrapeInterval)
		} else {
			slog.Info("gpu collector skipped", "reason", "nvidia-smi unavailable")
		}
	} else {
		slog.Info("gpu collector skipped", "reason", "disabled")
	}

	enabledCollectors := map[string]bool{
		"agent": true,
		"gpu":   gpuCollectorEnabled,
		"node":  cfg.Node.Enable,
	}
	scheduled = append(scheduled, collectors.ScheduledCollector{
		Collector: &collectors.AgentCollector{
			Hub:               hub,
			EnabledCollectors: enabledCollectors,
		},
		Interval: cfg.DefaultInterval,
	})
	slog.Info("agent self collector enabled", "interval", cfg.DefaultInterval)

	hub.SetCollectorsRunning(int64(len(scheduled)))

	// Submit startup metadata to gateway
	slog.Info("submitting startup metadata")
	system.SubmitStartupMetadata(ctx, hub, meta)

	// Set VM identity and start the async metrics submit loop
	hub.VMName = system.ResolveVMName()
	hub.InstanceUUID = system.GetCachedInstanceUUID()
	submitLoopDone := hub.StartSubmitLoop(ctx)

	mgr := &collectors.Manager{Scheduled: scheduled}
	if meta.UUID == "" {
		slog.Warn("metrics enablement sync disabled; instance uuid unavailable")
	} else {
		mgr.SetEnabled(false)
		hub.SetCollectorsRunning(0)
		go runMetricsEnabledSyncLoop(ctx, hub, mgr, meta.UUID, len(scheduled), metricsConfigSyncInterval)
	}

	managerErrCh := make(chan error, 1)
	go func() {
		managerErrCh <- mgr.Run(ctx)
	}()

	updateReadyCh := make(chan *update.Release, 1)
	var executablePath string
	var updater *update.Manager
	currentPath, err := os.Executable()
	if err != nil {
		slog.Warn("self-update disabled; unable to resolve current executable", "error", err)
	} else {
		executablePath = currentPath
		updateCheckURL := strings.TrimRight(cfg.Hub.URL, "/") + "/download"
		updater = update.NewManager(updateCheckURL, version)
		go runSelfUpdateLoop(ctx, updater, currentPath, autoUpdateInterval, updateReadyCh)
	}

	var restartRelease *update.Release
	externalShutdown := false
	for {
		select {
		case sig := <-signalCh:
			externalShutdown = true
			slog.Info("shutdown signal received", "signal", sig)
			cancel()
		case err := <-managerErrCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("manager exited", "error", err)
				os.Exit(1)
			}

			// Wait for submit loop to exit before draining (prevents double-submission on shutdown)
			slog.Info("collectors stopped, waiting for submit loop to exit")
			<-submitLoopDone

			// Collectors have exited and submit loop has stopped, now drain any remaining metrics
			slog.Info("submit loop exited, draining pending metrics")
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := hub.DrainPending(drainCtx); err != nil {
				slog.Warn("drain error", "error", err)
			}
			drainCancel()

			select {
			case sig := <-signalCh:
				externalShutdown = true
				slog.Info("shutdown signal received", "signal", sig)
			default:
			}

			if restartRelease != nil && updater != nil && !externalShutdown {
				if err := updater.PromoteRelease(executablePath, restartRelease); err != nil {
					slog.Error("self-update promote failed", "version", restartRelease.Version, "error", err)
					os.Exit(1)
				}
				slog.Info("restarting agent after self-update", "version", restartRelease.Version)
				if err := update.RestartProcess(executablePath); err != nil {
					slog.Error("self-update restart failed", "error", err)
					os.Exit(1)
				}
			} else if restartRelease != nil {
				_ = os.Remove(restartRelease.StagedPath)
				if externalShutdown {
					slog.Info("self-update skipped because shutdown was requested", "version", restartRelease.Version)
				}
			}
			slog.Info("Hyperstack agent shutdown complete")
			return
		case release := <-updateReadyCh:
			if release == nil || restartRelease != nil {
				continue
			}
			restartRelease = release
			slog.Info("self-update prepared; stopping collectors for restart", "version", release.Version)
			cancel()
		}
	}
}

func runMetricsEnabledSyncLoop(
	ctx context.Context,
	hub *client.HubClient,
	manager *collectors.Manager,
	uuid string,
	collectorCount int,
	interval time.Duration,
) {
	if interval <= 0 {
		interval = metricsConfigSyncInterval
	}

	lastEnabled := true
	hasLastEnabled := false
	sync := func() {
		syncCtx, cancel := context.WithTimeout(ctx, metricsConfigSyncTimeout)
		metadata, err := hub.GetMetadata(syncCtx, uuid)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("metrics enablement sync failed", "uuid", uuid, "error", err)
			return
		}

		enabled := metadata.MetricsEnabled
		manager.SetEnabled(enabled)
		if enabled {
			hub.SetCollectorsRunning(int64(collectorCount))
		} else {
			hub.SetCollectorsRunning(0)
		}

		if !hasLastEnabled || lastEnabled != enabled {
			if enabled {
				slog.Info("metrics enabled; collectors resumed", "uuid", uuid)
			} else {
				slog.Info("metrics disabled; collectors sleeping", "uuid", uuid)
			}
			lastEnabled = enabled
			hasLastEnabled = true
		}
	}

	sync()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sync()
		}
	}
}

func runDiagnosticCommand(args []string, stdout io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}

	switch args[0] {
	case "version", "--version", "-version":
		_, _ = fmt.Fprintln(stdout, version)
		return true, 0
	case "diagnose":
		if len(args) == 2 && args[1] == "status" {
			_, _ = fmt.Fprintf(stdout, "status=ok version=%s date=%s\n", version, date)
			return true, 0
		}
		_, _ = fmt.Fprintln(stdout, "usage: hyperstack-agent diagnose status")
		return true, 2
	default:
		return false, 0
	}
}

func runSelfUpdateLoop(
	ctx context.Context,
	updater *update.Manager,
	currentPath string,
	interval time.Duration,
	updateReadyCh chan<- *update.Release,
) {
	check := func() {
		release, err := updater.Check(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("self-update check failed", "url", updater.CheckURL, "error", err)
			return
		}
		if release == nil {
			return
		}
		slog.Info("new agent version available", "current_version", updater.CurrentVersion, "available_version", release.Version)
		validationCtx, validationCancel := context.WithTimeout(context.Background(), autoUpdateValidationTimeout)
		err = updater.DownloadRelease(validationCtx, release, currentPath)
		validationCancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("self-update download failed", "version", release.Version, "error", err)
			return
		}
		if ctx.Err() != nil {
			_ = os.Remove(release.StagedPath)
			return
		}
		select {
		case updateReadyCh <- release:
		default:
			_ = os.Remove(release.StagedPath)
		}
	}

	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
