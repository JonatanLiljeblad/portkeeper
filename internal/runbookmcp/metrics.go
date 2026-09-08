package runbookmcp

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
)

type toolMetrics struct {
	invocations *prometheus.CounterVec
	duration    *prometheus.HistogramVec
}

func newToolMetrics(registerer prometheus.Registerer) *toolMetrics {
	m := &toolMetrics{
		invocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_backend_tool_invocations_total",
			Help: "Completed backend tool handler executions, excluding protocol validation failures.",
		}, []string{"tool", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mcp_backend_tool_duration_seconds",
			Help:    "Backend tool handler execution duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tool", "outcome"}),
	}
	registerer.MustRegister(m.invocations, m.duration)
	return m
}

func (m *toolMetrics) observe(tool string, handler mcp.ToolHandlerFor[readInput, any]) mcp.ToolHandlerFor[readInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input readInput) (result *mcp.CallToolResult, output any, err error) {
		start := time.Now()
		completed := false
		defer func() {
			outcome := "success"
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				outcome = "cancelled"
			} else if !completed || err != nil || (result != nil && result.IsError) {
				outcome = "error"
			}
			m.invocations.WithLabelValues(tool, outcome).Inc()
			m.duration.WithLabelValues(tool, outcome).Observe(time.Since(start).Seconds())
		}()
		result, output, err = handler(ctx, req, input)
		completed = true
		return result, output, err
	}
}
