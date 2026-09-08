package runbookmcp

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	ReadTool   = "read_runbook"
	StreamTool = "stream_runbook"
)

//go:embed runbooks/*.md
var runbooks embed.FS

type readInput struct {
	Topic string `json:"topic" jsonschema:"Runbook topic: gateway-routing or rate-limiting"`
}

// NewHandler serves a small, embedded documentation collection over MCP.
// It cannot read arbitrary host files or execute the commands in the runbooks.
func NewHandler() http.Handler {
	return NewObservedHandler(prometheus.NewRegistry())
}

// NewObservedHandler registers execution metrics with the supplied registry.
// Only entry into a typed tool handler counts as an execution, not SDK validation.
// Use a fresh registry for each handler, or the default registry once at startup.
func NewObservedHandler(registerer prometheus.Registerer) http.Handler {
	metrics := newToolMetrics(registerer)
	server := mcp.NewServer(&mcp.Implementation{
		Name: "portkeeper-runbooks", Version: "0.1.0",
	}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        ReadTool,
		Description: "Read a Portkeeper operational runbook. Topics: gateway-routing, rate-limiting.",
	}, metrics.observe(ReadTool, readRunbook))
	mcp.AddTool(server, &mcp.Tool{
		Name:        StreamTool,
		Description: "Read an embedded runbook with three progress notifications before completion. Topics: gateway-routing, rate-limiting.",
	}, metrics.observe(StreamTool, streamRunbook))

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout:               5 * time.Minute,
		PropagateRequestCancellation: true,
	})
}

func streamRunbook(ctx context.Context, req *mcp.CallToolRequest, input readInput) (*mcp.CallToolResult, any, error) {
	result, output, err := readRunbook(ctx, req, input)
	if err != nil {
		return nil, nil, err
	}
	for step := 1; step <= 3; step++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if token := req.Params.GetProgressToken(); token != nil {
			if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token, Progress: float64(step), Total: 3,
				Message: fmt.Sprintf("Runbook progress %d/3", step),
			}); err != nil {
				return nil, nil, err
			}
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
	return result, output, nil
}

func readRunbook(_ context.Context, _ *mcp.CallToolRequest, input readInput) (*mcp.CallToolResult, any, error) {
	switch input.Topic {
	case "gateway-routing", "rate-limiting":
	default:
		return nil, nil, fmt.Errorf("unknown runbook %q; choose gateway-routing or rate-limiting", input.Topic)
	}

	content, err := runbooks.ReadFile("runbooks/" + input.Topic + ".md")
	if err != nil {
		return nil, nil, fmt.Errorf("read runbook: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(content)}},
	}, nil, nil
}
