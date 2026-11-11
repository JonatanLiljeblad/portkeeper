package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
)

const (
	// DefaultMaxMCPRequestBytes bounds the memory one inspected MCP POST can hold.
	DefaultMaxMCPRequestBytes int64 = 1 << 20

	// CodeToolNotPermitted is the JSON-RPC error code of a gateway tool denial.
	// It sits in the implementation-defined range and avoids codes used by MCP SDKs.
	CodeToolNotPermitted = -32090

	maxAuditToolBytes  = 128
	mcpBodyReadTimeout = 10 * time.Second
)

// toolCall is a tools/call the gateway has attributed to a tool.
type toolCall struct {
	tool   string
	reason string
}

// readMCPRequest buffers and inspects one MCP POST body. A nonzero status
// means the request must be refused with that HTTP status and message.
func (rt *Router) readMCPRequest(w http.ResponseWriter, req *http.Request) (body []byte, msg mcpRequest, status int, problem string) {
	reject := func(reason string, status int, problem string) ([]byte, mcpRequest, int, string) {
		mcpRequestRejectionsTotal.WithLabelValues(reason).Inc()
		return nil, mcpRequest{}, status, problem
	}
	if enc := strings.TrimSpace(req.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return reject("unsupported_encoding", http.StatusUnsupportedMediaType, "encoded MCP request bodies are not supported")
	}
	limit := rt.MaxMCPRequestBytes
	if limit <= 0 {
		limit = DefaultMaxMCPRequestBytes
	}
	// The body is one JSON message; a client may not hold a handler open by
	// trickling it. The deadline is cleared before any response is streamed.
	controller := http.NewResponseController(w)
	deadlineSet := controller.SetReadDeadline(time.Now().Add(mcpBodyReadTimeout)) == nil
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, limit))
	if deadlineSet {
		_ = controller.SetReadDeadline(time.Time{})
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return reject("too_large", http.StatusRequestEntityTooLarge,
				fmt.Sprintf("MCP request body exceeds %d bytes", limit))
		}
		return reject("unreadable", http.StatusBadRequest, "could not read MCP request body")
	}
	msg, err = inspectMCPRequest(body)
	if errors.Is(err, errMCPBatch) {
		return reject("batch", http.StatusBadRequest, err.Error())
	}
	if err != nil {
		return reject("malformed", http.StatusBadRequest, err.Error())
	}
	return body, msg, 0, ""
}

// authorizeTool applies an account's tool allowlist. An account without a
// list is unrestricted; a present but empty list permits nothing.
func authorizeTool(account mcpv1alpha1.ServiceAccountReference, tool string) (allowed bool, reason string) {
	if account.AllowedTools == nil {
		return true, "account_unrestricted"
	}
	if slices.Contains(account.AllowedTools, tool) {
		return true, "tool_listed"
	}
	return false, "tool_not_listed"
}

// audit writes one decision event. Tool names are client-controlled, so they
// are length-bounded here and escaped by the structured handler.
func (rt *Router) audit(ctx context.Context, agent string, b Backend, tool, decision, reason string) {
	attrs := []slog.Attr{
		slog.String("agent", agent),
		slog.String("namespace", b.Namespace),
		slog.String("server", b.Name),
		slog.String("tool", boundedTool(tool)),
		slog.String("decision", decision),
		slog.String("reason", reason),
	}
	if len(tool) > maxAuditToolBytes {
		attrs = append(attrs, slog.Bool("tool_truncated", true))
	}
	rt.AuditLog.LogAttrs(ctx, slog.LevelInfo, "mcp_tool_decision", attrs...)
}

func boundedTool(tool string) string {
	if len(tool) > maxAuditToolBytes {
		tool = tool[:maxAuditToolBytes]
	}
	return strings.ToValidUTF8(tool, "")
}

// writeToolDenied answers a denied tools/call as a JSON-RPC error on HTTP 200,
// so an MCP client reports a failed call and keeps its session.
func writeToolDenied(w http.ResponseWriter, id json.RawMessage, tool string) {
	type rpcError struct {
		Code    int               `json:"code"`
		Message string            `json:"message"`
		Data    map[string]string `json:"data"`
	}
	payload, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcError        `json:"error"`
	}{"2.0", id, rpcError{
		Code:    CodeToolNotPermitted,
		Message: fmt.Sprintf("tool %q is not permitted for this client by gateway policy", boundedTool(tool)),
		Data:    map[string]string{"source": "portkeeper-gateway", "reason": "tool_not_listed"},
	}})
	if err != nil {
		http.Error(w, "tool is not permitted by gateway policy", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func replaceBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.TransferEncoding = nil
}
