# Observability contract

Portkeeper deliberately measures two different things. The gateway measures
**HTTP transport requests** without inspecting JSON-RPC bodies. The instrumented
runbook backend measures **actual MCP tool-handler executions**. Initializing
a session, discovering tools, or opening an SSE connection is not a tool
execution. A third-party MCP server needs its own execution instrumentation;
the gateway cannot prove a tool ran merely because it proxied a request.

## Metrics

| Metric | Labels | Meaning |
|--------|--------|---------|
| `mcp_gateway_http_requests_total` | `namespace`, `server`, `endpoint`, `status`, `result` | Finished HTTP handlers, including authentication/policy/rate-limit rejections |
| `mcp_gateway_http_request_duration_seconds` | `namespace`, `server`, `endpoint` | Full HTTP request lifetime, including authentication and streaming |
| `mcp_gateway_http_requests_in_flight` | none | Active handlers, including TokenReview and open streams |
| `mcp_gateway_rate_limited_total` | `agent` | Requests rejected by the verified-identity limiter |
| `mcp_backend_tool_invocations_total` | `tool`, `outcome` | Completed instrumented tool-handler invocations |
| `mcp_backend_tool_duration_seconds` | `tool`, `outcome` | Time spent executing those handlers |

`result` is `complete` or `aborted`. A stream can send HTTP 200 and later
abort; it is then counted as `status="200",result="aborted"`, not a successful
complete response. Rejected requests count as complete HTTP handlers even
though the application operation did not succeed. Backend outcomes distinguish
success, error, and cancellation. Protocol/schema failures rejected before
entering a tool handler are not counted as executions.

Backend metrics do not include gateway authentication/network latency.
They reset when the backend restarts. Use `rate()`/`increase()` rather than
subtracting counter values across pod replacement. Prometheus adds the
backend's namespace/server target labels in the in-cluster demo.

### Migration from checkpoints 1-4

`mcp_gateway_tool_calls_total` and
`mcp_gateway_tool_call_duration_seconds` were misleading transport metrics;
they are replaced by the HTTP names above. The route label `tool` is now
`endpoint`, which is **not** an MCP tool name. The bundled dashboard and
Prometheus queries use the new names. This is an intentional pre-v0.1
metrics API change, not an alias that continues implying tool execution.

## Cardinality budget

Labels never contain MCP session IDs, request IDs, token contents, tool input
topics, arbitrary URL paths, or arbitrary unverified caller identities.

- At most **256 registered namespace/server pairs** retain individual HTTP
  labels per gateway process. Further pairs aggregate as
  `namespace="_overflow",server="_overflow"`.
- Unknown/ambiguous/malformed routes use `namespace="",server="_unresolved"`;
  unauthenticated requests use `namespace="",server="_unauthenticated"`.
  Random requested server names cannot allocate new server-label series.
- Endpoint is only `mcp` or `other`. Fifteen common HTTP status codes retain
  their exact number; other valid codes use `1xx` through `5xx`, and invalid
  codes use `other`. Result has two values.
- Rate-limit metrics retain at most **128 verified identities**, then use
  `agent="_overflow"`. This metric budget is separate from the 10,000-bucket
  enforcement limit; aggregation does not change anyone's quota.
- Backend tool labels come only from the two registered tool names, and
  outcome has three values. Operators must likewise bound scrape-target
  churn in Prometheus retention; process-local limits are not a global
  historical-series limit.

Admissions last for the process lifetime. Evicting a label from an auxiliary
map would not remove already-allocated Prometheus series, so the gateway
does not recycle these admissions. In total, HTTP counters are bounded by
259 server groups x 2 endpoints x 21 statuses x 2 results; histograms have
259 x 2 label combinations, and rate-limit counters have at most 129.
Logs retain exact verified identities and requested routes for diagnostics;
metric aggregation is not a reason to drop audit identity.

## Streams and sessions

Counters and duration histograms are updated when the HTTP handler finishes,
not when its headers are sent. Open streams appear in the in-flight gauge.
The duration histogram includes the entire stream lifetime, so it is not a
substitute for tool latency or time to first streamed event.

The backend owns sessions and their five-minute inactivity timeout. There is
no gateway session affinity across backend replicas, no virtual aggregated
MCP endpoint, and no automatic tool-call retry. Token and policy checks happen
for each HTTP request, not continuously inside an already-open stream.
The read-only `stream_runbook` demo emits progress before its final result;
the benchmark records first-progress and completion latency separately.

## Logs and failure evidence

The gateway emits `gateway_request` with quoted namespace/server/endpoint,
verified agent, HTTP status, a `completed` flag, and request lifetime.
Unauthenticated requests have an empty agent. Credentials are never logged.
The controller emits `mcpserver_status` only after a changed status has been
successfully patched, with generation, phase, readiness, and reason.

`make e2e` captures a real backend-pod loss. It briefly cordons its own
single-node kind cluster so a replacement cannot hide the outage, deletes
the backend pod, and observes HTTP 503 and `Ready=False` from an already
running client. It captures controller conditions, gateway counters, logs,
and Prometheus `up=0`; uncordoning allows a replacement and a successful
new MCP session. The scheduling hold is a demo control, not measured
unassisted recovery time or a production availability guarantee.

## Dashboard and scraping

For the in-cluster demo:

```bash
kubectl apply -k deploy/
kubectl port-forward -n portkeeper-system svc/grafana 3000:3000
# http://localhost:3000/d/portkeeper
```

This kustomization installs only monitoring in `portkeeper-system`, scrapes
the gateway Service and the runbook backend, and provisions HTTP and backend
execution panels. The underlying standalone `deploy/prometheus.yaml` retains
the host-run `host.docker.internal:8080` development target.

Prometheus has the trusted gateway-network label so it can scrape the
backend behind NetworkPolicy. Because MCP and metrics share a port, this
also permits that trusted monitoring pod to reach backend MCP directly;
it is not a path-scoped firewall. Ordinary client pods remain blocked.
Monitoring is demo-only: unauthenticated endpoints, anonymous Grafana admin,
and no persistent storage. Do not expose it to untrusted networks.
