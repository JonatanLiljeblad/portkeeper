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

Status: complete.

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

Compatibility decision: use `/<namespace>/<server>/<endpoint>` as the
canonical route. Preserve `/<server>/<endpoint>` only when its name is
unique across namespaces; otherwise return 409. Known unready backends
return 503, unknown servers 404, and failed upstream connections 502.

Delivered: current-generation Ready conditions with TCP-probed Deployment
availability, default-preserving reconciliation, ownership checks, and
namespace-aware registry/metrics. `make e2e` now exercises same-name servers,
broken image/port updates, recovery, child recreation, and foreground
garbage collection. Gateway polling remains eventually consistent; readiness
does not assert MCP application health.

### 4. Verified identity and per-server policy

Status: complete.

Authentication decision: audience-bound Kubernetes ServiceAccount bearer
tokens, verified on every HTTP request using TokenReview. Per-server
`allowedServiceAccounts` is deny-by-default; gateway and backend credentials
are separate. See `docs/authentication.md` for the supported flow, upgrade
behavior, bounded limiter semantics, and revocation/stream limitations.

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

Delivered: per-request audience-checked TokenReview, verified identity for
authorization/auditing/quotas, deny-by-default namespaced ServiceAccount
allowlists, credential stripping, bounded limiter state, and fail-closed
stale policy. `make e2e` uses checksum-pinned Calico and proves authenticated
MCP calls, 401/403/spoofing and policy-revocation behavior, and direct
Service/PodIP isolation with healthy-target controls in both namespaces.
The captured run is in `docs/demo.txt`. This remains workload-token auth,
not MCP OAuth; backend credentials and replica-shared quotas are not implemented.

### 5. Operational evidence and portfolio release

Status: complete.

Decision: retain a transparent gateway with explicit HTTP metrics; instrument
actual tool-handler execution in the runbook backend. Publish an experimental
`v0.1.0` release with measured, reproducible evidence, then perform a full
project overview before moving beyond this roadmap. Pause after checkpoint 5;
the overview starts only when explicitly resumed.

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

Delivered: explicit bounded HTTP transport metrics, instrumented backend
tool executions, real progress streaming, a provisioned monitoring dashboard,
and a controlled pod-loss/recovery scenario with conditions, logs, and metrics.
`make benchmark` runs the authenticated direct/gateway workload in kind.
`docs/results.md` publishes hardware/configuration, pooled results from 2,080
successful measured invocations, raw reports, and failure evidence. The
experimental `v0.1.0` release documents architecture decisions and limitations;
these are not production availability or performance guarantees.

Post-checkpoint status: v0.1.0 is closed. The roadmap was resumed with the
v0.2 checkpoints below.

## v0.2 direction

v0.1.0 is a correct, authenticated, observable proxy. v0.2 turns it into a
governance layer for AI agents: it decides and records which agent may call
which tool, accepts standard MCP clients, and is shown working with a real
agent on installable, published artifacts.

Primary audience for the result: AI infrastructure / agent platform
engineering, with identity and policy as the supporting depth.

The same delivery rules apply: one checkpoint at a time, acceptance backed by
reproducible evidence, commit and push, explain remaining limitations, pause.
Treat each checkpoint as work for a company: weigh safety, operating cost,
and efficiency, and build what is useful rather than what is impressive.
The summary after a checkpoint proposes a roadmap change only when there is
a specific reason or a better way to solve a problem.
Each checkpoint that reverses a v0.1.0 decision records the new decision in
`docs/decisions.md`.

### 6. Repository baseline and tool-level governance

Status: complete.

Baseline, before the feature work:

- Add a license, repository description and topics, and correct the README
  repo layout. Keep the README's operational detail but lead with the problem
  and the shortest reproducible command.

Decision to record first: the gateway becomes MCP-aware for requests only.
It reads JSON-RPC request bodies on the `/mcp` route to identify the method
and tool name. Responses and streams stay unbuffered and unparsed.

- Extend `spec.allowedServiceAccounts` so each entry can restrict the tools
  that account may call. Preserve deny-by-default and define the behavior of
  entries without a tool list before implementation. Regenerate deepcopy and
  CRD output.
- Enforce the policy on `tools/call`. Bound the inspected body size and define
  behavior for malformed, oversized, and non-JSON bodies, and for methods
  other than `tools/call`.
- Choose and document how a denial reaches the client (HTTP status or
  JSON-RPC error) so an SDK client surfaces it as a tool error rather than a
  transport failure.
- Emit one structured audit event per tool-call decision: verified identity,
  namespace, server, tool, decision, reason. An allowed event means
  "authorized and forwarded", not "executed".
- Add decision metrics with a bounded tool label, following the existing
  cardinality caps.
- State explicitly whether `tools/list` results are filtered. Filtering
  requires response rewriting and is out of scope unless a separate decision
  accepts it.

