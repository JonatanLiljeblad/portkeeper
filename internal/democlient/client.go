package democlient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	if err := validateTarget(endpoint, agentID); err != nil {
		return err
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

	return readRunbook(ctx, session, endpoint, agentID, topic, output)
}

func validateTarget(endpoint, agentID string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("endpoint must be an absolute HTTP or HTTPS URL")
	}
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("agent ID is required")
	}

	return nil
}

func readRunbook(ctx context.Context, session *mcp.ClientSession, endpoint, agentID, topic string, output io.Writer) error {
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

// WaitForStatus polls only HTTP GET, never MCP tools/call. It is used by the
// lifecycle demo to wait for the gateway's asynchronously refreshed registry.
func WaitForStatus(ctx context.Context, endpoint, agentID string, want int, output io.Writer) error {
	if err := validateTarget(endpoint, agentID); err != nil {
		return err
	}
	if want < 100 || want > 599 {
		return fmt.Errorf("expected status must be an HTTP status code")
	}
	client := &http.Client{
		Transport:     agentTransport{agentID: agentID},
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := "no response"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := client.Do(req)
		if err != nil {
			last = err.Error()
		} else {
			if err := res.Body.Close(); err != nil {
				return fmt.Errorf("close probe response: %w", err)
			}
			if res.StatusCode == want {
				_, err := fmt.Fprintf(output, "Observed HTTP %d at %s\n", want, endpoint)
				return err
			}
			last = res.Status
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for HTTP %d; last response: %s: %w", want, last, ctx.Err())
		case <-ticker.C:
		}
	}
}
