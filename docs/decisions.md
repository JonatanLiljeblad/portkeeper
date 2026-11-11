# Architecture decisions

The first table describes the experimental v0.1.0 scope. Decisions made
after that release, including ones that reverse it, are listed separately
below. Neither is a production certification.

| Decision | Why | Consequence / evidence |
|----------|-----|------------------------|
| Kubernetes CRD as registry; separate controller and gateway | Reuse Kubernetes ownership/lifecycle without introducing another database | Current-generation readiness and namespace-aware routes; `make e2e` covers updates, recreation and garbage collection |
| One backend per MCP endpoint; transparent streaming proxy | Preserve SDK protocol/session behavior without aggregating tools or buffering JSON-RPC | Gateway tests cover forwarding, streaming and cancellation; backend owns sessions |
| Workload ServiceAccount tokens, audience-bound TokenReview | Kubernetes supplies signature/expiry/bound-object validation; no local credential database | Every routed request depends on the API; no OAuth discovery or desktop login flow |
| Per-server ServiceAccount allowlists, deny by default | Authorization refers to verified namespaced identities rather than a spoofable header | Empty-policy, denied-account and spoofing scenarios are in the Kubernetes walkthrough |
| Fresh authorization over an eventually consistent registry | Preserve routing cache without allowing stale policy indefinitely | Five-second polling; policy older than 15 seconds fails closed; existing streams remain open |
| Enforced Calico NetworkPolicy in the isolated demo | Authorization is ineffective if ordinary clients can bypass the gateway | Exact-target positive controls accompany denied Service/PodIP probes; trusted deployers/admins remain outside the boundary |
| Local bounded quotas, not Redis/shared state | Keep a small implementation whose guarantees are explicit | 10,000 limiter identities per replica; restarts reset quotas; no cluster-global rate guarantee |
| Transport metrics at gateway, execution metrics at backend | Forwarding a JSON-RPC request cannot prove execution, and response inspection complicates streaming | Separate metric families; third-party backends need instrumentation; bounded label admissions |
| Real measurements with explicitly tuned benchmark profile | Default workload/API-client limits would confound proxy/authentication throughput measurements | `make benchmark` keeps auth/isolation enabled but temporarily raises both limits; reports are not production SLOs |
| Experimental source release with reproducible evidence | Prefer verifiable behavior over production-ready branding or invented latency targets | Published workload/configuration, raw results, failure walkthrough and known limitations |

## After v0.1.0

| Decision | Why | Consequence / evidence |
|----------|-----|------------------------|
| **Reverses "transparent proxy":** the gateway reads MCP request bodies, never responses | Per-server access cannot express "may read, may not act"; the tool name exists only in the JSON-RPC body | Every POST to `/mcp` is buffered (1 MiB cap, ten-second read deadline) and parsed, about 4 µs for a typical call on the development machine; responses and SSE streams remain unbuffered and the streaming, cancellation and full-duplex tests still pass |
| Strict request parsing, fail closed | A lenient gateway parser and a different backend parser can disagree on which tool a request names, bypassing policy | Duplicate or case-variant `method`/`params`/`name`, batches, trailing data and unattributable `tools/call` return 400; `TestInspectMCPRequest` covers each case. Non-conforming clients are refused rather than forwarded uninspected |
| Optional `allowedTools` per account; omission means all tools | Changing the meaning of existing entries would silently lock out, or silently narrow, every v0.1.0 resource | Per-server deny-by-default is unchanged; tool restriction is opt-in per account and the API rejects an empty list |
| Tool denial is a JSON-RPC error on HTTP 200, code `-32090` | An HTTP 403 is a transport failure to MCP SDKs and can end the session; an agent needs a failed call it can react to | SDK test and kind workflow show the denied call fails, the session continues, and the backend handler is never entered. HTTP status alone no longer identifies a denial |
| `tools/list` is not filtered | Hiding tools needs response rewriting and SSE buffering, which the gateway refuses to do | Restricted clients can discover tools they cannot call; enforcement is on `tools/call` only. Resources and prompts are not governed |
| Audit event per decision, on standard output | Operators need "who called which tool" from verified identity without trusting backends | `allow` means authorized and forwarded, not executed; arguments are never logged; durability and tamper-resistance belong to the log pipeline |
| Decision metrics label only declared tools | Requested tool names are client-controlled | Undeclared names aggregate as `_other`; 1,024 declared server/tool pairs, then `_overflow` |
| `spec.serviceAccountName` gives a backend its pod identity; no API token without it | A backend that acts on the cluster needs its own narrowly scoped identity, and one that does not should hold no credential an attacker could use | The controller sets `automountServiceAccountToken` to true only when the field is set, and to false otherwise. Upgrading the controller therefore rolls every existing backend once. Anyone who can create an `MCPServer` can run a pod as any ServiceAccount in that namespace, as with a Deployment |
| The state-changing demo backend is bounded three ways | Tool policy should not be the only thing between an agent and a cluster | Role limited to `get`/`patch` on one named Deployment; one namespace fixed at startup; opt-in label on the target. RBAC cannot distinguish restart from scale (both are `patch`), so the gateway's tool policy plus backend network isolation is what separates them |
| The agent separates planning from execution; CI replays a script | A model in CI costs money, needs a secret, and is not deterministic; the enforcement path does not depend on who chooses the calls | `make agent-demo` and e2e step 4d use the script. The model-driven planner is tested against a local stand-in and has not yet been run against the real API |
| Tool calls are never retried; model calls are, on overload | A tool call may change state; a model call does not | Up to two retries with backoff for HTTP 429/5xx or a transport failure from the model API only |
| The model key is a file, mounted from a short-lived Secret | Command lines, logs and repositories all leak | The workflow refuses a key file inside the repository and deletes the Secret after the run; the key is readable through the cluster's kubeconfig while the run lasts |

## Deliberately deferred

Backend token injection (`authType`/`authSecretRef` remain reserved), external
OIDC/OAuth login, tool aggregation, tool-discovery filtering, argument-level
policy, distributed quotas, HA controllers, backend replica/session routing,
autoscaling, durable telemetry and audit storage, and multi-cluster operation
require separate designs. They are not implicit features.

Before exposing the gateway outside the local demo, provide TLS and protect
its internal hop, restrict who can create workload tokens or change policy/
network labels, size API capacity and telemetry retention, and choose an
appropriate session/availability strategy. The included anonymous monitoring
must not be treated as a secure operational deployment.
