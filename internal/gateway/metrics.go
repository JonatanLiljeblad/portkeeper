package gateway

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_gateway_http_requests_total",
			Help: "Finished routed HTTP requests, including rejections and aborted streams; not MCP tool executions.",
		},
		[]string{"namespace", "server", "endpoint", "status", "result"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mcp_gateway_http_request_duration_seconds",
			Help:    "Routed HTTP request lifetime including authentication and full stream duration; not per-tool latency.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "server", "endpoint"},
	)

	httpRequestsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mcp_gateway_http_requests_in_flight",
		Help: "Active routed HTTP handlers, including authentication and open streams.",
	})

	rateLimitedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_gateway_rate_limited_total",
			Help: "Requests rejected by the per-agent rate limiter; excess distinct agent labels aggregate under _overflow.",
		},
		[]string{"agent"},
	)
	serverMetricLabels = boundedLabels{limit: 256, values: make(map[string]struct{})}
	agentMetricLabels  = boundedLabels{limit: 128, values: make(map[string]struct{})}
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration, httpRequestsInFlight, rateLimitedTotal)
}

// Label admissions are permanent for the process lifetime. Eviction would only
// bound this map, not the time series already allocated by CounterVec/HistogramVec.
type boundedLabels struct {
	mu     sync.Mutex
	limit  int
	values map[string]struct{}
}

func (b *boundedLabels) admit(value string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.values[value]; ok {
		return true
	}
	if len(b.values) >= b.limit {
		return false
	}
	b.values[value] = struct{}{}
	return true
}

func observedServer(b Backend) (string, string) {
	if !serverMetricLabels.admit(b.Namespace + "/" + b.Name) {
		return "_overflow", "_overflow"
	}
	return b.Namespace, b.Name
}

func observedEndpoint(endpoint string) string {
	if endpoint == "mcp" {
		return "mcp"
	}
	return "other"
}

func recordRateLimit(agent string) {
	if !agentMetricLabels.admit(agent) {
		agent = "_overflow"
	}
	rateLimitedTotal.WithLabelValues(agent).Inc()
}

func recordRequest(namespace, server, endpoint string, status int, completed bool, elapsed time.Duration) {
	statusLabel := strconv.Itoa(status)
	switch status {
	case 200, 201, 202, 204, 400, 401, 403, 404, 405, 409, 429, 500, 502, 503, 504:
	default:
		statusLabel = "other"
		if status >= 100 && status <= 599 {
			statusLabel = strconv.Itoa(status/100) + "xx"
		}
	}
	result := "complete"
	if !completed {
		result = "aborted"
	}
	httpRequestsTotal.WithLabelValues(namespace, server, endpoint, statusLabel, result).Inc()
	httpRequestDuration.WithLabelValues(namespace, server, endpoint).Observe(elapsed.Seconds())
}
