package gateway

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func newTestGateway(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	reg := &Registry{backends: map[types.NamespacedName]Backend{
		{Namespace: "test", Name: "demo"}: {
			Namespace: "test", Name: "demo", Ready: true, Address: strings.TrimPrefix(backend.URL, "http://"),
		},
	}}
	server := httptest.NewServer(NewRouter(reg, NewAgentLimiter(100, 100)))
	t.Cleanup(server.Close)
	return server
}

func TestParsePath(t *testing.T) {
	for _, tt := range []struct {
		path, namespace, server, tool string
		ok                            bool
	}{
		{"/demo/mcp", "", "demo", "mcp", true},
		{"/demo/echo", "", "demo", "echo", true},
		{"/demo/reverse-string/", "", "demo", "reverse-string", true},
		{"/test/demo/mcp", "test", "demo", "mcp", true},
		{"/test/demo/echo/", "test", "demo", "echo", true},
		{"/", "", "", "", false},
		{"/demo", "", "", "", false},
		{"/demo//mcp", "", "", "", false},
		{"/test/demo/mcp/extra", "", "", "", false},
	} {
		t.Run(tt.path, func(t *testing.T) {
			namespace, server, tool, ok := parsePath(tt.path)
			if namespace != tt.namespace || server != tt.server || tool != tt.tool || ok != tt.ok {
				t.Fatalf("parsePath(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tt.path, namespace, server, tool, ok, tt.namespace, tt.server, tt.tool, tt.ok)
			}
		})
	}
}

func TestRouterForwardsRequest(t *testing.T) {
	for _, tt := range []struct {
		name, method, path string
		status             int
	}{
		{"legacy", http.MethodPost, "echo", http.StatusOK},
		{"mcp-post", http.MethodPost, "mcp", http.StatusOK},
		{"mcp-get", http.MethodGet, "mcp", http.StatusOK},
		{"mcp-delete", http.MethodDelete, "mcp", http.StatusOK},
		{"backend-error", http.MethodPost, "mcp", http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
			server := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.Path != "/"+tt.path || r.URL.RawQuery != "key=value" {
					t.Errorf("unexpected backend request: %s %s", r.Method, r.URL)
				}
				for header, want := range map[string]string{
					"X-Agent-ID":           "test-agent",
					"Mcp-Session-Id":       "test-session",
					"Mcp-Protocol-Version": "2025-06-18",
					"Accept":               "application/json, text/event-stream",
					"Content-Type":         "application/json",
					"Last-Event-ID":        "event-1",
				} {
					if got := r.Header.Get(header); got != want {
						t.Errorf("%s = %q, want %q", header, got, want)
					}
				}
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != body {
					t.Errorf("backend body = %q, err = %v", data, err)
				}
				w.Header().Set("Mcp-Session-Id", "response-session")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write(data)
			}))

			req, err := http.NewRequestWithContext(t.Context(), tt.method,
				server.URL+"/demo/"+tt.path+"?key=value", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Agent-ID", "test-agent")
			req.Header.Set("Mcp-Session-Id", "test-session")
			req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Last-Event-ID", "event-1")
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			data, err := io.ReadAll(res.Body)
			if err != nil || string(data) != body {
				t.Fatalf("response body = %q, err = %v", data, err)
			}
			if res.StatusCode != tt.status || res.Header.Get("Mcp-Session-Id") != "response-session" {
				t.Fatalf("unexpected backend response: %d %v", res.StatusCode, res.Header)
			}
		})
	}
}

func TestRouterStreamsAndCancels(t *testing.T) {
	cancelled := make(chan struct{})
	server := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first-event\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush backend stream: %v", err)
			return
		}
		<-r.Context().Done()
		close(cancelled)
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/demo/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Agent-ID", "stream-agent")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "data: first-event\n" {
		t.Fatalf("streamed line = %q, err = %v", line, err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("client cancellation did not reach the backend")
	}
}

func TestRouterRejectsInvalidRequests(t *testing.T) {
	for _, tt := range []struct {
		name, path, agent string
		status            int
	}{
		{"missing-agent", "/demo/mcp", "", http.StatusBadRequest},
		{"invalid-path", "/demo", "test-agent", http.StatusBadRequest},
		{"unknown-server", "/missing/mcp", "test-agent", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			router := NewRouter(&Registry{}, NewAgentLimiter(5, 10))
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			req.Header.Set("X-Agent-ID", tt.agent)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
		})
	}
}

func TestRouterRateLimit(t *testing.T) {
	// Unknown servers also consume the request budget before registry lookup.
	router := NewRouter(&Registry{}, NewAgentLimiter(0.001, 1))
	for i, want := range []int{http.StatusNotFound, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/missing/mcp", nil)
		req.Header.Set("X-Agent-ID", "limited-agent")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("request %d: status = %d, want %d", i, rec.Code, want)
		}
	}
}

func TestRouterNamespaceIsolationAndReadiness(t *testing.T) {
	reg := &Registry{backends: make(map[types.NamespacedName]Backend)}
	for _, namespace := range []string{"team-a", "team-b"} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/mcp" {
				t.Errorf("backend path = %q, want /mcp", r.URL.Path)
			}
			_, _ = io.WriteString(w, namespace)
		}))
		t.Cleanup(backend.Close)
		reg.backends[types.NamespacedName{Namespace: namespace, Name: "shared"}] = Backend{
			Namespace: namespace, Name: "shared", Ready: true, Address: strings.TrimPrefix(backend.URL, "http://"),
		}
	}
	reg.backends[types.NamespacedName{Namespace: "team-a", Name: "pending"}] = Backend{
		Namespace: "team-a", Name: "pending",
	}
	router := NewRouter(reg, NewAgentLimiter(100, 100))
	for _, tt := range []struct {
		path   string
		status int
		body   string
	}{
		{"/team-a/shared/mcp", 200, "team-a"},
		{"/team-b/shared/mcp", 200, "team-b"},
		{"/shared/mcp", 409, "multiple namespaces"},
		{"/team-c/shared/mcp", 404, "unknown MCP server"},
		{"/team-a/pending/mcp", 503, "not ready"},
		{"/pending/mcp", 503, "not ready"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			req.Header.Set("X-Agent-ID", "isolation-test")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.body) {
				t.Fatalf("response = %d %q, want %d containing %q", w.Code, w.Body.String(), tt.status, tt.body)
			}
			if tt.status == 503 && w.Header().Get("Retry-After") != "5" {
				t.Fatal("unready response missing Retry-After")
			}
		})
	}
}

func TestRouterConnectionFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	backend.Close()
	router := NewRouter(&Registry{backends: map[types.NamespacedName]Backend{
		{Namespace: "test", Name: "down"}: {
			Namespace: "test", Name: "down", Ready: true, Address: strings.TrimPrefix(backend.URL, "http://"),
		},
	}}, NewAgentLimiter(5, 10))
	req := httptest.NewRequest(http.MethodPost, "/test/down/mcp", nil)
	req.Header.Set("X-Agent-ID", "failure-test")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestStatusCapturingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &statusCapturingWriter{ResponseWriter: rec, status: http.StatusOK}
	w.WriteHeader(http.StatusEarlyHints)
	if w.status != http.StatusOK {
		t.Fatalf("informational response recorded as final status: %d", w.status)
	}
	w.WriteHeader(http.StatusCreated)
	if w.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.status)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed {
		t.Fatal("Flush did not reach the underlying response writer")
	}
}
