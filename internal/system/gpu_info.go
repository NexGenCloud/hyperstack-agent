package system

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// GPUInfo describes a GPU as reported by nvidia-smi.
type GPUInfo struct {
	Index string
	UUID  string
	Name  string
}

// QueryNvidiaGPUs returns the list of GPUs visible to the current process by invoking nvidia-smi.
func QueryNvidiaGPUs(ctx context.Context) ([]GPUInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/env", "nvidia-smi", "--query-gpu=index,uuid,name", "--format=csv,noheader")
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/usr/local/bin:/bin")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	infos := make([]GPUInfo, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		info := GPUInfo{
			Index: strings.TrimSpace(parts[0]),
			UUID:  strings.TrimSpace(parts[1]),
			Name:  strings.TrimSpace(strings.Join(parts[2:], ",")),
		}
		infos = append(infos, info)
	}
	return infos, nil
}
