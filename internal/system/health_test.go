package system

import (
	"context"
	"net"
	"testing"
)

func TestStartHealthServerReturnsListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve test listener: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Logf("listener close failed: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdown, err := StartHealthServer(ctx, listener.Addr().String())
	if err == nil {
		_ = shutdown(context.Background())
		t.Fatal("StartHealthServer() error = nil, want listen error")
	}
}

func TestSanitizeLabelEscapesPrometheusLabelCharacters(t *testing.T) {
	got := sanitizeLabel("gpu\"0\\slot\nrack\r")
	want := "gpu\\\"0\\\\slot\\nrack\\r"
	if got != want {
		t.Fatalf("sanitizeLabel() = %q, want %q", got, want)
	}
}
