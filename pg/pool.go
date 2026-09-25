package pg

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolOption configures a single connection pool.
//
// Every PoolOption is also a PoolsOption. What a PoolOption sets belongs to
// the connection rather than to a pool's role, so NewPools applies it to
// every pool it creates rather than to the main pool alone.
type PoolOption interface {
	PoolsOption

	applyPool(c *poolConf)
}

// poolConf is what a pool's PoolOptions add up to.
type poolConf struct {
	afterConnect func(context.Context, *pgx.Conn) error
}

type poolOptionFunc func(c *poolConf)

func (f poolOptionFunc) applyPool(c *poolConf) {
	f(c)
}

func (f poolOptionFunc) applyPools(o *poolsOptions) {
	o.pool = append(o.pool, f)
}

// WithAfterConnect runs fn on every connection the pool opens, before that
// connection is handed to anyone.
//
// It is the hook for per-connection state that a connection string cannot
// carry, and registering an extension's types is the case it exists for.
// pgvector's RegisterTypes reads the extension-assigned OIDs out of the
// database and registers the codecs on that connection's type map, and pgx
// v5 has no process-wide registry that would let it be done once instead.
// Without the hook a query that passes a vector fails when pgx looks for a
// codec, so a service that needs it and hasn't got it starts cleanly, pings
// cleanly, and then fails on the first vector it writes.
//
// fn runs on a connection that is not yet in use, so it may query the
// database. An error from it fails that connection rather than the pool, and
// the pool opens another on the next acquire — but NewPool pings, so a hook
// that fails deterministically fails at startup rather than quietly later.
func WithAfterConnect(fn func(context.Context, *pgx.Conn) error) PoolOption {
	return poolOptionFunc(func(c *poolConf) {
		c.afterConnect = fn
	})
}

// poolConfig assembles the pgxpool configuration of a single pool, so that
// what the options add up to can be inspected without opening one.
func poolConfig(
	connString string,
	maxConns int,
	opts []PoolOption,
) (*pgxpool.Config, error) {
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

	var pc poolConf

	for _, opt := range opts {
		opt.applyPool(&pc)
	}

	conf.AfterConnect = pc.afterConnect

	return conf, nil
}

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
// Anything the connection string cannot express is a PoolOption;
// WithAfterConnect is the one that registers an extension's types.
//
// The collector is unregistered again if the ping fails, so a failed call
// leaves nothing behind.
func NewPool(
	ctx context.Context,
	reg prometheus.Registerer,
	name string,
	connString string,
	maxConns int,
	opts ...PoolOption,
) (*pgxpool.Pool, error) {
	if reg == nil {
		return nil, errors.New("no metrics registerer provided")
	}

	conf, err := poolConfig(connString, maxConns, opts)
	if err != nil {
		return nil, err
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
