package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// PoolNameMain is the pool label of the pool queries run on.
	PoolNameMain = "main"
	// PoolNamePubSub is the pool label of the direct pool kept for
	// LISTEN behind a bouncer.
	PoolNamePubSub = "pubsub"
)

// DefaultPubSubMaxConns is the size of the pubsub pool when queries go through
// a bouncer: it then carries only the LISTEN session, which Subscribe hijacks
// out of the pool for the life of the process, plus one spare.
const DefaultPubSubMaxConns = 2

// Pools are the connection pools of a service, created by NewPools.
type Pools struct {
	// Main is the pool queries run on: the bouncer pool when a bouncer is
	// configured, otherwise the direct pool.
	Main *pgxpool.Pool
	// PubSub is a direct pool for LISTEN, which doesn't survive
	// transaction pooling. It is only created when asked for with
	// WithPubSub, and is the same pool as Main when no bouncer is
	// configured. Nil otherwise.
	PubSub *pgxpool.Pool
}

// Close closes the pools, each one once.
func (p *Pools) Close() {
	p.Main.Close()

	if p.PubSub != nil && p.PubSub != p.Main {
		p.PubSub.Close()
	}
}

// PoolsOption configures NewPools. Only this package implements it, which is
// what lets a PoolOption widen to one.
type PoolsOption interface {
	applyPools(o *poolsOptions)
}

type poolsOptions struct {
	bouncerConnString string
	pubsub            bool
	pool              []PoolOption
}

type poolsOptionFunc func(o *poolsOptions)

func (f poolsOptionFunc) applyPools(o *poolsOptions) {
	f(o)
}

// WithBouncer routes queries through a transaction pooler such as PgBouncer.
// An empty connection string, or one equal to the direct connection string,
// leaves the service on its direct pool, so a service can pass its bouncer
// setting through whether or not it is set.
func WithBouncer(connString string) PoolsOption {
	return poolsOptionFunc(func(o *poolsOptions) {
		o.bouncerConnString = connString
	})
}

// WithPubSub asks for a pool that can LISTEN, for applications that use
// Subscribe. Behind a bouncer that is a direct pool of its own, sized
// DefaultPubSubMaxConns; without one it is the main pool.
func WithPubSub() PoolsOption {
	return poolsOptionFunc(func(o *poolsOptions) {
		o.pubsub = true
	})
}

// NewPools creates the connection pools of a service with NewPool, so that
// every pool is pinged and has its PoolStatCollector registered on reg.
//
// The main pool is created on the bouncer connection string when WithBouncer
// has one, otherwise on connString, and maxConns sizes it as it does for
// NewPool. A pubsub pool is only created with WithPubSub and a bouncer; its
// size is fixed at DefaultPubSubMaxConns, and it doesn't count against
// maxConns. Without a bouncer the main pool serves as the pubsub pool, and is
// registered once, as "main".
//
// A PoolOption such as WithAfterConnect applies to every pool created, since
// what it sets belongs to the connection rather than to a pool's role: a type
// registration the main pool needs is one the pubsub pool needs too.
func NewPools(
	ctx context.Context,
	reg prometheus.Registerer,
	connString string,
	maxConns int,
	opts ...PoolsOption,
) (*Pools, error) {
	var o poolsOptions

	for _, opt := range opts {
		opt.applyPools(&o)
	}

	plan := planPools(connString, maxConns, o)

	main, err := NewPool(ctx, reg,
		PoolNameMain, plan.main.connString, plan.main.maxConns, o.pool...)
	if err != nil {
		return nil, fmt.Errorf("create %s pool: %w", PoolNameMain, err)
	}

	pools := Pools{Main: main}

	switch {
	case plan.pubsub != nil:
		pubsub, err := NewPool(ctx, reg,
			PoolNamePubSub, plan.pubsub.connString, plan.pubsub.maxConns,
			o.pool...)
		if err != nil {
			// A new collector for the same pool and name has the
			// same descriptors, which is what Unregister matches on.
			reg.Unregister(NewPoolStatCollector(main, PoolNameMain))
			main.Close()

			return nil, fmt.Errorf("create %s pool: %w", PoolNamePubSub, err)
		}

		pools.PubSub = pubsub
	case o.pubsub:
		pools.PubSub = main
	}

	return &pools, nil
}

type poolSpec struct {
	connString string
	maxConns   int
}

type poolPlan struct {
	main   poolSpec
	pubsub *poolSpec
}

// planPools decides which pools NewPools creates. A nil pubsub spec means that
// no separate pubsub pool is created.
func planPools(connString string, maxConns int, o poolsOptions) poolPlan {
	bouncer := o.bouncerConnString
	if bouncer == "" || bouncer == connString {
		return poolPlan{
			main: poolSpec{connString: connString, maxConns: maxConns},
		}
	}

	plan := poolPlan{
		main: poolSpec{connString: bouncer, maxConns: maxConns},
	}

	if o.pubsub {
		plan.pubsub = &poolSpec{
			connString: connString,
			maxConns:   DefaultPubSubMaxConns,
		}
	}

	return plan
}
