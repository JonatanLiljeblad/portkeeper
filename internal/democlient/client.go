package democlient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Credentials struct {
	TokenFile string
	AgentID   string
}

type credentialTransport struct {
	credentials Credentials
}

func (t credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Del("Authorization")
	out.Header.Del("X-Agent-ID")
	if t.credentials.AgentID != "" {
		out.Header.Set("X-Agent-ID", t.credentials.AgentID)
	}
	if t.credentials.TokenFile != "" {
		// Reopen the projected volume path on every request to follow rotation.
		data, err := os.ReadFile(t.credentials.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" || strings.ContainsAny(token, " \t\r\n") {
			return nil, errors.New("token file must contain a nonempty bearer token")
		}
		out.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultTransport.RoundTrip(out)
}

func httpClient(credentials Credentials) *http.Client {
	return &http.Client{
		Transport: credentialTransport{credentials: credentials},
		// Never reattach credentials to a redirect destination, even same-origin.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Run discovers and calls the read-only runbook tool through an MCP endpoint.
// It makes one tool call, without application-level retries.
func Run(ctx context.Context, endpoint string, credentials Credentials, topic string, output io.Writer) (err error) {
	if err := validateTarget(endpoint); err != nil {
		return err
	}
	if credentials.TokenFile == "" {
		return errors.New("token file is required for an MCP workflow")
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "portkeeper-demo", Version: "0.1.0"}, &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient(credentials),
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return fmt.Errorf("connect to gateway: %w", err)
	}
	defer func() { err = errors.Join(err, session.Close()) }()

	return readRunbook(ctx, session, endpoint, topic, output)
}

func validateTarget(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("endpoint must be an absolute HTTP or HTTPS URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must not contain credentials, a query, or a fragment")
	}

	return nil
}

func readRunbook(ctx context.Context, session *mcp.ClientSession, endpoint, topic string, output io.Writer) error {
	if _, err := fmt.Fprintf(output, "Connected to %s\n", endpoint); err != nil {
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
func WaitForStatus(ctx context.Context, endpoint string, credentials Credentials, want int, output io.Writer) error {
	if err := validateTarget(endpoint); err != nil {
		return err
	}
	if want < 100 || want > 599 {
		return fmt.Errorf("expected status must be an HTTP status code")
	}
	client := httpClient(credentials)
	client.Timeout = 5 * time.Second
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

// ProbeNetwork tests TCP connectivity, not HTTP or authentication. A blocked
// result requires successful DNS and a dial timeout, never refusal or DNS failure.
// The caller must independently prove the target is healthy.
func ProbeNetwork(ctx context.Context, endpoint string, blocked bool, output io.Writer) error {
	if err := validateTarget(endpoint); err != nil {
		return err
	}
	u, _ := url.Parse(endpoint)
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return fmt.Errorf("resolve network probe target: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("network probe target resolved to no addresses")
	}
	for _, address := range addresses {
		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(address.String(), port))
		cancel()
		if conn != nil {
			conn.Close()
		}
		if !blocked {
			if err != nil {
				return fmt.Errorf("expected TCP connectivity: %w", err)
			}
			continue
		}
		if err == nil {
			return errors.New("expected network isolation, but TCP connection succeeded")
		}
		var opErr *net.OpError
		if ctx.Err() != nil || !errors.As(err, &opErr) || opErr.Op != "dial" || !opErr.Timeout() {
			return fmt.Errorf("expected enforced TCP dial timeout, not DNS/refusal/cancellation: %w", err)
		}
	}
	_, err = fmt.Fprintf(output, "Network probe passed: DNS resolved; TCP blocked=%t at %s\n", blocked, endpoint)
	return err
}
