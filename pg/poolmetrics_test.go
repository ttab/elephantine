package pg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephantine/pg"
)

func TestPoolStatCollector(t *testing.T) {
	// Pool creation is lazy, so no server needs to be listening for the
	// stat collection to work.
	pool, err := pgxpool.New(context.Background(),
		"postgres://user:pass@localhost:1/collector_test")
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	defer pool.Close()

	collector := pg.NewPoolStatCollector(pool, "main")

	problems, err := testutil.CollectAndLint(collector)
	if err != nil {
		t.Fatalf("lint collector: %v", err)
	}

	for _, p := range problems {
		t.Errorf("metric %q: %s", p.Metric, p.Text)
	}

	count := testutil.CollectAndCount(collector)
	if count != 13 {
		t.Fatalf("expected 13 metrics, got %d", count)
	}
}
