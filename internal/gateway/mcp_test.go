package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jonatan/portkeeper/internal/runbookmcp"
)

type agentTransport struct {
	base http.RoundTripper
}

func (t agentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("X-Agent-ID", "sdk-integration")
	return t.base.RoundTrip(out)
}

func TestMCPInteroperability(t *testing.T) {
	server := newTestGateway(t, runbookmcp.NewHandler())
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "portkeeper-integration", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL + "/demo/mcp",
		HTTPClient: &http.Client{
			Transport: agentTransport{base: server.Client().Transport},
		},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect MCP client through gateway: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v", err)
		}
	})

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "read_runbook" {
		t.Fatalf("unexpected tools: %+v", tools.Tools)
	}
	for _, tt := range []struct {
		topic, text string
		isError     bool
	}{
		{"gateway-routing", "# Gateway routing", false},
		{"rate-limiting", "# Rate limiting", false},
		{"missing", "unknown runbook", true},
		{"../../go.mod", "unknown runbook", true},
	} {
		t.Run(tt.topic, func(t *testing.T) {
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: "read_runbook", Arguments: map[string]any{"topic": tt.topic},
			})
			if err != nil {
				t.Fatalf("call tool: %v", err)
			}
			if result.IsError != tt.isError || len(result.Content) != 1 {
				t.Fatalf("unexpected tool result: %+v", result)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !strings.Contains(text.Text, tt.text) {
				t.Fatalf("unexpected tool content: %+v", result.Content)
			}
		})
	}
}
