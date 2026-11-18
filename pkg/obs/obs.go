// Package obs: Prometheus metrics + structured logging helpers.
package obs

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	OpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "snapvol_op_duration_seconds",
		Help:    "Operation latency by operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"op"})
	OpErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "snapvol_op_errors_total",
		Help: "Operation errors by operation.",
	}, []string{"op"})
	Volumes = promauto.NewGauge(prometheus.GaugeOpts{Name: "snapvol_volumes", Help: "Current volumes."})
)

// Observe records latency/error for op; call as `defer obs.Observe("CreateVolume", &err)()`.
func Observe(op string, errp *error) func() {
	start := time.Now()
	return func() {
		OpDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
		if errp != nil && *errp != nil {
			OpErrors.WithLabelValues(op).Inc()
		}
	}
}
