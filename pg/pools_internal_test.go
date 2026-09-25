package pg

import (
	"reflect"
	"testing"
)

func TestPlanPools(t *testing.T) {
	const (
		direct  = "postgres://direct/db"
		bouncer = "postgres://bouncer/db"
	)

	cases := map[string]struct {
		opts poolsOptions
		want poolPlan
	}{
		"direct only": {
			want: poolPlan{main: poolSpec{direct, 8}},
		},
		"direct with pubsub shares the main pool": {
			opts: poolsOptions{pubsub: true},
			want: poolPlan{main: poolSpec{direct, 8}},
		},
		"bouncer without pubsub skips the direct pool": {
			opts: poolsOptions{bouncerConnString: bouncer},
			want: poolPlan{main: poolSpec{bouncer, 8}},
		},
		"bouncer with pubsub": {
			opts: poolsOptions{bouncerConnString: bouncer, pubsub: true},
			want: poolPlan{
				main:   poolSpec{bouncer, 8},
				pubsub: &poolSpec{direct, DefaultPubSubMaxConns},
			},
		},
		"bouncer equal to direct is no bouncer": {
			opts: poolsOptions{bouncerConnString: direct, pubsub: true},
			want: poolPlan{main: poolSpec{direct, 8}},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := planPools(direct, 8, c.opts)

			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("expected %+v, got %+v", c.want, got)
			}
		})
	}
}
