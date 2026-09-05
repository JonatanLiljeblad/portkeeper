package democlient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonatan/portkeeper/internal/runbookmcp"
)

func TestWaitForStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("X-Agent-ID") != "probe-agent" {
			t.Errorf("unexpected probe: %s, agent=%q", r.Method, r.Header.Get("X-Agent-ID"))
		}

		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var output bytes.Buffer
	if err := WaitForStatus(t.Context(), server.URL, "probe-agent", 503, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Observed HTTP 503") {
		t.Fatalf("unexpected output %q", &output)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := WaitForStatus(ctx, server.URL, "probe-agent", 404, &output); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted deadline error, got %v", err)
	}
}

func TestProbeRunbookEndpoint(t *testing.T) {
	server := httptest.NewServer(runbookmcp.NewHandler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := WaitForStatus(ctx, server.URL+"/mcp", "probe-agent", http.StatusBadRequest, &output); err != nil {
		t.Fatalf("stateful MCP GET without a session should return 400: %v", err)
	}
}