Acceptance: in kind, one ServiceAccount calls an allowed tool and is denied a
second tool on the same server; the denial is a clean SDK-visible error; both
decisions appear in the audit log and metrics; the streaming and cancellation
regressions and the full-duplex behavior still pass.

Delivered: Apache-2.0 license, repository description and topics, and a
problem-first README with a corrected layout. `allowedTools` is an optional
per-account list; omission keeps the v0.1.0 meaning of all tools. Every POST
to `/mcp` is buffered (1 MiB, ten-second read deadline) and strictly parsed;
ambiguous bodies that different JSON parsers could read differently are
refused. A denied `tools/call` returns a JSON-RPC error (`-32090`) on HTTP
200, before readiness is consulted, and never reaches the backend. Each
decision produces a structured audit event and a bounded-label counter, with
a dashboard panel. `make e2e` step 4c proves allow, deny, session survival,
audit attribution, and non-execution against the real cluster.

Remaining limitations: `tools/list` is not filtered; resources and prompts
are not governed; policy is by tool name, not arguments; the audit trail is
a log stream without durable or tamper-evident storage; an allowed event
does not prove execution. The cost of inspection is measured only by a
parser micro-benchmark; the end-to-end figure is refreshed in checkpoint 10.

### 7. Standard MCP authorization (OAuth 2.1 / OIDC)

Status: not started.

Decision to record first: which identity provider runs in kind, and how
external subjects are expressed in per-server policy alongside
ServiceAccounts.

- Make the gateway an OAuth 2.1 resource server as described by the MCP
  authorization specification: protected resource metadata, a
  `WWW-Authenticate` challenge on 401, and audience-bound access tokens
  validated against the issuer's published keys.
- Keep the ServiceAccount flow working and unchanged for in-cluster workloads.
  Both identity kinds feed the same authorization, audit, and rate-limit path.
- Define key rotation, clock skew, issuer outage, and token revocation
  behavior. Authorization still fails closed.
- Cover wrong audience, wrong issuer, expired, and unsigned tokens.

Acceptance: an unmodified MCP client completes discovery and login against
the in-cluster identity provider and calls a tool through the gateway; a
token minted for another audience is rejected; the ServiceAccount e2e
scenarios still pass.

### 8. Real agent scenario

Status: not started.

Decision: the headline demo uses a real LLM agent. CI runs a deterministic
scripted replay of the same tool sequence, so the pipeline needs no API key
and no model call. The model credential is supplied at run time and never
appears in the repository, images, logs, or artifacts.

- Add a second MCP backend with a state-changing tool that acts only on a
  sandboxed demo resource, so policy has something meaningful to block.
- Build an incident scenario: the agent reads a runbook, inspects state, and
  attempts a remediation. One step is permitted and one is denied by policy.
- Show the outcome from the operator's side: the audit trail and dashboard
  explain what the agent did and what it was refused.
- Record the run as the project's primary demo.

Acceptance: `make` targets run the scripted scenario in kind and, with a
key supplied, the live agent scenario; the denied action is visible to the
agent as a tool error and to the operator in the audit trail; the recording
matches a reproducible run.

### 9. Production shape

Status: not started.

- Publish versioned controller, gateway, and backend images to a registry
  with an SBOM and signatures, built by an automated release workflow.
- Provide an installable package (Helm chart or equivalent) that deploys from
  the published images, replacing hand-applied manifests for users.
- Terminate TLS at the gateway and protect the gateway-to-backend hop, or
  document precisely why a hop is left unencrypted.
- Run more than one gateway replica. State the resulting rate-limit
  semantics; add shared quota state only if the measured behavior requires it.
- Add OpenTelemetry tracing so one agent tool call is followable from gateway
  to backend, without buffering streams.

Acceptance: a fresh cluster installs a tagged release from published
artifacts without a local build; signatures verify; deleting one gateway
replica under load does not fail new requests; a trace links a gateway
request to its backend tool execution.

### 10. Portfolio release

Status: not started.

- Rewrite the README around the problem, the demo, and the evidence, with the
  operational reference moved into `docs/`.
- Add an architecture diagram and embed the recorded agent demo.
- Write a short design narrative: the decisions that were reversed between
  v0.1 and v0.2, and why.
- Refresh the benchmark and failure evidence against the v0.2 gateway,
  including the cost of request inspection.
- Publish the release with an honest limitations section.

Acceptance: a reader with five minutes understands the problem, sees the
agent demo, and can find the evidence behind every claim; a reader with an
hour can reproduce it.

## Scope guardrails

Use one MCP endpoint per backend; tool aggregation stays out of scope. Keep
the existing controller/gateway separation and CRD-backed registry. Request
inspection is allowed from checkpoint 6; response parsing, stream buffering,
and automatic tool-call retries are not. Prefer protocol correctness,
lifecycle behavior, and evidence over a UI, autoscaling, or multi-cluster
support. Add shared state such as Redis only for a demonstrated need.
