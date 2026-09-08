package runbookmcp

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
)

func executionCount(t *testing.T, registry *prometheus.Registry, name, tool, outcome string) uint64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if len(labels) != 2 {
				t.Fatalf("unexpected labels: %v", labels)
			}
			if labels["tool"] == tool && labels["outcome"] == outcome {
				if metric.Counter != nil {
					return uint64(metric.Counter.GetValue())
				}
				return metric.Histogram.GetSampleCount()
			}
		}
	}
	return 0
}

func TestObservedHandlerExecutionsAndEarlyStreaming(t *testing.T) {
	registry := prometheus.NewRegistry()
	server := httptest.NewServer(NewObservedHandler(registry))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var progressTimes []time.Time
	var progress []float64
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			if req.Params.ProgressToken != "stream-test" {
				t.Error("progress token was not preserved")
			}
			progressTimes = append(progressTimes, time.Now())
			progress = append(progress, req.Params.Progress)
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("expected exactly two tools: %v, %v", tools, err)
	}
	if n := executionCount(t, registry, "mcp_backend_tool_invocations_total", ReadTool, "success"); n != 0 {
		t.Fatal("initialization and discovery counted as tool execution")
	}
	for _, args := range []any{map[string]any{}, map[string]any{"topic": 17}} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ReadTool, Arguments: args})
		if err == nil && !result.IsError {
			t.Fatal("schema-invalid arguments were accepted")
		}
	}
	if n := executionCount(t, registry, "mcp_backend_tool_invocations_total", ReadTool, "error"); n != 0 {
		t.Fatal("schema validation counted as handler execution")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: ReadTool, Arguments: map[string]any{"topic": "gateway-routing"}})
	if err != nil || result.IsError {
		t.Fatalf("read: %v, %v", result, err)
	}
	for _, tool := range []string{ReadTool, StreamTool} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"topic": "../../etc/passwd"}})
		if err != nil || !result.IsError {
			t.Fatalf("allowlist rejection: %v, %v", result, err)
		}
	}
	params := &mcp.CallToolParams{Name: StreamTool, Arguments: map[string]any{"topic": "gateway-routing"}}
	params.SetProgressToken("stream-test")
	streamed, err := session.CallTool(ctx, params)
	completed := time.Now()
	if err != nil || streamed.IsError {
		t.Fatalf("stream: %v, %v", streamed, err)
	}
	if streamed.Content[0].(*mcp.TextContent).Text != result.Content[0].(*mcp.TextContent).Text {
		t.Fatal("streaming changed the runbook content")
	}
	mu.Lock()
	if len(progressTimes) != 3 || completed.Sub(progressTimes[0]) < 100*time.Millisecond ||
		progress[0] != 1 || progress[1] != 2 || progress[2] != 3 {
		t.Errorf("expected three progress notifications well before completion: %v, %v", progressTimes, progress)
	}
	mu.Unlock()
	for _, metric := range []string{"mcp_backend_tool_invocations_total", "mcp_backend_tool_duration_seconds"} {
		for _, tool := range []string{ReadTool, StreamTool} {
			for _, outcome := range []string{"success", "error"} {
				if n := executionCount(t, registry, metric, tool, outcome); n != 1 {
					t.Errorf("%s{%s,%s} = %d, want 1", metric, tool, outcome, n)
				}
			}
		}
	}
}

func TestStreamingCancellationRecordedOnce(t *testing.T) {
	registry := prometheus.NewRegistry()
	server := httptest.NewServer(NewObservedHandler(registry))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) { cancelCall() },
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	params := &mcp.CallToolParams{Name: StreamTool, Arguments: map[string]any{"topic": "gateway-routing"}}
	params.SetProgressToken("cancel")
	if _, err := session.CallTool(callCtx, params); err == nil {
		t.Fatal("cancelled call succeeded")
	}
	for executionCount(t, registry, "mcp_backend_tool_invocations_total", StreamTool, "cancelled") == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("backend did not observe cancellation")
		case <-time.After(time.Millisecond):
		}
	}
	for _, metric := range []string{"mcp_backend_tool_invocations_total", "mcp_backend_tool_duration_seconds"} {
		if n := executionCount(t, registry, metric, StreamTool, "cancelled"); n != 1 {
			t.Errorf("%s cancelled = %d, want 1", metric, n)
		}
		if n := executionCount(t, registry, metric, StreamTool, "success"); n != 0 {
			t.Errorf("%s counted cancellation twice", metric)
		}
	}
}

func TestMetricOutcomeClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		result *mcp.CallToolResult
		err    error
		want   string
	}{
		{"success", &mcp.CallToolResult{}, nil, "success"},
		{"error", nil, errors.New("failure"), "error"},
		{"result_error", &mcp.CallToolResult{IsError: true}, nil, "error"},
		{"cancel", nil, context.Canceled, "cancelled"},
		{"deadline", nil, context.DeadlineExceeded, "cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			handler := newToolMetrics(registry).observe(ReadTool, func(context.Context, *mcp.CallToolRequest, readInput) (*mcp.CallToolResult, any, error) {
				return test.result, nil, test.err
			})
			handler(t.Context(), nil, readInput{})
			for _, metric := range []string{"mcp_backend_tool_invocations_total", "mcp_backend_tool_duration_seconds"} {
				if n := executionCount(t, registry, metric, ReadTool, test.want); n != 1 {
					t.Errorf("%s count = %d", metric, n)
				}
			}
		})
	}
}

func TestMetricPanicIsNotSuccess(t *testing.T) {
	registry := prometheus.NewRegistry()
	handler := newToolMetrics(registry).observe(ReadTool, func(context.Context, *mcp.CallToolRequest, readInput) (*mcp.CallToolResult, any, error) {
		panic("handler failure")
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("metrics wrapper swallowed panic")
			}
		}()
		handler(t.Context(), nil, readInput{})
	}()
	for _, metric := range []string{"mcp_backend_tool_invocations_total", "mcp_backend_tool_duration_seconds"} {
		if executionCount(t, registry, metric, ReadTool, "error") != 1 ||
			executionCount(t, registry, metric, ReadTool, "success") != 0 {
			t.Errorf("%s incorrectly counted a panicking handler", metric)
		}
	}
}
