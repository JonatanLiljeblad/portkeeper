package runbookmcp

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed runbooks/*.md
var runbooks embed.FS

type readInput struct {
	Topic string `json:"topic" jsonschema:"Runbook topic: gateway-routing or rate-limiting"`
}

// NewHandler serves a small, embedded documentation collection over MCP.
// It cannot read arbitrary host files or execute the commands in the runbooks.
func NewHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "portkeeper-runbooks", Version: "0.1.0",
	}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_runbook",
		Description: "Read a Portkeeper operational runbook. Topics: gateway-routing, rate-limiting.",
	}, readRunbook)

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout:               5 * time.Minute,
		PropagateRequestCancellation: true,
	})
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
