package pg

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolStatCollector exposes the pgxpool.Stat statistics of a connection pool
// as Prometheus metrics. Register one collector per pool, using the name to
// tell pools apart if the application uses more than one.
//
// The connection gauges answer whether the pool is exhausted (acquired at
// max_conns), and the empty acquire counters tell you how often, and for how
// long, clients have had to wait for a connection.
type PoolStatCollector struct {
	pool *pgxpool.Pool

	totalConns        *prometheus.Desc
	idleConns         *prometheus.Desc
	acquiredConns     *prometheus.Desc
	constructingConns *prometheus.Desc
	maxConns          *prometheus.Desc

	acquires            *prometheus.Desc
	acquireDuration     *prometheus.Desc
	emptyAcquires       *prometheus.Desc
	emptyAcquireWait    *prometheus.Desc
	canceledAcquires    *prometheus.Desc
	newConns            *prometheus.Desc
	maxLifetimeDestroys *prometheus.Desc
	maxIdleDestroys     *prometheus.Desc
}

// NewPoolStatCollector creates a Prometheus collector that reports connection
// pool statistics for the given pool.
func NewPoolStatCollector(
	pool *pgxpool.Pool, name string,
) *PoolStatCollector {
	labels := prometheus.Labels{"pool": name}

	return &PoolStatCollector{
		pool: pool,
		totalConns: prometheus.NewDesc(
			"pgxpool_total_conns",
			"Current number of connections in the pool.",
			nil, labels),
		idleConns: prometheus.NewDesc(
			"pgxpool_idle_conns",
			"Current number of idle connections in the pool.",
			nil, labels),
		acquiredConns: prometheus.NewDesc(
			"pgxpool_acquired_conns",
			"Current number of acquired connections.",
			nil, labels),
		constructingConns: prometheus.NewDesc(
			"pgxpool_constructing_conns",
			"Current number of connections being constructed.",
			nil, labels),
		maxConns: prometheus.NewDesc(
			"pgxpool_max_conns",
			"Maximum size of the pool.",
			nil, labels),
		acquires: prometheus.NewDesc(
			"pgxpool_acquires_total",
			"Cumulative count of successful connection acquires.",
			nil, labels),
		acquireDuration: prometheus.NewDesc(
			"pgxpool_acquire_duration_seconds_total",
			"Total time blocked waiting for successful "+
				"connection acquires.",
			nil, labels),
		emptyAcquires: prometheus.NewDesc(
			"pgxpool_empty_acquires_total",
			"Cumulative count of acquires that waited because "+
				"the pool was empty.",
			nil, labels),
		emptyAcquireWait: prometheus.NewDesc(
			"pgxpool_empty_acquire_wait_seconds_total",
			"Total time waited for a connection because the "+
				"pool was empty.",
			nil, labels),
		canceledAcquires: prometheus.NewDesc(
			"pgxpool_canceled_acquires_total",
			"Cumulative count of acquires cancelled by the "+
				"caller, typically because a context timed "+
				"out while waiting for a connection.",
			nil, labels),
		newConns: prometheus.NewDesc(
			"pgxpool_new_conns_total",
			"Cumulative count of new connections opened.",
			nil, labels),
		maxLifetimeDestroys: prometheus.NewDesc(
			"pgxpool_max_lifetime_destroys_total",
			"Cumulative count of connections destroyed because "+
				"they exceeded the maximum lifetime.",
			nil, labels),
		maxIdleDestroys: prometheus.NewDesc(
			"pgxpool_max_idle_destroys_total",
			"Cumulative count of connections destroyed because "+
				"they exceeded the maximum idle time.",
			nil, labels),
	}
}

// Describe implements prometheus.Collector.
func (c *PoolStatCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

// Collect implements prometheus.Collector.
func (c *PoolStatCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()

	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(
			d, prometheus.GaugeValue, v)
	}

	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(
			d, prometheus.CounterValue, v)
	}

	gauge(c.totalConns, float64(s.TotalConns()))
	gauge(c.idleConns, float64(s.IdleConns()))
	gauge(c.acquiredConns, float64(s.AcquiredConns()))
	gauge(c.constructingConns, float64(s.ConstructingConns()))
	gauge(c.maxConns, float64(s.MaxConns()))

	counter(c.acquires, float64(s.AcquireCount()))
	counter(c.acquireDuration, s.AcquireDuration().Seconds())
	counter(c.emptyAcquires, float64(s.EmptyAcquireCount()))
	counter(c.emptyAcquireWait, s.EmptyAcquireWaitTime().Seconds())
	counter(c.canceledAcquires, float64(s.CanceledAcquireCount()))
	counter(c.newConns, float64(s.NewConnsCount()))
	counter(c.maxLifetimeDestroys, float64(s.MaxLifetimeDestroyCount()))
	counter(c.maxIdleDestroys, float64(s.MaxIdleDestroyCount()))
}
