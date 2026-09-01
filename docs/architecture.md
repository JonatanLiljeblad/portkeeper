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
4. A client sends a tool-call request to the gateway. The gateway looks up
   which backend owns that tool/server name and proxies the request there.
5. The gateway logs the call (server, tool, latency, status) and exposes it
   as a Prometheus metric.

## Why the controller and gateway are separate binaries

They have different failure modes and scaling needs: the controller is a
singleton reconciliation loop (like most K8s operators), while the gateway
is a request-serving hot path that may eventually need to scale
horizontally. Keeping them separate now avoids having to split them apart
under pressure later.
