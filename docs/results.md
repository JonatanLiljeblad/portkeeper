# v0.1.0 operational evidence

This is one measured, single-node experimental run, not a performance promise
or a production-readiness claim. Run `make benchmark` to reproduce the workload
and failure scenario on a new, isolated kind cluster. It retains logs/reports
and removes the cluster it created.

## Environment and workload

Measured on **2026-09-08 at 18:50 UTC** on an Apple M2 with 8 logical CPUs and
16 GiB host memory, Darwin 25.6.0/arm64. Docker 29.7.2 exposed 8 CPUs and
8,319,770,624 bytes of memory to its Linux VM. The client ran Go 1.25.14 on
Linux/arm64. The cluster used the digest-pinned Kubernetes 1.35.8 kind image
and checksum-pinned Calico 3.32.0.

Gateway and backend each had one replica on that node. The gateway requested
50m CPU/64Mi memory, with a 256Mi memory limit and no CPU limit. The backend
had no explicit resource requests/limits. The benchmark client requested
100m CPU/64Mi memory, with a 256Mi memory limit. Prometheus, Grafana, Calico,
and the Kubernetes/control-plane components were also running.

Authentication and network isolation stayed enabled. For this **measurement
profile only**, the gateway's per-agent RPS/burst and TokenReview client
QPS/burst were 1000/1000 instead of their usual 5/10 defaults. The 32-review
concurrency limit and three-second review deadline remained unchanged.
The direct baseline used a trusted, explicitly permitted network actor and
sent no gateway credential. Gateway latency includes real TokenReview,
token-file reads, proxying, telemetry, and network overhead.

There were **24 phases, 2,080 measured invocations, 312 warm-ups, and zero
reported errors**. Each worker had its own SDK session, with one outstanding
tool call. Both tools were measured at concurrency 1, 4, and 8, with 20
measured calls and three warm-ups per worker per phase. Two repetitions
reversed direct/gateway ordering. Setup/discovery and session close are
excluded from the latency samples, but any failure there fails the report.

See the [methodology](benchmark-methodology.md), [raw report](evidence/benchmark.json),
[environment capture](evidence/benchmark-environment.txt), and captured
[gateway](evidence/benchmark-gateway-deployment.yaml) and
[backend](evidence/benchmark-backend-deployment.yaml) configuration.
The environment capture names the pre-release base commit and dirty worktree:
measurements were taken from the release implementation before the final
commit, not from the unchanged base commit alone.

## Invocation completion latency

Values below are **milliseconds**, recomputed from raw samples using
nearest-rank percentiles and pooling the two repetitions for each leg.
The p50 delta is gateway p50 minus direct p50, not a percentile of paired
request differences. Per-repetition results and p99 remain in the raw report.

| Tool | Concurrency | Calls per leg | Direct p50 | Gateway p50 | p50 delta | Direct p95 | Gateway p95 |
|------|-------------|---------------|------------|-------------|-----------|------------|-------------|
| `read_runbook` | 1 | 40 | 0.343 | 1.381 | 1.038 | 0.840 | 1.813 |
| `read_runbook` | 4 | 160 | 1.216 | 2.409 | 1.193 | 2.308 | 4.322 |
| `read_runbook` | 8 | 320 | 1.993 | 4.730 | 2.737 | 4.449 | 8.654 |
| `stream_runbook` | 1 | 40 | 153.723 | 158.691 | 4.968 | 155.766 | 161.740 |
| `stream_runbook` | 4 | 160 | 155.084 | 158.190 | 3.106 | 158.529 | 163.023 |
| `stream_runbook` | 8 | 320 | 153.835 | 157.489 | 3.654 | 159.244 | 166.277 |

The streaming tool deliberately waits about 150ms while emitting three SDK
progress notifications before its final result. Its completion latency must
not be presented as generic MCP execution cost.

## First streamed progress

These are client-observed **first SDK progress callback** latencies in
milliseconds, not HTTP-header latency or final-response latency. Every
measured stream delivered all three updates, with the first at least 25ms
before completion; fully buffered progress would fail this workload.

| Concurrency | Direct p50 | Gateway p50 | Direct p95 | Gateway p95 |
|-------------|------------|-------------|------------|-------------|
| 1 | 0.988 | 5.024 | 2.077 | 7.069 |
| 4 | 1.753 | 5.121 | 5.397 | 9.845 |
| 8 | 1.377 | 4.815 | 5.882 | 11.418 |

These short, closed-loop phases characterize this demo/configuration only.
Small sample sizes, shared-node scheduling, order, caches, and monitoring
load affect the results. There are no confidence intervals, sustained-load
capacity claims, availability SLOs, or extrapolations to third-party tools.

## Real pod loss and recovery

The workflow cordoned its own node briefly to hold replacement scheduling,
then deleted the backend pod while an already-running client observed it.
This is a controlled outage, not an unassisted recovery-time benchmark.

