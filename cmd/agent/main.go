package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NexGenCloud/hyperstack-agent/internal/client"
	"github.com/NexGenCloud/hyperstack-agent/internal/collectors"
	"github.com/NexGenCloud/hyperstack-agent/internal/config"
	"github.com/NexGenCloud/hyperstack-agent/internal/probes"
	"github.com/NexGenCloud/hyperstack-agent/internal/system"
)

var (
	version = "dev"
	date    = "unknown"
)

const (
	startupMetadataInitialBackoff = 500 * time.Millisecond
	startupMetadataMaxBackoff     = 1 * time.Minute
	defaultHealthAddr             = "127.0.0.1:9100"
)

func main() {
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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
	if err := mgr.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("manager exited", "error", err)
		os.Exit(1)
	}

	// Wait for submit loop to exit before draining (prevents double-submission on shutdown)
	slog.Info("collectors stopped, waiting for submit loop to exit")
	<-submitLoopDone

	// Collectors have exited and submit loop has stopped, now drain any remaining metrics
	slog.Info("submit loop exited, draining pending metrics")
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()

	if err := hub.DrainPending(drainCtx); err != nil {
		slog.Warn("drain error", "error", err)
	}

	slog.Info("Hyperstack agent shutdown complete")
}
