# Experimental v0.1.0 benchmark methodology

These measurements characterize the read-only demo and its deployed
authentication path, not production capacity or a latency guarantee. No
performance values are asserted here. Publish the actual JSON report, the exact
configuration, and environment evidence together.

## Reproduce the client workload

The existing `mcp-client` binary supports:

```sh
mcp-client -benchmark \
  -direct-endpoint http://runbooks-svc.portkeeper-demo.svc.cluster.local:9001/mcp \
  -endpoint http://mcp-gateway.portkeeper-system.svc.cluster.local:8080/portkeeper-demo/runbooks/mcp \
  -token-file /var/run/portkeeper/token \
  -concurrency=1,4,8 -calls-per-worker=20 -warmup=3 \
  -repetitions=2 -topic=gateway-routing -timeout=10m
```

Use the actual gateway Service name and projected token mount from the deployed
Job if they differ. `-benchmark`, `-expect-status`, and `-expect-network` are
mutually exclusive. The default timeout is ten minutes in benchmark mode and
thirty seconds in the existing demo/probe modes.

The Go entry point is
`democlient.Benchmark(ctx, BenchmarkConfig, io.Writer) error`. Configuration
fields are `DirectEndpoint`, `GatewayEndpoint`, `Credentials`, `Concurrency`,
`CallsPerWorker`, `WarmupPerWorker`, `Repetitions`, and `Topic`. Zero values
select the defaults shown above, except endpoints and the gateway token file,
which are required. Warm-up cannot be disabled. Bounds are eight distinct
concurrency values in 1–64, 1–1,000 measured calls per worker, 1–100 warm-up calls,
1–10 repetitions, and at most 100,000 measured calls across the whole run.

For the kind measurement profile:

- Run the client in `portkeeper-system`, using the `mcp-benchmark-client` ServiceAccount
  and a projected token with audience `portkeeper`.
- Give its Pod both `app=mcp-benchmark` and
  `mcp.portkeeper.dev/gateway=true`. The latter intentionally grants the benchmark
  the network identity needed to reach the backend directly. It is **not** an
  application authentication identity. The Pod must not match the gateway
  Service's application selector.
- Temporarily authorize that ServiceAccount on the runbooks registration.
  Temporarily use gateway per-agent limits of 1,000 requests/second and burst
  1,000, and Kubernetes TokenReview client limits of 1,000 QPS and burst 1,000.
  Record these measurement-only settings; restore the normal configuration
  afterward. Default security/rate-limit settings are not changed by the client.
- Use the established monitoring network identity to scrape backend `/metrics`.
  Do not add an unauthenticated network-policy exception for the benchmark.

The gateway leg uses the real projected bearer token and the deployed
TokenReview authorization path. It includes authentication overhead; it is **not**
a proxy-only comparison. The direct leg sends neither `Authorization` nor
`X-Agent-ID`, even when gateway credentials are configured. The gateway-leg client reopens
the token file on every HTTP request to follow rotation. Neither leg follows
redirects. Keep the network-policy and ServiceAccount grants explicit when
interpreting this deliberately privileged measurement client.

## Work and ordering

Each worker creates a real official Go SDK MCP session, completes SDK connection
negotiation, and discovers the tool before any worker starts warm-up. SDK 1.7 may
probe `server/discover` and fall back to legacy `initialize`; these are setup,
not tool invocations. Each phase has fresh sessions, one per worker, with at most
one outstanding tool call per worker. There is no fixed offered requests/second:
this is a closed-loop, concurrency-controlled workload with no think time.

The two tools are measured in separate phases:

- `read_runbook`: reads the embedded allowlisted document and returns its content.
- `stream_runbook`: reads the same document, sends three SDK progress notifications
  over the tool call's POST SSE response, and returns the complete document.
  Progress increases from 1 to 3. The backend waits 50 ms after each notification,
  including the last: approximately 150 ms of intentional execution delay.
  It respects request cancellation and does not read arbitrary filesystem paths.

For each concurrency value, the client measures `read_runbook` then
`stream_runbook`. Each repetition executes the direct and gateway phases
sequentially. Odd repetitions use direct then gateway; even repetitions reverse
that order. This reduces simple order bias but does not eliminate scheduling,
cache, CPU-frequency, thermal, or cluster effects. Direct and gateway phases are
not simultaneous or statistically paired requests. The fixed demo delay can
dominate streaming completion latency and hide small transport differences.

Workers perform their warm-up calls concurrently. A second barrier waits for all
warm-ups to finish before releasing the measured workers. SDK setup, discovery,
warm-up, and session close are excluded from measured call and wall durations.
The phase wall interval includes measured-worker dispatch, the measured calls,
and waiting for their completion, but excludes aggregation/report serialization.
The caller's context bounds the run, including HTTP shutdown; an additional
30-second HTTP exchange timeout prevents individual hung requests.

With the default configuration there are 24 phase measurements, 2,080 measured
calls, 312 additional warm-up calls, and 104 SDK sessions. A phase at concurrency
`N` contains `20*N` measured calls and `3*N` warm-up calls. Initialization,
discovery, and session close can generate additional HTTP exchanges.

SDK standalone GET SSE is disabled, reconnect retries are disabled with
`MaxRetries: -1`, and automatic multi-round-trip tool resubmission is disabled.
There are no application-level tool retries. Rejections count as errors, not
as omitted or retried samples.

## Latency, streaming, and output

