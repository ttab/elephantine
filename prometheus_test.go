package elephantine_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
)

func TestRegisterOrReuse(t *testing.T) {
	reg := prometheus.NewRegistry()

	newVec := func() *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_things_total",
			Help: "Things counted by the test.",
		}, []string{"name"})
	}

	first, err := elephantine.RegisterOrReuse(reg, newVec())
	if err != nil {
		t.Fatalf("register first collector: %v", err)
	}

	second, err := elephantine.RegisterOrReuse(reg, newVec())
	if err != nil {
		t.Fatalf("register duplicate collector: %v", err)
	}

	if first != second {
		t.Fatal("expected the second registration to reuse the first collector")
	}

	// A collector with the same name but a different type must be
	// rejected rather than silently reused.
	//nolint:promlinter // the _total suffix mismatch is deliberate here.
	_, err = elephantine.RegisterOrReuse(reg,
		prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_things_total",
			Help: "Things counted by the test.",
		}, []string{"name"}))
	if err == nil {
		t.Fatal("expected an error when reusing a mismatched collector type")
	}
}
