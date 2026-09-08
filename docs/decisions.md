# Architecture decisions

These decisions describe the experimental v0.1.0 scope, not a production
certification.

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

## Deliberately deferred

Backend token injection (`authType`/`authSecretRef` remain reserved), external
OIDC/OAuth login, tool aggregation, per-tool authorization, distributed quotas,
HA controllers, backend replica/session routing, autoscaling, durable telemetry,
and multi-cluster operation require separate designs. They are not implicit
features of v0.1.0.

Before exposing the gateway outside the local demo, provide TLS and protect
its internal hop, restrict who can create workload tokens or change policy/
network labels, size API capacity and telemetry retention, and choose an
appropriate session/availability strategy. The included anonymous monitoring
must not be treated as a secure operational deployment.
