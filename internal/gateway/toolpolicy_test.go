package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/types"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
	"github.com/jonatan/portkeeper/internal/democlient"
	"github.com/jonatan/portkeeper/internal/runbookmcp"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("audit line is not JSON: %q", line)
		}
		events = append(events, event)
	}
	return events
}

// newPolicyGateway serves one backend whose only account has the given tool list.
func newPolicyGateway(t *testing.T, name string, tools []string, ready bool, handler http.Handler) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	reg := &Registry{backends: map[types.NamespacedName]Backend{
		{Namespace: "policy", Name: name}: {
			Namespace: "policy", Name: name, Ready: ready, Address: strings.TrimPrefix(backend.URL, "http://"),
			AllowedServiceAccounts: []mcpv1alpha1.ServiceAccountReference{{Namespace: "test", Name: "agent", AllowedTools: tools}},
			PolicyObservedAt:       time.Now(),
		},
	}}
	router := NewRouter(reg, NewAgentLimiter(1000, 1000), testAuthenticator(t))
	audit := &lockedBuffer{}
	router.AuditLog = slog.New(slog.NewJSONHandler(audit, nil))
	router.MaxMCPRequestBytes = 512
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, audit
}

func decisionCount(t *testing.T, server, tool, decision string) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := toolDecisionsTotal.WithLabelValues("policy", server, tool, decision).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetCounter().GetValue()
}

func TestToolPolicyThroughSDKClient(t *testing.T) {
	runbooks := runbookmcp.NewHandler()
	server, audit := newPolicyGateway(t, "sdk", []string{runbookmcp.ReadTool}, true,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte(runbookmcp.StreamTool)) {
				t.Error("denied tool call reached the backend")
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			runbooks.ServeHTTP(w, r)
		}))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "policy-test", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   server.URL + "/policy/sdk/mcp",
		HTTPClient: &http.Client{Transport: agentTransport{base: server.Client().Transport}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	call := func(tool string) (*mcp.CallToolResult, error) {
		return session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"topic": "rate-limiting"}})
	}
	if result, err := call(runbookmcp.ReadTool); err != nil || result.IsError {
		t.Fatalf("allowed tool failed: %v %+v", err, result)
	}
	_, err = call(runbookmcp.StreamTool)
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeToolNotPermitted || !strings.Contains(rpcErr.Message, "gateway policy") {
		t.Fatalf("denied tool error = %v, want JSON-RPC code %d", err, CodeToolNotPermitted)
	}
	// A denial is a failed call, not a broken session; discovery stays unfiltered.
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("session unusable after denial: %v %+v", err, tools)
	}
	if result, err := call(runbookmcp.ReadTool); err != nil || result.IsError {
		t.Fatalf("allowed tool failed after denial: %v %+v", err, result)
	}

	var got []string
	for _, event := range audit.events(t) {
		if event["msg"] != "mcp_tool_decision" || event["agent"] != testPrincipal ||
			event["namespace"] != "policy" || event["server"] != "sdk" {
			t.Fatalf("unexpected audit event: %v", event)
		}
		got = append(got, event["tool"].(string)+"/"+event["decision"].(string)+"/"+event["reason"].(string))
	}
	want := "read_runbook/allow/tool_listed stream_runbook/deny/tool_not_listed read_runbook/allow/tool_listed"
	if strings.Join(got, " ") != want {
		t.Fatalf("audit trail = %q, want %q", got, want)
	}
	// Only tools named in the resource become labels; the denied one is not.
	if decisionCount(t, "sdk", runbookmcp.ReadTool, "allow") != 2 || decisionCount(t, "sdk", "_other", "deny") != 1 {
		t.Fatal("tool decision metrics do not match the audit trail")
	}
}

