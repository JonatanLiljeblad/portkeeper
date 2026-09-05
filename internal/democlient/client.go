package democlient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type agentTransport struct {
	agentID string
}

func (t agentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("X-Agent-ID", t.agentID)
	return http.DefaultTransport.RoundTrip(out)
}

// Run discovers and calls the read-only runbook tool through an MCP endpoint.
// It makes one tool call, without application-level retries.
func Run(ctx context.Context, endpoint, agentID, topic string, output io.Writer) (err error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("endpoint must be an absolute HTTP or HTTPS URL")
	}
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("agent ID is required")
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "portkeeper-demo", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: agentTransport{agentID: agentID}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return fmt.Errorf("connect to gateway: %w", err)
	}
	defer func() { err = errors.Join(err, session.Close()) }()

	if _, err := fmt.Fprintf(output, "Connected to %s as %s\n", endpoint, agentID); err != nil {
		return err
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("discover tools: %w", err)
	}
	found := false
	for _, tool := range tools.Tools {
		if _, err := fmt.Fprintf(output, "Discovered tool: %s\n", tool.Name); err != nil {
			return err
		}
		found = found || tool.Name == "read_runbook"
	}
	if !found {
		return fmt.Errorf("backend did not advertise read_runbook")
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_runbook", Arguments: map[string]any{"topic": topic},
	})
	if err != nil {
		return fmt.Errorf("call read_runbook: %w", err)
	}
	if result.IsError {
		return fmt.Errorf("read_runbook rejected topic %q", topic)
	}
	if len(result.Content) == 0 {
		return fmt.Errorf("read_runbook returned no content")
	}
	for _, content := range result.Content {
		text, ok := content.(*mcp.TextContent)
		if !ok || text.Text == "" {
			return fmt.Errorf("read_runbook returned empty or non-text content")
		}
		if _, err := fmt.Fprintln(output, text.Text); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(output, "MCP discovery and tool call completed.")
	return err
}
