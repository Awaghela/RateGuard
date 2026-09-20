// Package metrics defines the Prometheus instruments RateGuard exposes.
//
// Labels are deliberately low-cardinality (no per-client labels): with
// thousands of API clients, a per-client label would blow up the time-series
// count. Per-client visibility belongs in logs or a top-K sketch.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// Decision outcomes recorded in rateguard_decisions_total{result=...}.
const (
	ResultAllowed     = "allowed"
	ResultDenied      = "denied"
	ResultErrorOpen   = "error_allowed" // Redis failed, FAIL_OPEN=true
	ResultErrorClosed = "error_denied"  // Redis failed, FAIL_OPEN=false
)

type Metrics struct {
	Registry *prometheus.Registry

	Decisions       *prometheus.CounterVec   // result
	DecisionLatency *prometheus.HistogramVec // result; policy lookup + Redis round-trip
	PolicyLookups   *prometheus.CounterVec   // source=cache|db|default|stale
	RedisErrors     prometheus.Counter
	HTTPRequests    *prometheus.CounterVec   // route, code
	HTTPLatency     *prometheus.HistogramVec // route
	TokensRequested prometheus.Counter
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Rate-limit checks live in the sub-millisecond to tens-of-ms range.
	buckets := []float64{.0002, .0005, .001, .002, .005, .01, .025, .05, .1, .25, .5, 1}

	m := &Metrics{
		Registry: reg,
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rateguard_decisions_total",
			Help: "Rate-limit decisions by outcome.",
		}, []string{"result"}),
		DecisionLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rateguard_decision_duration_seconds",
			Help:    "Time to reach a rate-limit decision (policy lookup + Redis script).",
			Buckets: buckets,
		}, []string{"result"}),
		PolicyLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rateguard_policy_lookups_total",
			Help: "Policy lookups by source (cache, db, default, stale).",
		}, []string{"source"}),
		RedisErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rateguard_redis_errors_total",
			Help: "Failed Redis limiter calls.",
		}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rateguard_http_requests_total",
			Help: "HTTP requests by route and status code.",
		}, []string{"route", "code"}),
		HTTPLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rateguard_http_request_duration_seconds",
			Help:    "HTTP handler latency by route.",
			Buckets: buckets,
		}, []string{"route"}),
		TokensRequested: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rateguard_tokens_requested_total",
			Help: "Sum of token cost across all check requests.",
		}),
	}
	reg.MustRegister(m.Decisions, m.DecisionLatency, m.PolicyLookups, m.RedisErrors,
		m.HTTPRequests, m.HTTPLatency, m.TokensRequested)

	// Pre-create series so dashboards show 0 rather than "no data".
	for _, r := range []string{ResultAllowed, ResultDenied, ResultErrorOpen, ResultErrorClosed} {
		m.Decisions.WithLabelValues(r)
	}
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