func TestToolPolicyRequestHandling(t *testing.T) {
	const allowed = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read"}}`
	const denied = `{"jsonrpc":"2.0","id":"req-9","method":"tools/call","params":{"name":"write"}}`
	for _, tt := range []struct {
		name, path, body, encoding string
		tools                      []string
		unready                    bool
		status, hits               int
		contains, audit            string
	}{
		{name: "listed-tool", path: "mcp", body: allowed, tools: []string{"read"}, status: 200, hits: 1, audit: "read/allow"},
		{name: "unlisted-tool", path: "mcp", body: denied, tools: []string{"read"}, status: 200, contains: `"id":"req-9"`, audit: "write/deny"},
		{name: "unrestricted-account", path: "mcp", body: denied, status: 200, hits: 1, audit: "write/allow"},
		{name: "empty-list-permits-nothing", path: "mcp", body: allowed, tools: []string{}, status: 200, contains: `"error"`, audit: "read/deny"},
		{name: "other-method-not-audited", path: "mcp", body: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, tools: []string{"read"}, status: 200, hits: 1},
		{name: "denial-precedes-readiness", path: "mcp", body: denied, tools: []string{"read"}, unready: true, status: 200, contains: `"error"`, audit: "write/deny"},
		{name: "unforwarded-call-not-audited-as-allowed", path: "mcp", body: allowed, tools: []string{"read"}, unready: true, status: 503},
		{name: "restricted-account-other-endpoint", path: "echo", body: "hello", tools: []string{"read"}, status: 403},
		{name: "restricted-account-endpoint-case-variant", path: "MCP", body: denied, tools: []string{"read"}, status: 403},
		{name: "unrestricted-account-other-endpoint", path: "echo", body: "hello", status: 200, hits: 1},
		{name: "ambiguous-tool", path: "mcp", body: `{"id":1,"method":"tools/call","params":{"name":"read","Name":"write"}}`, tools: []string{"read"}, status: 400},
		{name: "batch", path: "mcp", body: "[" + allowed + "]", tools: []string{"read"}, status: 400},
		{name: "oversized", path: "mcp", body: `{"id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 600) + `"}}`, status: 413},
		{name: "compressed", path: "mcp", body: allowed, encoding: "gzip", status: 415},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hits := 0
			server, audit := newPolicyGateway(t, "raw", tt.tools, !tt.unready,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits++
					body, _ := io.ReadAll(r.Body)
					if string(body) != tt.body || r.ContentLength != int64(len(tt.body)) {
						t.Errorf("backend body = %q (length %d), want the original request", body, r.ContentLength)
					}
				}))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/policy/raw/"+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer test-token")
			if tt.encoding != "" {
				req.Header.Set("Content-Encoding", tt.encoding)
			}
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != tt.status || !strings.Contains(string(body), tt.contains) || hits != tt.hits {
				t.Fatalf("status=%d hits=%d body=%q, want status=%d hits=%d containing %q",
					res.StatusCode, hits, body, tt.status, tt.hits, tt.contains)
			}
			got := ""
			for _, event := range audit.events(t) {
				got += event["tool"].(string) + "/" + event["decision"].(string)
			}
			if got != tt.audit {
				t.Fatalf("audit = %q, want %q", got, tt.audit)
			}
		})
	}
}

func TestDemoClientExpectsToolDenial(t *testing.T) {
	server, _ := newPolicyGateway(t, "client", []string{runbookmcp.ReadTool}, true, runbookmcp.NewHandler())
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	credentials := democlient.Credentials{TokenFile: tokenFile}
	var output bytes.Buffer
	if err := democlient.ExpectToolDenied(t.Context(), server.URL+"/policy/client/mcp", credentials, runbookmcp.StreamTool, "rate-limiting", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Tool stream_runbook denied by gateway policy") {
		t.Fatalf("unexpected output: %s", &output)
	}
	// A permitted tool must fail the expectation rather than pass silently.
	if err := democlient.ExpectToolDenied(t.Context(), server.URL+"/policy/client/mcp", credentials, runbookmcp.ReadTool, "rate-limiting", io.Discard); err == nil {
		t.Fatal("permitted tool was reported as denied")
	}
}

func TestAuditBoundsClientControlledToolName(t *testing.T) {
	server, audit := newPolicyGateway(t, "bounds", []string{"read"}, true, http.NotFoundHandler())
	tool := strings.Repeat("a", 200) + `\n{\"decision\":\"allow\"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/policy/bounds/mcp",
		strings.NewReader(`{"id":1,"method":"tools/call","params":{"name":"`+tool+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	events := audit.events(t)
	if len(events) != 1 || events[0]["decision"] != "deny" || len(events[0]["tool"].(string)) != maxAuditToolBytes || events[0]["tool_truncated"] != true {
		t.Fatalf("audit events = %v", events)
	}
}
