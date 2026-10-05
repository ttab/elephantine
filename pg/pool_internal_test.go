package pg

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5"
)

const configTestConnString = "postgres://user:pass@localhost:1/pool_test"

func TestPoolConfigAfterConnect(t *testing.T) {
	sentinel := errors.New("ran")

	conf, err := poolConfig(configTestConnString, 4, []PoolOption{
		WithAfterConnect(func(_ context.Context, _ *pgx.Conn) error {
			return sentinel
		}),
	})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	if conf.MaxConns != 4 {
		t.Fatalf("expected max conns 4, got %d", conf.MaxConns)
	}

	if conf.AfterConnect == nil {
		t.Fatal("expected AfterConnect to be set")
	}

	// The hook has to be the one handed in, not merely non-nil.
	if err := conf.AfterConnect(t.Context(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("expected the registered hook to run, got %v", err)
	}
}

func TestPoolConfigWithoutOptions(t *testing.T) {
	conf, err := poolConfig(configTestConnString, 0, nil)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	// pgx checks AfterConnect for nil, so leaving it unset is what keeps a
	// pool without the option on pgx's own path.
	if conf.AfterConnect != nil {
		t.Fatal("expected AfterConnect to be unset")
	}

	// Zero leaves the size to the connection string and then to pgx, whose
	// default is the max(4, NumCPU()) that NewPool's documentation warns
	// about: it is read from the node, not from the container's CPU quota.
	want := int32(max(4, runtime.NumCPU()))
	if conf.MaxConns != want {
		t.Fatalf("expected pgx's own default of %d, got %d",
			want, conf.MaxConns)
	}
}

func TestPoolOptionWidensToPoolsOption(t *testing.T) {
	var o poolsOptions

	// The point of the interface: a PoolOption can be passed where NewPools
	// wants a PoolsOption, and is collected for every pool it creates.
	var opt PoolsOption = WithAfterConnect(
		func(_ context.Context, _ *pgx.Conn) error { return nil })

	opt.applyPools(&o)

	if len(o.pool) != 1 {
		t.Fatalf("expected one pool option collected, got %d", len(o.pool))
	}

	var pc poolConf

	o.pool[0].applyPool(&pc)

	if pc.afterConnect == nil {
		t.Fatal("expected the collected option to set afterConnect")
	}
}

func TestPoolsOptionsKeepPoolOptionsSeparate(t *testing.T) {
	var o poolsOptions

	for _, opt := range []PoolsOption{
		WithBouncer("postgres://bouncer/db"),
		WithPubSub(),
		WithAfterConnect(func(_ context.Context, _ *pgx.Conn) error { return nil }),
	} {
		opt.applyPools(&o)
	}

	if o.bouncerConnString != "postgres://bouncer/db" {
		t.Fatalf("unexpected bouncer conn string %q", o.bouncerConnString)
	}

	if !o.pubsub {
		t.Fatal("expected pubsub to be asked for")
	}

	if len(o.pool) != 1 {
		t.Fatalf("expected one pool option, got %d", len(o.pool))
	}
}
