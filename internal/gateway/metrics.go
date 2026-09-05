package gateway

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	toolCallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_gateway_tool_calls_total",
			Help: "Total tool calls proxied through the gateway, by server, tool, and status.",
		},
		[]string{"namespace", "server", "tool", "status"},
	)

	toolCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mcp_gateway_tool_call_duration_seconds",
			Help:    "Latency of tool calls proxied through the gateway.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "server", "tool"},
	)

	rateLimitedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_gateway_rate_limited_total",
			Help: "Requests rejected by the per-agent rate limiter, by agent.",
		},
		[]string{"agent"},
	)
)

func init() {
	prometheus.MustRegister(toolCallsTotal, toolCallDuration, rateLimitedTotal)
}

func recordCall(namespace, server, tool string, status int, elapsed time.Duration) {
	statusLabel := strconv.Itoa(status)
	toolCallsTotal.WithLabelValues(namespace, server, tool, statusLabel).Inc()
	toolCallDuration.WithLabelValues(namespace, server, tool).Observe(elapsed.Seconds())
}
