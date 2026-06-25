package system

import (
	"context"
	"log/slog"
	"os/exec"
	"time"
)

// HasNvidiaGPU returns true if nvidia-smi is present and returns successfully.
func HasNvidiaGPU() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/env", "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	cmd.Env = append(cmd.Env, "PATH=/usr/bin:/usr/local/bin:/bin")
	if err := cmd.Run(); err != nil {
		slog.Debug("gpu detect: nvidia-smi not runnable", "error", err)
		return false
	}
	slog.Debug("gpu detect: nvidia-smi ok")
	return true
}
