package probes

import (
	"context"
	"fmt"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
)

const gpuMetricPrefix = "hyperstack_nvidia_gpu_"

// GPUProbe collects metrics via NVML to mirror the NVIDIA DCGM exporter.
type GPUProbe struct{}

func (GPUProbe) Name() string { return "gpu" }

func (GPUProbe) Collect(ctx context.Context) ([]metrics.Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml init failed: %s", nvml.ErrorString(ret))
	}
	defer func() {
		_ = nvml.Shutdown()
	}()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml DeviceGetCount failed: %s", nvml.ErrorString(ret))
	}
	if count == 0 {
		return nil, nil
	}

	var samples []metrics.Sample
	for idx := 0; idx < count; idx++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		device, ret := nvml.DeviceGetHandleByIndex(idx)
		if ret != nvml.SUCCESS {
			continue
		}

		name, _ := nvml.DeviceGetName(device)
		uuid, _ := nvml.DeviceGetUUID(device)
		labels := map[string]string{
			"index": fmt.Sprintf("%d", idx),
			"name":  name,
			"uuid":  uuid,
		}

		if util, ret := nvml.DeviceGetUtilizationRates(device); ret == nvml.SUCCESS {
			samples = append(samples,
				newSample(gpuMetricPrefix+"utilization_gpu_percent", labels, float64(util.Gpu)),
				newSample(gpuMetricPrefix+"utilization_memory_percent", labels, float64(util.Memory)),
			)
		}

		if memInfo, ret := nvml.DeviceGetMemoryInfo(device); ret == nvml.SUCCESS {
			samples = append(samples,
				newSample(gpuMetricPrefix+"memory_total_bytes", labels, float64(memInfo.Total)),
				newSample(gpuMetricPrefix+"memory_used_bytes", labels, float64(memInfo.Used)),
				newSample(gpuMetricPrefix+"memory_free_bytes", labels, float64(memInfo.Free)),
			)
		}

		if temp, ret := nvml.DeviceGetTemperature(device, nvml.TEMPERATURE_GPU); ret == nvml.SUCCESS {
			samples = append(samples, newSample(gpuMetricPrefix+"temperature_gpu_celsius", labels, float64(temp)))
		}

		if fan, ret := nvml.DeviceGetFanSpeed(device); ret == nvml.SUCCESS {
			samples = append(samples, newSample(gpuMetricPrefix+"fan_speed_percent", labels, float64(fan)))
		}

		if power, ret := nvml.DeviceGetPowerUsage(device); ret == nvml.SUCCESS {
			samples = append(samples, newSample(gpuMetricPrefix+"power_draw_watts", labels, float64(power)/1000.0))
		}
	}

	return samples, nil
}
