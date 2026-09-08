# Copilot Instructions for portkeeper

## Project model

portkeeper is a Go 1.25+, Kubernetes-native registry and gateway for MCP
servers. Its control plane and gateway are independently runnable binaries:

- `cmd/controller` runs the controller-runtime reconciliation loop. An
  `mcp.portkeeper.dev/v1alpha1` `MCPServer` CR is reconciled into a one-replica
  `Deployment` named after the CR and a `<mcpserver-name>-svc` `Service`. Both
  resources must retain their controller reference and the
  `mcp.portkeeper.dev/server: <name>` selector/label contract. Readiness
  follows the current Deployment rollout and TCP probe, not resource creation.
  Preserve Kubernetes defaults during mutation and patch status only on
  changes. Never adopt resources not controlled by the CR's UID.
- `cmd/gateway` is the request-serving path. `internal/gateway.Registry` polls
  all `MCPServer` resources every five seconds and constructs backend addresses
  as `<name>-svc.<namespace>.svc.cluster.local:<port>`. When running the
  gateway on the host, `GATEWAY_BACKEND_HOST` replaces the DNS host while a
  same-port `kubectl port-forward` supplies connectivity.

The gateway accepts `/<namespace>/<server-name>/mcp` for Streamable HTTP and
`/<namespace>/<server-name>/<tool-name>` for toy routes. Legacy two-segment
paths work only for cluster-unique server names; ambiguity returns 409.
It requires a Kubernetes ServiceAccount bearer token with gateway audience
(`GATEWAY_AUTH_AUDIENCE`, default `portkeeper`) on every routed request.
TokenReview is uncached, audience-checked, bounded to 32 concurrent calls with
a three-second deadline. Never trust `X-Agent-ID`: overwrite it with the
verified username upstream and strip gateway Authorization/Proxy-Authorization.
It strips the routing prefix before reverse proxying,
and records a structured `gateway_request` log plus Prometheus metrics for proxied
and rate-limited calls. Keep these routing, attribution, and metric-label
semantics consistent when changing gateway behavior. `/metrics` is exposed on
the gateway's HTTP server. The controller metrics server uses `:8081`; the
gateway defaults to `:8080`.

The `tools` field is metadata; live HTTP routing uses `Registry.Resolve`
and `types.NamespacedName` keys, not tool-only discovery. Use
`MCPServer.IsReady()` as the shared current-generation condition predicate.
Registered-but-unready servers return 503, absent servers 404, and upstream
connection failures 502. Registry polling is asynchronous and retains the
previous snapshot on API errors. Per-agent rate limits are in-memory and process-local:
`GATEWAY_RATE_LIMIT_RPS` defaults to `5` and `GATEWAY_RATE_LIMIT_BURST` to
`10`. Invalid/nonfinite values are fatal at startup. Limiter state is capped
at 10,000 identities; only fully replenished buckets idle for 15 minutes are
evicted at capacity, otherwise new identities receive 429.

`spec.allowedServiceAccounts` is deny-by-default, per-namespaced-server
authorization, including legacy URLs. Empty policies deny everyone.
Routing snapshots survive API errors, but policies older than 15 seconds
must fail closed with 503. Authentication failures return 401; verified but
unauthorized accounts receive 403 before backend readiness is disclosed.
Existing streams are not reauthorized mid-response. See docs/authentication.md.

`hack/toy-mcp-server` is a standalone nested Go module and demo HTTP backend,
not part of the root module's `./...` package pattern.
`internal/runbookmcp` and `hack/runbook-mcp-server` are in the root module
and provide the real, read-only MCP demo backend using the official Go SDK.
Its documentation is embedded at build time; inputs must not permit arbitrary
filesystem reads.

## Commands

```bash
# Root Go module: compile all production packages.
make build

# Run all root-module Go tests with the race detector.
make test

# Build images and exercise actual Kubernetes discovery/routing in kind.
make e2e

# Add direct/gateway MCP concurrency and streaming measurements to that workflow.
make benchmark

# Exercise an actual MCP SDK client through the gateway without Kubernetes.
go test -race ./internal/gateway -run '^TestMCPInteroperability$' -count=1

# Run the read-only MCP backend on 127.0.0.1:9001.
make run-runbook-server

# Regenerate artifacts after changing api/v1alpha1 types or kubebuilder markers.
make generate     # api/v1alpha1/zz_generated.deepcopy.go
make manifests    # config/crd/bases/

# Reconcile root-module dependencies after intentionally changing imports.
make tidy
```

Run `gofmt` on changed Go files. There is no repository lint target.

