package collectors

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type countingCollector struct {
	count atomic.Int64
}

func (c *countingCollector) Run(context.Context) error {
	c.count.Add(1)
	return nil
}

func TestManagerSkipsCollectorsWhenDisabled(t *testing.T) {
	collector := &countingCollector{}
	manager := &Manager{
		Scheduled: []ScheduledCollector{
			{Collector: collector, Interval: 5 * time.Millisecond},
		},
		JitterFraction: 0.01,
	}
	manager.SetEnabled(false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.Run(ctx)
	}()

	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done

	if got := collector.count.Load(); got != 0 {
		t.Fatalf("collector runs = %d, want 0", got)
	}
}

func TestManagerRunsCollectorsWhenReenabled(t *testing.T) {
	collector := &countingCollector{}
	manager := &Manager{
		Scheduled: []ScheduledCollector{
			{Collector: collector, Interval: 5 * time.Millisecond},
		},
		JitterFraction: 0.01,
	}
	manager.SetEnabled(false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.Run(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	if got := collector.count.Load(); got != 0 {
		cancel()
		<-done
		t.Fatalf("collector runs while disabled = %d, want 0", got)
	}

	manager.SetEnabled(true)
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) && collector.count.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := collector.count.Load(); got == 0 {
		t.Fatal("collector runs = 0, want at least one run after re-enable")
	}
}
