# Architecture

## Components

```
                     ┌─────────────────────────┐
                     │   Kubernetes API Server   │
                     │  (stores MCPServer CRs)   │
                     └───────────┬───────────────┘
                     watches     │      reads registry
              ┌──────────────────┴───────────────────┐
              │                                       │
     ┌────────▼─────────┐                   ┌─────────▼────────┐
     │   Controller       │                  │     Gateway        │
     │ (cmd/controller)   │                  │  (cmd/gateway)     │
     │                     │                  │                    │
     │ reconcile loop:     │                  │ - list MCPServers  │
     │ MCPServer spec  ->  │                  │ - route request    │
     │ Deployment+Service  │                  │   to matching pod  │
     └────────┬────────────┘                  │ - log/metric each  │
              │ creates/manages                │   tool call        │
              │                                └─────────┬─────────┘
     ┌────────▼─────────┐                                │
     │  MCP Server Pod    │◄───────── proxied requests ───┘
     │  (user's own image)│
     └─────────────────────┘

              ▲
              │ registers via
     ┌────────┴─────────┐
     │  Client / Agent    │  (only ever talks to the Gateway,
     │  (e.g. Claude,     │   never directly to an MCP server pod)
     │   another service) │
     └─────────────────────┘
```

## Data flow

1. An operator applies an `MCPServer` custom resource describing an MCP
   server (image, port, exposed tools, auth type).
2. The **controller** notices the new/changed object and reconciles it into
   a real `Deployment` + `Service` running that MCP server's container.
3. The **gateway** continuously reads the set of `MCPServer` objects from
   the Kubernetes API — this is its live registry, no separate database.
4. An MCP client connects to `/<server-name>/mcp` with `X-Agent-ID` on every
   HTTP request. The gateway looks up the backend by server name and forwards
   to `/mcp`. Legacy toy calls use `/<server-name>/<tool-name>`.
5. The backend handles MCP initialization, discovery, and tool execution.
   The gateway forwards protocol headers, bodies, responses, and cancellation
   without owning sessions or interpreting tool calls. Its response wrapper
   exposes `Unwrap` so the reverse proxy can flush SSE responses.
6. The gateway logs and measures completed HTTP requests. Existing metric
   names say "tool calls", but MCP requests use the route label `tool="mcp"`;
   they do not yet identify the tool inside a JSON-RPC message.

## MCP interoperability boundary

`internal/runbookmcp` implements a read-only documentation backend with the
official Go MCP SDK; `hack/runbook-mcp-server` serves it on loopback by
default. Gateway integration tests use an SDK client and actual HTTP servers,
with an in-memory registry entry rather than Kubernetes discovery.

The gateway uses one endpoint per backend, not one virtual aggregated MCP
server. Session state stays in the backend; the current one-replica workload
avoids cross-replica session routing. Any future scaling work must explicitly
choose sessionless backends or an appropriate session-routing strategy.

Namespaced registry keys, workload-derived readiness, authenticated agent
identity, and protocol-aware metrics remain roadmap work. Current
`X-Agent-ID` attribution and rate limiting are not access control.

## Why the controller and gateway are separate binaries

They have different failure modes and scaling needs: the controller is a
singleton reconciliation loop (like most K8s operators), while the gateway
is a request-serving hot path that may eventually need to scale
horizontally. Keeping them separate now avoids having to split them apart
under pressure later.
