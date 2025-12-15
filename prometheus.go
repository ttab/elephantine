package elephantine

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

func NewMetricsHelper(reg prometheus.Registerer) *MetricsHelper {
	return &MetricsHelper{
		reg: reg,
	}
}

type MetricsHelper struct {
	reg prometheus.Registerer
	err error
}

func (h *MetricsHelper) Err() error {
	return h.err
}

func (h *MetricsHelper) Counter(
	o *prometheus.Counter,
	opts prometheus.CounterOpts,
) {
	if h.err != nil {
		return
	}

	c := prometheus.NewCounter(opts)

	err := h.reg.Register(c)
	if err != nil {
		h.err = fmt.Errorf("register %q counter: %w", opts.Name, err)

		return
	}

	*o = c
}

func (h *MetricsHelper) CounterVec(
	o **prometheus.CounterVec,
	opts prometheus.CounterOpts,
	labels []string,
) {
	if h.err != nil {
		return
	}

	c := prometheus.NewCounterVec(opts, labels)

	err := h.reg.Register(c)
	if err != nil {
		h.err = fmt.Errorf("register %q counter vector: %w", opts.Name, err)

		return
	}

	*o = c
}

func (h *MetricsHelper) Gauge(
	o *prometheus.Gauge,
	opts prometheus.GaugeOpts,
) {
	if h.err != nil {
		return
	}

	g := prometheus.NewGauge(opts)

	err := h.reg.Register(g)
	if err != nil {
		h.err = fmt.Errorf("register %q gauge: %w", opts.Name, err)

		return
	}

	*o = g
}

func (h *MetricsHelper) GaugeVec(
	o **prometheus.GaugeVec,
	opts prometheus.GaugeOpts,
	labels []string,
) {
	if h.err != nil {
		return
	}

	g := prometheus.NewGaugeVec(opts, labels)

	err := h.reg.Register(g)
	if err != nil {
		h.err = fmt.Errorf("register %q gauge vector: %w", opts.Name, err)

		return
	}

	*o = g
}

func (h *MetricsHelper) Histogram(
	o *prometheus.Histogram,
	opts prometheus.HistogramOpts,
) {
	if h.err != nil {
		return
	}

	hist := prometheus.NewHistogram(opts)

	err := h.reg.Register(hist)
	if err != nil {
		h.err = fmt.Errorf("register %q histogram: %w", opts.Name, err)

		return
	}

	*o = hist
}

func (h *MetricsHelper) HistogramVec(
	o **prometheus.HistogramVec,
	opts prometheus.HistogramOpts,
	labels []string,
) {
	if h.err != nil {
		return
	}

	hist := prometheus.NewHistogramVec(opts, labels)

	err := h.reg.Register(hist)
	if err != nil {
		h.err = fmt.Errorf("register %q histogram vector: %w", opts.Name, err)

		return
	}

	*o = hist
}
