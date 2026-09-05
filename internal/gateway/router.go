package gateway

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Router resolves each incoming request to a backend MCP server and
// proxies it there. MCP backends use "/<server-name>/mcp"; legacy HTTP
// tools continue to use "/<server-name>/<tool-name>".
type Router struct {
	registry *Registry
	limiter  *AgentLimiter
}

func NewRouter(reg *Registry, limiter *AgentLimiter) *Router {
	return &Router{registry: reg, limiter: limiter}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// X-Agent-ID is a self-reported label identifying the caller — not
	// verified identity/auth. Required so rate limits and logs always
	// have an agent to attribute to; we never guess or default one.
	agentID := req.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "missing required X-Agent-ID header", http.StatusBadRequest)
		return
	}

	serverName, toolName, ok := parsePath(req.URL.Path)
	if !ok {
		http.Error(w, "expected path /<server-name>/<tool-name>", http.StatusBadRequest)
		return
	}

	if !rt.limiter.Allow(agentID) {
		elapsed := time.Since(start)
		log.Printf("tool_call server=%s tool=%s agent=%s status=%d latency=%s",
			serverName, toolName, agentID, http.StatusTooManyRequests, elapsed)
		recordCall(serverName, toolName, http.StatusTooManyRequests, elapsed)
		rateLimitedTotal.WithLabelValues(agentID).Inc()
		http.Error(w, "rate limit exceeded for agent "+agentID, http.StatusTooManyRequests)
		return
	}

	backend, found := rt.registry.ByName(serverName)
	if !found {
		recordCall(serverName, toolName, http.StatusNotFound, time.Since(start))
		http.Error(w, "unknown MCP server: "+serverName, http.StatusNotFound)
		return
	}

	target := &url.URL{Scheme: "http", Host: backend.Address}
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Strip only the routing prefix. MCP bodies and protocol headers belong
	// to the backend; the gateway does not terminate the MCP session.
	outReq := req.Clone(req.Context())
	outReq.URL.Path = "/" + toolName
	outReq.URL.RawPath = ""

	statusRecorder := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
	proxy.ServeHTTP(statusRecorder, outReq)

	elapsed := time.Since(start)
	log.Printf("tool_call server=%s tool=%s agent=%s status=%d latency=%s",
		serverName, toolName, agentID, statusRecorder.status, elapsed)
	recordCall(serverName, toolName, statusRecorder.status, elapsed)
}

func parsePath(path string) (server, tool string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// statusCapturingWriter lets us observe the status code the reverse proxy
// writes, since http.ResponseWriter doesn't expose it after the fact.
type statusCapturingWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCapturingWriter) WriteHeader(code int) {
	if code >= http.StatusOK {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying Flush support,
// which the reverse proxy needs to deliver SSE without buffering.
func (w *statusCapturingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
