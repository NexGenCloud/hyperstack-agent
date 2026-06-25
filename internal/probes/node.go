package probes

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	"github.com/NexGenCloud/hyperstack-agent/internal/metrics"
)

const metricPrefix = "hyperstack_node_"

// NodeProbe collects system-level statistics similar to the Prometheus node_exporter.
type NodeProbe struct{}

func (NodeProbe) Name() string { return "node" }

func (NodeProbe) Collect(ctx context.Context) ([]metrics.Sample, error) {
	var samples []metrics.Sample

	if avg, err := load.AvgWithContext(ctx); err == nil {
		samples = append(samples,
			newSample(metricPrefix+"load1", nil, avg.Load1),
			newSample(metricPrefix+"load5", nil, avg.Load5),
			newSample(metricPrefix+"load15", nil, avg.Load15),
		)
	}

	if cpuTimes, err := cpu.TimesWithContext(ctx, false); err == nil {
		for _, ct := range cpuTimes {
			labels := map[string]string{"cpu": ct.CPU, "mode": "user"}
			samples = append(samples, newSample(metricPrefix+"cpu_seconds_total", labels, ct.User))
			labels = map[string]string{"cpu": ct.CPU, "mode": "system"}
			samples = append(samples, newSample(metricPrefix+"cpu_seconds_total", labels, ct.System))
			labels = map[string]string{"cpu": ct.CPU, "mode": "idle"}
			samples = append(samples, newSample(metricPrefix+"cpu_seconds_total", labels, ct.Idle))
			labels = map[string]string{"cpu": ct.CPU, "mode": "iowait"}
			samples = append(samples, newSample(metricPrefix+"cpu_seconds_total", labels, ct.Iowait))
		}
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		samples = append(samples,
			newSample(metricPrefix+"memory_MemTotal_bytes", nil, float64(vm.Total)),
			newSample(metricPrefix+"memory_MemAvailable_bytes", nil, float64(vm.Available)),
			newSample(metricPrefix+"memory_Active_bytes", nil, float64(vm.Active)),
			newSample(metricPrefix+"memory_Inactive_bytes", nil, float64(vm.Inactive)),
		)
	}

	if partitions, err := disk.PartitionsWithContext(ctx, false); err == nil {
		for _, part := range partitions {
			if part.Mountpoint == "" {
				continue
			}
			if usage, err := disk.UsageWithContext(ctx, part.Mountpoint); err == nil {
				labels := map[string]string{
					"mountpoint": part.Mountpoint,
					"fstype":     part.Fstype,
				}
				samples = append(samples,
					newSample(metricPrefix+"filesystem_size_bytes", labels, float64(usage.Total)),
					newSample(metricPrefix+"filesystem_free_bytes", labels, float64(usage.Free)),
					newSample(metricPrefix+"filesystem_avail_bytes", labels, float64(usage.Free)),
					newSample(metricPrefix+"filesystem_usage_percent", labels, usage.UsedPercent),
				)
			}
		}
	}

	if ioCounters, err := disk.IOCountersWithContext(ctx); err == nil {
		for device, stats := range ioCounters {
			labels := map[string]string{"device": device}
			samples = append(samples,
				newSample(metricPrefix+"disk_read_bytes_total", labels, float64(stats.ReadBytes)),
				newSample(metricPrefix+"disk_written_bytes_total", labels, float64(stats.WriteBytes)),
				newSample(metricPrefix+"disk_reads_completed_total", labels, float64(stats.ReadCount)),
				newSample(metricPrefix+"disk_writes_completed_total", labels, float64(stats.WriteCount)),
				newSample(metricPrefix+"disk_read_time_seconds_total", labels, float64(stats.ReadTime)/1000.0),
				newSample(metricPrefix+"disk_write_time_seconds_total", labels, float64(stats.WriteTime)/1000.0),
			)
		}
	}

	if netIO, err := net.IOCountersWithContext(ctx, true); err == nil {
		for _, nic := range netIO {
			labels := map[string]string{"device": nic.Name}
			samples = append(samples,
				newSample(metricPrefix+"network_receive_bytes_total", labels, float64(nic.BytesRecv)),
				newSample(metricPrefix+"network_transmit_bytes_total", labels, float64(nic.BytesSent)),
				newSample(metricPrefix+"network_receive_packets_total", labels, float64(nic.PacketsRecv)),
				newSample(metricPrefix+"network_transmit_packets_total", labels, float64(nic.PacketsSent)),
				newSample(metricPrefix+"network_receive_errs_total", labels, float64(nic.Errin)),
				newSample(metricPrefix+"network_transmit_errs_total", labels, float64(nic.Errout)),
			)
		}
	}

	if info, err := host.InfoWithContext(ctx); err == nil {
		if info.BootTime > 0 {
			samples = append(samples, newSample(metricPrefix+"boot_time_seconds", nil, float64(info.BootTime)))
		}
		unameLabels := map[string]string{
			"sysname":  info.OS,
			"release":  info.KernelVersion,
			"version":  info.PlatformVersion,
			"machine":  info.KernelArch,
			"nodename": info.Hostname,
		}
		samples = append(samples, newSample(metricPrefix+"uname_info", unameLabels, 1.0))
	}

	now := float64(time.Now().Unix())
	samples = append(samples, newSample(metricPrefix+"time_seconds", nil, now))

	return samples, nil
}
