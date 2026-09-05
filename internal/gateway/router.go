package gateway

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Router resolves each incoming request to a backend MCP server and
// proxies it there. Namespaced routes are "/<namespace>/<server>/<endpoint>";
// legacy "/<server>/<endpoint>" routes require a cluster-unique server name.
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

	namespace, serverName, toolName, ok := parsePath(req.URL.Path)
	if !ok {
		http.Error(w, "expected /<namespace>/<server>/<endpoint> or /<server>/<endpoint>", http.StatusBadRequest)
		return
	}

	recorder := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		elapsed := time.Since(start)
		log.Printf("tool_call namespace=%s server=%s tool=%s agent=%s status=%d latency=%s",
			namespace, serverName, toolName, agentID, recorder.status, elapsed)
		recordCall(namespace, serverName, toolName, recorder.status, elapsed)
	}()
	if !rt.limiter.Allow(agentID) {
		rateLimitedTotal.WithLabelValues(agentID).Inc()
		http.Error(recorder, "rate limit exceeded for agent "+agentID, http.StatusTooManyRequests)
		return
	}

	backend, err := rt.registry.Resolve(namespace, serverName)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrServerNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, ErrAmbiguousServer) {
			status = http.StatusConflict
		}
		http.Error(recorder, err.Error(), status)
		return
	}
	namespace = backend.Namespace
	if !backend.Ready {
		recorder.Header().Set("Retry-After", "5")
		http.Error(recorder, "MCP server is not ready", http.StatusServiceUnavailable)
		return
	}

	target := &url.URL{Scheme: "http", Host: backend.Address}
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Strip only the routing prefix. MCP bodies and protocol headers belong
	// to the backend; the gateway does not terminate the MCP session.
	outReq := req.Clone(req.Context())
	outReq.URL.Path = "/" + toolName
	outReq.URL.RawPath = ""

	proxy.ServeHTTP(recorder, outReq)
}

func parsePath(path string) (namespace, server, tool string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 && len(parts) != 3 {
		return "", "", "", false
	}
	for _, part := range parts {
		if part == "" {
			return "", "", "", false
		}
	}
	if len(parts) == 3 {
		return parts[0], parts[1], parts[2], true
	}
	return "", parts[0], parts[1], true
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
