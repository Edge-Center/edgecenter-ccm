package edgecenter

import (
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

const subsystem = "edgecenter"

var (
	apiRequestsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem: subsystem,
			Name:      "api_requests_total",
			Help:      "Total number of Edgecenter API HTTP requests.",
		},
		[]string{"method", "path", "code"},
	)

	apiRequestDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem: subsystem,
			Name:      "api_request_duration_seconds",
			Help:      "Latency of Edgecenter API HTTP requests.",
			Buckets:   []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "path", "code"},
	)

	apiRequestErrorsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem: subsystem,
			Name:      "api_request_errors_total",
			Help:      "Total number of Edgecenter API request transport errors (no HTTP response).",
		},
		[]string{"method", "path", "reason"},
	)

	edgecenterOperationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem: subsystem,
			Name:      "operation_duration_seconds",
			Help:      "Latency of Edgecenter cloud-provider operations (create_volume, attach_disk, etc.).",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		},
		[]string{"operation"},
	)

	edgecenterOperationErrorsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem: subsystem,
			Name:      "operation_errors_total",
			Help:      "Total number of Edgecenter cloud-provider operation errors.",
		},
		[]string{"operation"},
	)
)

func RegisterMetrics() {
	register := func(r metrics.Registerable, name string) {
		if err := legacyregistry.Register(r); err != nil {
			klog.Warningf("unable to register metrics %s: %v", name, err)
		}
	}

	register(apiRequestsTotal, "apiRequestsTotal")
	register(apiRequestDuration, "apiRequestDuration")
	register(apiRequestErrorsTotal, "apiRequestErrorsTotal")
	register(edgecenterOperationDuration, "edgecenterOperationDuration")
	register(edgecenterOperationErrorsTotal, "edgecenterOperationErrorsTotal")

	apiRequestsTotal.WithLabelValues("GET", "/__init__", "0").Add(0)
	edgecenterOperationDuration.WithLabelValues("__init__").Observe(0)
}
