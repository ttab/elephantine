package pg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephantine/pg"
)

// Nothing listens on port 1, so pings against it fail fast.
const unreachableConnString = "postgres://user:pass@localhost:1/pool_test"

func TestNewPoolFailedPingUnregisters(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	_, err := pg.NewPool(t.Context(), reg, "main", unreachableConnString, 2)
	if err == nil {
		t.Fatal("expected an error from an unreachable database")
	}

	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	if count != 0 {
		t.Fatalf("expected no metrics after a failed ping, got %d", count)
	}

	// The name is free again, so a retry can register it.
	placeholder, err := pgxpool.New(context.Background(), unreachableConnString)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	defer placeholder.Close()

	err = reg.Register(pg.NewPoolStatCollector(placeholder, "main"))
	if err != nil {
		t.Fatalf("register after failed NewPool: %v", err)
	}
}

func TestNewPoolDuplicateName(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	existing, err := pgxpool.New(context.Background(), unreachableConnString)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	defer existing.Close()

	err = reg.Register(pg.NewPoolStatCollector(existing, "main"))
	if err != nil {
		t.Fatalf("register existing pool: %v", err)
	}

	_, err = pg.NewPool(t.Context(), reg, "main", unreachableConnString, 2)
	if err == nil {
		t.Fatal("expected an error for a pool name already registered")
	}
}

func TestNewPoolRejectsBadArguments(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	cases := map[string]struct {
		reg        prometheus.Registerer
		connString string
		maxConns   int
	}{
		"nil registerer": {
			connString: unreachableConnString,
		},
		"bad connection string": {
			reg:        reg,
			connString: "postgres://%zz",
		},
		"max conns out of range": {
			reg:        reg,
			connString: unreachableConnString,
			maxConns:   1 << 31,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pg.NewPool(t.Context(), c.reg, "main",
				c.connString, c.maxConns)
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewPoolsFailureRegistersNothing(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	_, err := pg.NewPools(t.Context(), reg, unreachableConnString, 8,
		pg.WithBouncer("postgres://user:pass@localhost:2/pool_test"),
		pg.WithPubSub())
	if err == nil {
		t.Fatal("expected an error from an unreachable database")
	}

	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	if count != 0 {
		t.Fatalf("expected no metrics after a failed NewPools, got %d", count)
	}
}