| Evidence | Captured outcome |
|----------|------------------|
| [Before](evidence/failure-before-status.yaml) | Current-generation `Ready=True`, `DeploymentAvailable` |
| [Unavailable](evidence/failure-unavailable-status.yaml) | `Ready=False`, `DeploymentUnavailable`, phase Pending |
| [Client](evidence/failure-client.log) | HTTP 503 through the authenticated gateway |
| [Scrape failure](evidence/failure-backend-down.json) | Prometheus backend `up=0` |
| [Recovered](evidence/failure-recovered-status.yaml) | Replacement pod restored `Ready=True` after scheduling resumed |
| [Scrape recovery](evidence/failure-backend-recovered.json) | Prometheus backend `up=1` |
| [Transcript](demo.txt) | A new SDK session discovered and executed the tool after recovery |

The condition timestamps were 18:49:40 UTC for unavailable and 18:49:47 UTC
for recovered, but that interval includes the deliberate scheduling hold.
Do not label it an automatic failover guarantee.

The [controller log](evidence/failure-controller.log),
[gateway log](evidence/failure-gateway.log), and gateway metric snapshots
[before](evidence/failure-before-gateway.prom),
[during](evidence/failure-unavailable-gateway.prom), and
[after](evidence/failure-recovered-gateway.prom) preserve the diagnostic trail.
Backend snapshots [before](evidence/failure-before-backend.prom) and
[after](evidence/failure-recovered-backend.prom) also show why counters must
not be subtracted across pod replacement.

## Separate transport and execution evidence

The initial SDK workflow produced multiple HTTP requests but only one
executed `read_runbook` handler. The e2e script asserts this distinction.
The final [backend metric snapshot](evidence/benchmark-backend.prom) records
1,196 streaming executions and 1,198 read executions: the benchmark accounts
for 1,196 of each, with two earlier post-recreation lifecycle reads.
The [gateway snapshot](evidence/benchmark-gateway.prom) counts HTTP exchanges,
including setup and control traffic, rather than inferring tool invocations.

The [observability contract](observability.md) defines metric names,
cardinality limits, aborted-stream accounting and session behavior. The
[architecture decisions](decisions.md) and [authentication boundary](authentication.md)
describe what this release does and deliberately does not guarantee.

## Per-tool policy evidence (after v0.1.0)

Captured from one `make e2e` run on 2026-11-11, step 4c. It is not part of
the v0.1.0 measurements above and has not been benchmarked.

The account `mcp-readonly-client` is allowed only `read_runbook`. The
gateway's [audit trail](evidence/tool-audit.jsonl) for the run holds three
events: one allowed read by the unrestricted demo client, then for the
restricted client one `allow` (`tool_listed`) for `read_runbook` and one
`deny` (`tool_not_listed`) for `stream_runbook`.

The [gateway decision counters](evidence/tool-decisions-gateway.prom) agree:
two allowed `read_runbook` decisions and one denied `stream_runbook`. The
[backend execution counters](evidence/tool-decisions-backend.prom) taken
immediately afterwards show two `read_runbook` executions and no
`stream_runbook` series at all, so the denied call was not executed. The
client reported the denial as JSON-RPC error `-32090` and then listed tools
on the same session; see step 4c of the [captured run](demo.txt).

The parser micro-benchmark (`go test ./internal/gateway -run '^$' -bench
InspectMCP -benchmem`) measured about 4 µs and 43 allocations per typical
`tools/call` body on the Apple M2 above. That is the parsing cost only, not
an end-to-end latency comparison.

## Agent scenario evidence (after v0.1.0)

Captured from one `make agent-demo` run on 2026-11-11, using the scripted
planner. No model was involved; see the [scenario](agent-scenario.md) for
what that does and does not show.

The [captured run](agent-demo.txt) shows the sandbox Deployment at 0 of 1
ready, the agent's five tool calls, the refusal of `scale_deployment`, and
the Deployment afterwards at 1 of 1 ready with one replica and a
`restarted-by` annotation naming the agent's ServiceAccount.

The [audit trail](evidence/incident-audit.jsonl) for the agent's identity
holds five events in order: allowed `read_runbook`, `get_deployment_status`,
`restart_deployment`, `get_deployment_status`, then denied
`scale_deployment`. The [decision counters](evidence/incident-decisions.prom)
for the ops server agree: two allowed status calls, one allowed restart, one
denied scale.

A second run the same day used a model instead of the script: `qwen2.5:7b`
served by Ollama on the host, through the Chat Completions compatible
planner. The [captured run](agent-demo-local.txt) and its
[audit trail](evidence/incident-audit-local-model.jsonl) show six tool calls:
four allowed, and two denied (`stream_runbook`, which the model chose
unprompted, and `scale_deployment`). The Deployment ended at 1 of 1 ready
with one replica. This is a single run of one small model.