For the kind-based end-to-end demo, build the toy image with `make toy-image`,
create the cluster with `make kind-up`, load it with
`kind load docker-image toy-mcp-server:0.1`, install the CRD with
`make install-crds`, and run `make run-controller` and `make run-gateway` in
separate terminals. The README contains the required backend port-forward and
gateway `GATEWAY_BACKEND_HOST=localhost` setup. `make apply-sample` registers
the demo `MCPServer`.

For the real MCP workflow, use `make e2e`, not the legacy toy-image targets.
It creates its own named cluster and kubeconfig, refuses existing cluster
names, captures logs, and deletes the cluster it created. `KEEP_CLUSTER=1`
retains it and prints exact access/cleanup commands. See
`docs/kubernetes-demo.md`. The root Dockerfile's `controller`, `gateway`,
`runbooks`, and `demo-client` targets produce the four local images.
The workflow also covers duplicate names, broken image/port updates,
recovery, child recreation, and real Kubernetes garbage collection.
Its separate `deploy/kind-e2e-config.yaml` disables kind's default CNI;
`make e2e` installs checksum-pinned Calico VXLAN to enforce NetworkPolicy.
Backend policies combine the system namespace AND the dedicated
`mcp.portkeeper.dev/gateway: "true"` pod label. Keep it distinct from the
gateway Service selector so network-control pods cannot become endpoints.
Negative Service/PodIP probes must retain their exact-target healthy controls.

## Kubernetes API and generated configuration

- Treat `api/v1alpha1/mcpserver_types.go` as the source of truth for the CRD.
  After changing its schema, kubebuilder markers, or registered types, run
  both generation commands and include the resulting deepcopy and CRD changes.
- `authType` is schema-constrained to `none` or `token`; `authSecretRef` reserves
  backend credentials and is not consumed. Do not confuse implemented
  client-to-gateway ServiceAccount authentication with backend token injection.
- Keep `config/rbac/role.yaml` aligned with the controller's
  `+kubebuilder:rbac` markers and the gateway's read-only registry access.
  The controller needs write access to `MCPServer` status plus full management
  access to owned Deployments and Services; the gateway lists/gets/watches
  `MCPServer` objects and creates TokenReviews, but cannot read Secrets.

## Observability and local deployment

- `/readyz` is an unauthenticated initial-registry-sync signal used by the
  gateway Deployment; it does not promise backend health or cache freshness.
- In-cluster controller/gateway manifests live in `portkeeper-system`; the
  primary MCP sample and client Job live in `portkeeper-demo`, with a
  same-name lifecycle sample in `portkeeper-other`. Keep
  `deploy/rbac.yaml` account namespaces aligned with these Deployments.
- CI runs `make benchmark` through the same e2e script and uploads only
  `artifacts/e2e.*/logs/`. Never include generated kubeconfigs in artifacts.
- Keep `statusCapturingWriter.Unwrap`: the reverse proxy uses
  `http.ResponseController` to reach the underlying flush support for SSE.
  Do not buffer streams or introduce automatic tool-call retries.
- Gateway `mcp_gateway_http_*` metrics count HTTP handlers, not MCP executions.
  `endpoint` is mcp|other; `result` is complete|aborted, including failures
  after HTTP200 headers. Open streams remain in the in-flight gauge.
  Actual execution metrics are `mcp_backend_tool_*` in the instrumented backend.
  Do not parse/buffer gateway JSON-RPC to infer execution.
- Gateway metrics are registered globally in `internal/gateway/metrics.go` and
  are consumed by the Grafana dashboard in `deploy/grafana.yaml`. Preserve
  their label contracts unless updating the dashboard and queries too.
  Retain cardinality caps: 256 resolved server pairs, fixed unresolved/
  unauthenticated/overflow groups, 128 verified rate-limit agent labels,
  finite endpoint/status/result labels; admissions do not recycle.
- `deploy/prometheus.yaml` intentionally scrapes
  `host.docker.internal:8080` for host-run gateway development. If changing to
  an in-cluster gateway, point its scrape target at the gateway Service.
- Monitoring manifests are demo-only: Grafana permits anonymous admin access
  and neither Prometheus nor Grafana has persistence.
- `kubectl apply -k deploy/` installs in-cluster monitoring in portkeeper-system.
  Its trusted Prometheus pod has the gateway-network label and can access
  both metrics and MCP on backend port9001; ordinary clients remain isolated.
- Benchmarks temporarily raise per-agent and TokenReview client QPS/burst
  to1000/1000 and restore defaults afterward. Normal defaults remain5/10;
  max32 simultaneous TokenReviews and3s deadline still apply.
