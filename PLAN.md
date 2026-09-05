# Portkeeper roadmap

## Goal

Build a Kubernetes-native MCP control plane and gateway whose claims can be
demonstrated with real clients, reproducible deployments, failure scenarios,
and measured results. The target users are platform teams operating internal
MCP servers for AI agents.

The architecture stays deliberately small: Go, controller-runtime, an
`MCPServer` CRD as the registry, an independently runnable HTTP gateway,
Prometheus/Grafana, and kind for local deployments.

## Starting baseline

The controller creates owned Deployments and Services from `MCPServer`
resources. The gateway polls the registry, proxies toy HTTP tool routes, and
provides self-reported agent attribution, process-local rate limiting, logs,
and a demo metrics dashboard.

This baseline does not yet establish production readiness: the toy backend
is not an MCP implementation, `Ready` does not reflect workload availability,
and `X-Agent-ID` is not authentication.

## Delivery checkpoints

Each checkpoint is a reviewable implementation increment with its own
acceptance criteria. Commit and push the completed checkpoint, explain the
result and remaining limitations, and pause before starting the next one.

### 1. Real MCP interoperability foundation

Status: complete.

- Use the official Go MCP SDK for a useful read-only runbook backend and
  an actual MCP client in gateway integration tests.
- Expose a backend at `/<server-name>/mcp`, forwarding to its `/mcp`
  Streamable HTTP endpoint. Preserve existing toy routes.
- Preserve protocol headers, JSON bodies, streaming, cancellation, and
  backend HTTP errors without automatically retrying tool calls.
- Exercise discovery and tool invocation through the real gateway handler,
  plus streaming and legacy-routing regression cases.

Acceptance: the SDK client connects through the gateway, discovers the
runbook tool, and retrieves a document. A flushed stream reaches the client
before the backend closes it. This checkpoint uses local HTTP servers, not
a simulated claim of Kubernetes end-to-end coverage.

### 2. Reproducible Kubernetes MCP workflow

Status: complete.

- Package the real MCP backend, controller, and gateway for kind; supply
  working ServiceAccounts and role bindings.
- Provide a sample `MCPServer` and an SDK-based demo client with documented
  agent-header configuration.
- Add a repeatable end-to-end command and CI for Go checks and the kind
  integration workflow.
- Capture a short terminal walkthrough showing declaration, deployment,
  tool discovery, and a successful call through the gateway.

Delivered through `make e2e`, `.github/workflows/ci.yml`, and
`docs/kubernetes-demo.md`, with an actual captured run in `docs/demo.txt`.
The gateway's startup readiness now waits for its initial registry list;
this does not replace the backend lifecycle work in checkpoint 3.

Acceptance: a fresh checkout can reproduce the workflow with documented
prerequisites; CI proves it against Kubernetes rather than a registry stub.

### 3. Trustworthy Kubernetes lifecycle

Status: pending.

- Derive readiness and meaningful conditions from observed Deployment
  availability and generation, not successful resource creation.
- Introduce namespace-aware registry keys and routing with an explicit
  compatibility decision for existing name-only URLs.
- Cover repeated reconciliation, spec changes, owned-resource recreation,
  and garbage collection.
- Define client-visible behavior for unknown, unready, and failed backends.
  Preserve cancellation and avoid automatic retries of side-effecting calls.

Acceptance: an unavailable backend is not advertised as ready; same-name
servers in different namespaces cannot silently overwrite one another;
deletion and recovery scenarios have repeatable coverage.

### 4. Verified identity and per-server policy

Status: pending.

- Choose and document a supported authentication flow before implementation.
  Distinguish client-to-gateway identity from backend credentials.
- Authorize verified agents per server; derive rate limits and audit identity
  from authentication rather than trusting an arbitrary header.
- Demonstrate deployment-level gateway-bypass prevention using network
  isolation, with a local cluster setup that actually enforces the policy.
- Bound per-agent limiter state and document replica-local semantics before
  considering a shared rate-limit service.

Acceptance: one authenticated agent is allowed and another denied; spoofing
`X-Agent-ID` does not bypass policy; direct backend access is blocked in the
documented deployment.

### 5. Operational evidence and portfolio release

Status: pending.

- Compare direct-backend and gateway latency under a documented workload,
  including concurrency and streaming. Publish hardware, methodology, and
  measured overhead rather than invented performance targets.
- Demonstrate backend pod failure and recovery, including client errors,
  controller conditions, logs, and metrics.
- Distinguish transport requests from actual MCP tool invocations in
  observability; define label-cardinality limits and session behavior.
- Publish concise architecture decisions, known limitations, a release,
  and a short walkthrough.

Acceptance: another developer can reproduce the measurements and failure
demo, and each reliability or security claim points to concrete evidence.

## Scope guardrails

Use one MCP endpoint per backend before attempting tool aggregation. Keep
the existing controller/gateway separation and CRD-backed registry. Prefer
protocol correctness, lifecycle behavior, and evidence over a UI, Redis,
autoscaling, or multi-cluster support. Revisit those only for a demonstrated
need.