Standard output is exactly one JSON object (`schema_version: 1`) for a valid
configuration, even when measurements fail. Configuration validation failures
return an error before a report exists. The CLI exits nonzero after emitting the
report if any initialization, warm-up, tool call, stream validation, or close
failed, or if the deadline prevented planned calls. It does not print tokens,
credential paths, endpoint URLs, or raw upstream/SDK error strings in the report.

Top-level fields:

- `successful`, `config`, `go_version`, `goos`, `goarch`, and `num_cpu`;
- `measurements`: ordered phase records;
- `overhead`: included **only if the entire benchmark succeeded**.

Each phase identifies `target` (`direct` or `gateway`), `tool`, `concurrency`,
and one-based `repetition`, and reports:

- `planned_calls`, `attempts`, `successes`, `errors`, and `error_categories`;
- separate `initialization_errors`, `close_errors`, `warmup_attempts`, and
  `warmup_errors`. These maps count failures by category, not tool samples.
  If any worker fails initialization/discovery, that phase makes no tool calls.
- `wall_seconds`, `attempts_per_second`, and `successes_per_second`, with
  throughput equal to the respective count divided by the measured wall interval;
- `completion_latency_seconds`, covering all attempted calls, including failures;
  `success_latency_seconds`, covering only successes; and
  `first_progress_latency_seconds`, covering only successful validated streams;
- raw `samples`, with zero-based `worker` and `call`, `duration_seconds`,
  optional `first_progress_seconds`, `progress_updates`, and `error_category`.
  Samples are grouped by worker, then call, not by completion order.

Each latency summary contains `samples`, `p50`, `p95`, and `p99`. All duration
values are seconds, not milliseconds. Percentiles use the nearest-rank method:
sort the selected samples ascending and take index `ceil(p*N)-1`. Empty sets
have count and values zero; zero is not evidence of zero latency. Raw samples
allow independent recomputation, including distinguishing fast rejections from
successful work.

Invocation completion duration begins immediately before SDK `CallTool` and ends
when it returns. It includes request serialization, token-file access on the
gateway leg, network/proxy/authentication work, backend execution, and result
decoding. It is not backend-only execution duration.

Streaming calls explicitly set a unique progress token and match callbacks to
that invocation. First-progress duration ends when the client receives the first
SDK progress callback, **not** on HTTP headers or the final result. Validation
requires all three updates and the first callback at least 25 ms before
completion. A missing update fails as `stream_missing_progress`; an update
replayed too close to completion fails as `stream_late_progress`. This margin
checks that the deliberately delayed demo is not fully buffered. It is not a
general-purpose definition of valid MCP streaming and can fail under severe
client scheduling stalls.

Other finite categories include `http_401`, `http_403`, `http_429`, `http_5xx`,
`http_other`, `deadline`, `cancelled`, `transport_timeout`, `transport`,
`protocol_or_transport`, `missing_tool`, `tool_error`, `input_required`, and
`invalid_content`. Raw error messages are intentionally not reported. HTTP
DELETE 405 is permitted by MCP and is not treated as a close failure; other
rejected close responses are errors. Calls never started after cancellation are
visible as `attempts < planned_calls`, not fabricated error samples.

Overhead records match each successful direct/gateway pair by tool, concurrency,
and repetition. `completion_delta_seconds` and `first_progress_delta_seconds`
contain gateway minus direct **summary quantiles**, not quantiles of paired
per-request differences. `p50_completion_percent` is
`100 * (gateway_p50-direct_p50) / direct_p50`. Negative deltas are retained.
There are no confidence intervals, significance claims, or extrapolations.

## Relate the report to backend and gateway metrics

The runbook server exposes `/metrics` on the same listener as `/mcp` (normally
port 9001). Production registers:

- `mcp_backend_tool_invocations_total{tool,outcome}`;
- `mcp_backend_tool_duration_seconds{tool,outcome}` (Prometheus histogram).

`tool` is one of the two registered names and `outcome` is `success`, `error`,
or `cancelled`. Metrics are updated once on handler completion. Topic, input,
agent, and session values are never metric labels. Typed SDK schema/protocol
validation failures before handler entry do **not** count as executed tools.
Initialization/discovery/close are not tool executions. The histogram measures
handler duration, excluding gateway work and SDK validation/serialization.

On an otherwise idle backend, a fully successful default benchmark produces
2,392 handler completions including warm-up, split equally between the two tools.
Use counter deltas and actual report counts, not this expected value, if there
is other traffic or any failure. Backend counters include both direct and
gateway legs. Gateway HTTP counters include protocol/setup traffic and do not
reveal JSON-RPC invocation counts; the gateway remains transparent.

`runbookmcp.NewHandler()` retains its `http.Handler` API and uses an isolated
registry. `NewObservedHandler(prometheus.Registerer)` exposes the same MCP
behavior while registering execution metrics with the supplied registry; use a
fresh registry in tests or the default registry once in production.

## Evidence and limitations

Archive the exact report and client command, source revision/image identity,
cluster/Kubernetes versions, gateway configuration, and backend replica count.
Record actual host CPU model/core counts, memory, operating system, Docker CPU
and memory allocation, node placement, and any resource limits separately.
`runtime.NumCPU` is the client's runtime-visible CPU count, not a statement of
physical host hardware or usable container CPU quota.

Run on a quiet, identified cluster; record monitoring and other competing
workloads. Repeat independent runs rather than presenting one p99 from a tiny
sample as representative. Network-hop differences, Kubernetes networking,
TokenReview/API-server load, token-file I/O, connection reuse, and client
scheduling are all part of this comparison. This is an experimental baseline
for these two demo tools, not a security-isolation proof, proxy-only benchmark,
multi-replica scaling test, or production performance claim.
