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
	auth     Authenticator
}

func NewRouter(reg *Registry, limiter *AgentLimiter, auth Authenticator) *Router {
	return &Router{registry: reg, limiter: limiter, auth: auth}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	agentID := ""
	authErr := ErrAuthenticationUnavailable
	if rt.auth != nil {
		agentID, authErr = rt.auth.Authenticate(req.Context(), req)
	}

	namespace, serverName, toolName, ok := parsePath(req.URL.Path)
	recorder := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		elapsed := time.Since(start)
		log.Printf("tool_call namespace=%q server=%q tool=%q agent=%q status=%d latency=%s",
			namespace, serverName, toolName, agentID, recorder.status, elapsed)
		if authErr == nil && agentID != "" {
			recordCall(namespace, serverName, toolName, recorder.status, elapsed)
		}
	}()
	if authErr != nil || agentID == "" {
		if errors.Is(authErr, ErrUnauthenticated) {
			recorder.Header().Set("WWW-Authenticate", `Bearer realm="portkeeper"`)
			http.Error(recorder, ErrUnauthenticated.Error(), http.StatusUnauthorized)
		} else {
			recorder.Header().Set("Retry-After", "5")
			http.Error(recorder, ErrAuthenticationUnavailable.Error(), http.StatusServiceUnavailable)
		}
		return
	}
	if !ok {
		http.Error(recorder, "expected /<namespace>/<server>/<endpoint> or /<server>/<endpoint>", http.StatusBadRequest)
		return
	}
	if !rt.limiter.Allow(agentID) {
		rateLimitedTotal.WithLabelValues(agentID).Inc()
		recorder.Header().Set("Retry-After", "1")
		http.Error(recorder, "agent rate limit or limiter capacity exceeded", http.StatusTooManyRequests)
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
	if time.Since(backend.PolicyObservedAt) > policyMaxAge {
		recorder.Header().Set("Retry-After", "5")
		http.Error(recorder, "authorization policy is stale", http.StatusServiceUnavailable)
		return
	}
	if !backend.allows(agentID) {
		http.Error(recorder, "agent is not authorized for this MCP server", http.StatusForbidden)
		return
	}
	if !backend.Ready {
		recorder.Header().Set("Retry-After", "5")
		http.Error(recorder, "MCP server is not ready", http.StatusServiceUnavailable)
		return
	}

	target := &url.URL{Scheme: "http", Host: backend.Address}
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host
		r.SetXForwarded()
		r.Out.Header.Del("Authorization")
		r.Out.Header.Del("Proxy-Authorization")
		// Rewrite runs after hop-by-hop header removal, so Connection cannot
		// remove or replace the identity verified by the gateway.
		r.Out.Header.Set("X-Agent-ID", agentID)
	}}

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
