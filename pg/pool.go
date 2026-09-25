package pg

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// NewPool creates a connection pool, registers a PoolStatCollector for it on
// reg under the given name, and verifies that the database answers. The name
// is the collector's pool label: "main" for the primary pool, and the pool's
// role, such as "pubsub", for any other.
//
// A positive maxConns sizes the pool, overriding pool_max_conns in the
// connection string. Zero or less leaves that to the connection string, and
// failing that to pgx, whose default is max(4, NumCPU()) read from the node's
// cpuset rather than the container's CPU quota — so a service should size its
// pools explicitly.
//
// The collector is unregistered again if the ping fails, so a failed call
// leaves nothing behind.
func NewPool(
	ctx context.Context,
	reg prometheus.Registerer,
	name string,
	connString string,
	maxConns int,
) (*pgxpool.Pool, error) {
	if reg == nil {
		return nil, errors.New("no metrics registerer provided")
	}

	conf, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}

	if maxConns > math.MaxInt32 {
		return nil, fmt.Errorf("max conns %d exceeds %d", maxConns, math.MaxInt32)
	}

	if maxConns > 0 {
		conf.MaxConns = int32(maxConns)
	}

	// Creating the pool doesn't connect, so it's cheap to back out of.
	pool, err := pgxpool.NewWithConfig(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	collector := NewPoolStatCollector(pool, name)

	err = reg.Register(collector)
	if err != nil {
		pool.Close()

		return nil, fmt.Errorf("register %q pool metrics: %w", name, err)
	}

	err = pool.Ping(ctx)
	if err != nil {
		reg.Unregister(collector)
		pool.Close()

		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return pool, nil
}
