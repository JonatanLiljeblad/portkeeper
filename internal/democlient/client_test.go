package democlient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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
	if err := WaitForStatus(t.Context(), server.URL, Credentials{AgentID: "probe-agent"}, 503, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Observed HTTP 503") {
		t.Fatalf("unexpected output %q", &output)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := WaitForStatus(ctx, server.URL, Credentials{AgentID: "probe-agent"}, 404, &output); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted deadline error, got %v", err)
	}
}

func TestProbeRunbookEndpoint(t *testing.T) {
	server := httptest.NewServer(runbookmcp.NewHandler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := WaitForStatus(ctx, server.URL+"/mcp", Credentials{}, http.StatusBadRequest, &output); err != nil {
		t.Fatalf("stateful MCP GET without a session should return 400: %v", err)
	}
}

func tokenFile(t *testing.T, token string) string {
	t.Helper()
	f, err := os.CreateTemp(".", ".client-test-token-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	if _, err := f.WriteString(token); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestCredentialsRotateAndRedirectsAreRefused(t *testing.T) {
	path := tokenFile(t, "first")
	want := "first"
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect destination must never be contacted")
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+want {
			t.Error("request did not use the current token file contents")
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	var output bytes.Buffer
	credentials := Credentials{TokenFile: path}
	client := httpClient(credentials)
	for _, token := range []string{"first", "rotated"} {
		want = token
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		response, err := client.Get(source.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusTemporaryRedirect {
			t.Fatal("redirect must be returned without following it")
		}
	}
	if err := WaitForStatus(t.Context(), source.URL, credentials, 307, &output); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), source.URL, credentials, "gateway-routing", &output); err == nil {
		t.Fatal("SDK workflow must not follow redirects")
	}
	if strings.Contains(output.String(), "rotated") {
		t.Fatal("token leaked to output")
	}
}

func TestRunWithToken(t *testing.T) {
	path := tokenFile(t, "sdk-test-token")
	handler := runbookmcp.NewHandler()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer sdk-test-token" {
			t.Error("SDK request missing credentials")
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := Run(ctx, server.URL+"/mcp", Credentials{TokenFile: path}, "gateway-routing", &output); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 3 || !strings.Contains(output.String(), "MCP discovery and tool call completed.") {
		t.Fatal("SDK workflow did not complete")
	}
	if strings.Contains(output.String(), "sdk-test-token") {
		t.Fatal("token leaked to output")
	}
}

func TestInvalidCredentialsAndTargets(t *testing.T) {
	var output bytes.Buffer
	if err := Run(t.Context(), "http://localhost/mcp", Credentials{}, "gateway-routing", &output); err == nil {
		t.Fatal("SDK run should require a token file")
	}
	for _, endpoint := range []string{"relative", "http://user:password@localhost/mcp", "http://localhost/mcp?token=secret"} {
		if err := validateTarget(endpoint); err == nil {
			t.Fatal("accepted unsafe endpoint")
		}
	}
	for _, token := range []string{"", "invalid\nheader"} {
		client := httpClient(Credentials{TokenFile: tokenFile(t, token)})
		if _, err := client.Get("http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "token file must contain") {
			t.Fatal("malformed token should fail before dialing")
		}
	}
}

func TestNetworkProbeRejectsFalsePositives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	var output bytes.Buffer
	if err := ProbeNetwork(t.Context(), server.URL, false, &output); err != nil {
		t.Fatal(err)
	}
	if err := ProbeNetwork(t.Context(), server.URL, true, &output); err == nil {
		t.Fatal("reachable target must not count as blocked")
	}
	server.Close()
	if err := ProbeNetwork(t.Context(), server.URL, true, &output); err == nil {
		t.Fatal("connection refused must not count as policy enforcement")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ProbeNetwork(ctx, "http://does-not-exist.invalid:9001", true, &output); err == nil {
		t.Fatal("DNS failure/cancellation must not count as policy enforcement")
	}
}
