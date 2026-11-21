// Package metrics defines the Prometheus metrics exported on /metrics.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	ChainHeight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "settler_chain_block",
		Help: "Block height reported by the node, by assurance level (trusted, virtual, verified).",
	}, []string{"level"})

	IndexedBlock = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "settler_indexed_block",
		Help: "Highest block the settler has stored.",
	})

	PaymentsIndexed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "settler_payments_indexed_total",
		Help: "InvoicePaid events written to the database.",
	})

	PaymentsConfirmed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "settler_payments_confirmed_total",
		Help: "Payments that reached the assurance level their amount requires, by that level.",
	}, []string{"level"})

	Reorgs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "settler_reorgs_total",
		Help: "Chain reorganisations detected.",
	})

	ReorgDepth = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "settler_reorg_depth_blocks",
		Help:    "Number of blocks rolled back per reorg.",
		Buckets: []float64{1, 2, 3, 5, 10, 20, 50, 100},
	})

	OrphanedPayments = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "settler_orphaned_payments_total",
		Help: "Payments removed because their block was reorged out.",
	})

	RPCErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "settler_rpc_errors_total",
		Help: "Failed JSON-RPC calls, by method.",
	}, []string{"method"})

	RPCDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "settler_rpc_duration_seconds",
		Help:    "JSON-RPC call latency, by method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	OutboxPublished = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "settler_outbox_published_total",
		Help: "Events delivered from the outbox.",
	})
)

// Register adds every metric to the given registry.
func Register(r prometheus.Registerer) {
	r.MustRegister(ChainHeight, IndexedBlock, PaymentsIndexed, PaymentsConfirmed, Reorgs, ReorgDepth,
		OrphanedPayments, RPCErrors, RPCDuration, OutboxPublished)
}
