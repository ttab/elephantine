package pg_test

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine/pg"
)

// TestNewJobLockSharedMetrics verifies that multiple job locks can register
// their metrics against the same registerer, both for different lock names
// and for re-created locks with the same name.
func TestNewJobLockSharedMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	logger := slog.Default()

	opts := pg.JobLockOptions{
		MetricsRegisterer: reg,
	}

	_, err := pg.NewJobLock(nil, logger, "archiver", opts)
	if err != nil {
		t.Fatalf("create first job lock: %v", err)
	}

	_, err = pg.NewJobLock(nil, logger, "scheduler", opts)
	if err != nil {
		t.Fatalf("create job lock with other name: %v", err)
	}

	_, err = pg.NewJobLock(nil, logger, "archiver", opts)
	if err != nil {
		t.Fatalf("re-create job lock with the same name: %v", err)
	}
}
