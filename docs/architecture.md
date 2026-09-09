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
   server (image, port, exposed tools, allowed client ServiceAccounts).
2. The **controller** notices the new/changed object and reconciles it into
   a real `Deployment` + `Service` running that MCP server's container.
3. The **gateway** continuously reads the set of `MCPServer` objects from
   the Kubernetes API — this is its live registry, no separate database.
4. An MCP client connects to `/<namespace>/<server-name>/mcp` with
   a gateway-audience ServiceAccount bearer token on every HTTP request.
   The gateway verifies it with Kubernetes TokenReview, then authorizes the
   verified ServiceAccount against the server's deny-by-default policy.
   The registry uses `NamespacedName`
   keys. Legacy `/<server-name>/<endpoint>` routes resolve only when the
   name is cluster-unique; ambiguity returns 409, including when one of the
   colliding servers is unready.
5. The backend handles MCP initialization, discovery, and tool execution.
   The gateway forwards protocol headers, bodies, responses, and cancellation
   without owning sessions or interpreting tool calls. Its response wrapper
   exposes `Unwrap` so the reverse proxy can flush SSE responses and enable
   full-duplex HTTP/1 request handling. Without full duplex, an early response
   flush can close the inbound body while the upstream transport is still
   checking its EOF, aborting an otherwise healthy stream. HTTP/2 already
   supports concurrent request reads and response writes.
   Gateway credentials are stripped and `X-Agent-ID` is overwritten with
   the verified username before proxying.
6. The gateway logs and measures HTTP handlers using explicit HTTP metric
   names and `endpoint="mcp"`. In-flight requests include open streams;
   completed and aborted responses are distinct. Backend instrumentation
   counts actual tool executions without teaching the gateway JSON-RPC.
   Namespace/server labels are bounded, and unresolved routes aggregate
   without reflecting arbitrary client-supplied names.

## Reconciliation and readiness

The controller preserves Kubernetes defaults while updating its managed
image, port, replica count, labels, and TCP readiness probe. It will only
mutate existing child resources controlled by that MCPServer's UID. Status
is patched only when it changes, preserving condition transition timestamps
and avoiding self-triggered reconciliation loops.

`Ready=True` requires the current Deployment generation to have been observed
and its rollout completed, with the desired replica updated, ready, and
available. An available old replica cannot make an incomplete new rollout
ready. Progress-deadline and replica failures produce `Failed`; ordinary
startup and incomplete rollouts are `Pending`. Deleting CRs do not recreate
children, and owner references let Kubernetes garbage-collect them.

The shared `MCPServer.IsReady()` predicate rejects missing conditions, stale
CR generations, and deleting objects. The gateway returns 503 for registered
but unready backends, 404 for absent ones, and 502 for upstream connection
failures. It does not retry tool calls. The registry is still polled every
five seconds and retains its last snapshot on API failure; these guarantees
are eventually observed, not an instantaneous health or security boundary.
Authorization fails closed with 503 when the policy snapshot is older than
15 seconds, even though the routing cache is retained.

## MCP interoperability boundary

`internal/runbookmcp` implements a read-only documentation backend with the
official Go MCP SDK; `hack/runbook-mcp-server` serves it on loopback by
default. Gateway integration tests use an SDK client and actual HTTP servers,
with an in-memory registry entry rather than Kubernetes discovery.

`make e2e` covers the separate Kubernetes boundary: the controller and gateway
run in `portkeeper-system`, the `MCPServer` and backend run in
`portkeeper-demo`, and an SDK client Job connects through the gateway's
cluster Service. Each control-plane component uses its own ServiceAccount
and ClusterRoleBinding. The client Job mounts only a short-lived,
gateway-audience projected token, not a default API-audience token.

The gateway's `/readyz` endpoint becomes successful after its first registry
list. The Deployment readiness probe uses this startup signal. It does not
assert backend readiness or ongoing registry freshness.

The gateway uses one endpoint per backend, not one virtual aggregated MCP
server. Session state stays in the backend; the current one-replica workload
avoids cross-replica session routing. Any future scaling work must explicitly
choose sessionless backends or an appropriate session-routing strategy.

The [identity and policy decision](authentication.md) describes TokenReview,
per-server authorization, credential separation, bounded replica-local
limiting, and the enforced backend network-isolation demo. See
[observability](observability.md) for separate transport/execution metrics
and [decisions](decisions.md) for the release scope. This is not an OAuth
authorization server.

## Why the controller and gateway are separate binaries

They have different failure modes and scaling needs: the controller is a
singleton reconciliation loop (like most K8s operators), while the gateway
is a request-serving hot path that may eventually need to scale
horizontally. Keeping them separate now avoids having to split them apart
under pressure later.
