package elephantine_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
)

func TestIdleConnections(t *testing.T) {
	client := elephantine.NewHTTPClient(10*time.Second,
		elephantine.MaxConnectionsPerHost(64),
		elephantine.IdleConnections(48, 24, 30*time.Second),
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.Transport)
	}

	if transport.MaxIdleConns != 48 {
		t.Errorf("MaxIdleConns = %d, want 48", transport.MaxIdleConns)
	}

	if transport.MaxIdleConnsPerHost != 24 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 24",
			transport.MaxIdleConnsPerHost)
	}

	if transport.IdleConnTimeout != 30*time.Second {
		t.Errorf("IdleConnTimeout = %s, want 30s", transport.IdleConnTimeout)
	}

	// The idle pool limits must leave the open-connection cap alone.
	if transport.MaxConnsPerHost != 64 {
		t.Errorf("MaxConnsPerHost = %d, want 64", transport.MaxConnsPerHost)
	}
}

func TestNewHTTPClientInstrumentationAlias(t *testing.T) {
	spelled, err := elephantine.NewHTTPClientInstrumentation(
		prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewHTTPClientInstrumentation: %v", err)
	}

	misspelled, err := elephantine.NewHTTPClientIntrumentation(
		prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewHTTPClientIntrumentation: %v", err)
	}

	if spelled == nil || misspelled == nil {
		t.Fatal("expected both constructors to return an instrumentation")
	}
}
