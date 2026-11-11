# Gateway identity and per-server policy

## Supported flow

Portkeeper supports Kubernetes ServiceAccount bearer tokens issued by the
same cluster the gateway uses for its registry. This is a workload-to-gateway
authentication flow, not an implementation of MCP OAuth discovery or an
interactive login flow. Clients must support supplying a bearer token on
every HTTP request.

Use short-lived, projected ServiceAccount tokens with audience `portkeeper`.
`GATEWAY_AUTH_AUDIENCE` changes that audience; client token projections must
match it. Do not reuse the Kubernetes API audience. The demo requests a
600-second token, and its HTTP transport rereads the token file for every
request so kubelet rotation is picked up without restarting the MCP client.

The gateway sends each token to Kubernetes `TokenReview` with the explicit
audience. It requires an authenticated response, the matching returned
audience, a nonempty user UID, and a valid
`system:serviceaccount:<namespace>:<name>` username. Kubernetes validates
signature, expiry and bound-object validity. The gateway does not cache
successful reviews. API failures fail closed with HTTP 503; reviews have a
three-second deadline and a maximum of 32 in flight per gateway process.
The Kubernetes client's TokenReview QPS/burst defaults remain 5/10, now
explicitly configurable using `GATEWAY_TOKEN_REVIEW_QPS` and
`GATEWAY_TOKEN_REVIEW_BURST`. These are API-client capacity settings, not
per-agent quotas; the benchmark documents its temporarily raised profile.
This intentionally makes API availability and TokenReview throughput part of
the request path, rather than introducing a revocation cache in this checkpoint.

`X-Agent-ID` is optional and untrusted. It cannot select an identity or reset
a quota. After authentication, the gateway overwrites it with the verified
ServiceAccount username for the backend. Both `Authorization` and
`Proxy-Authorization` are removed before proxying; gateway credentials must
never become backend credentials.

## Authorization and migration

Every server denies all clients unless its CR explicitly allows them:

```yaml
spec:
  allowedServiceAccounts:
    - namespace: portkeeper-demo
      name: mcp-demo-client
```

Entries refer to client ServiceAccounts, not backend ServiceAccounts. An
entry without `allowedTools` authorizes all endpoints and all tools on that
one namespaced MCP server. There are no wildcards. A legacy name-only route
resolves the exact server first and applies that server's policy; it cannot
bypass namespace authorization. Ambiguous legacy names still return HTTP 409.

## Per-tool authorization

An entry can narrow its account to named tools:

```yaml
spec:
  allowedServiceAccounts:
    - namespace: portkeeper-demo
      name: mcp-readonly-client
      allowedTools:
        - read_runbook
```

- **Omitted `allowedTools`** keeps the v0.1.0 meaning: every tool. Existing
  resources are unaffected by the upgrade. The API rejects an empty list;
  a gateway that nevertheless observes one permits no tool.
- **With `allowedTools`**, a `tools/call` naming any other tool is denied.
  Names match exactly and are case-sensitive. The account can reach only the
  server's `/mcp` endpoint; other endpoints return 403, because only the
  inspected MCP endpoint can enforce the list.
- **A denial is not an HTTP error.** The gateway answers HTTP 200 with a
  JSON-RPC error, code `-32090`, echoing the request id. An MCP client
  reports a failed call and keeps its session. The backend never receives
  the request.
- **Not governed:** `tools/list` is forwarded unfiltered, so a restricted
  client can see the names and descriptions of tools it cannot call. Other
  MCP methods (resources, prompts, completions) are not restricted by this
  list. Filtering discovery would require rewriting responses, which the
  gateway does not do.
- Every decision is written to the [audit trail](observability.md#tool-audit-trail).

To enforce this the gateway reads the request body of every POST to an
`/mcp` route, for all accounts. It reads only requests, never responses or
streams. The body must be one JSON-RPC message of at most 1 MiB
(`GATEWAY_MCP_MAX_REQUEST_BYTES`), delivered within ten seconds.

The check is only sound if the backend sees the same method and tool the
gateway saw, and JSON parsers disagree on ambiguous input. The gateway
therefore refuses, with HTTP 400, a body that repeats `method`, `params`,
`id`, `jsonrpc`, or the `name` inside `params`, or that spells one of them
with different case (Go's JSON decoder would match `Name` to `name`). It
also refuses JSON-RPC batches (removed from MCP in 2025-06-18), trailing
data, a `tools/call` without a string tool name or without an id, bodies
over the limit (413), and compressed bodies (415). These refusals apply to
every account, so they are an upgrade consideration for non-conforming
clients.

Policy changes follow the registry: a narrowed or widened list applies to
new requests within the five-second poll.

**Upgrade behavior:** existing clients that only send `X-Agent-ID` now receive
401, and existing CRs without `allowedServiceAccounts` deny authenticated
clients with 403. Install the updated CRD, add explicit policy, give the
gateway `create` access to `tokenreviews.authentication.k8s.io`, and provision
gateway-audience client tokens before switching clients.

Policy is refreshed with the registry every five seconds. Failed lists keep
the routing snapshot, but authorization fails closed after 15 seconds
without a successful fresh policy observation. Revocation is therefore
eventual, not instantaneous. Already-open streams are not reauthorized or
terminated on token expiry or policy changes; every subsequent HTTP request
is checked again. No tool calls are automatically retried.

Permissions attach to the logical ServiceAccount namespace/name. Deleting
and recreating that ServiceAccount retains its policy eligibility, but an old
token is subject to Kubernetes bound-object validation. Administrators who
can mint tokens for an allowed account, modify CR policy, or control the
gateway are trusted. Do not grant those capabilities to untrusted tenants.

## Credentials in each direction

Client tokens authenticate **to the gateway**. The existing `authType` and
`authSecretRef` fields reserve **gateway-to-backend credentials**; backend
token injection remains unimplemented and these fields are not read to
authenticate clients. This checkpoint does not give the gateway Secret-read
permission. A backend requiring its own token is not yet supported by this
flow.

The demo uses HTTP only within its disposable local cluster or a loopback
port-forward. Deploy behind a trusted TLS terminator and protect the
terminator-to-gateway hop before sending bearer tokens over an untrusted
network. The gateway does not provide TLS or OAuth metadata itself. Do not
publish tokens in logs, commands captured as transcripts, or committed files.

For host-side development, a cluster administrator can issue a short-lived
token with `kubectl create token <service-account> --audience=portkeeper
--duration=10m`. Store it in a mode-0600 temporary file and use the demo
client's `-token-file` option. Manually issued files are not rotated for you.
Do not use a kubeconfig bearer token or a default API-audience projection.

## Isolation boundary

Per-server authorization only protects traffic passing through the gateway.
The [Kubernetes demo](kubernetes-demo.md) installs a policy-enforcing CNI and
ingress NetworkPolicies for MCP-labeled backend pods in both demo namespaces,
allowing only gateway pods in `portkeeper-system`. It demonstrates denial
through both backend Service and Pod IP, alongside a successful gateway call
to the same healthy backend.

NetworkPolicy does not protect against trusted node/cluster administrators,
API-authorized port-forwarding, privileged workloads, gateway compromise, or
users able to change the relevant namespaces, policies and pod labels.
Namespace and workload creation permissions are part of this trust boundary.
For additional backend namespaces, install equivalent policy before deploying
backends. The legacy host-run toy setup is not a network-isolation demo.

## Quotas and failure responses

Rate limits and audit identity use the verified username, across all servers
and all HTTP requests (including MCP control messages). Defaults remain
five requests per second and burst ten. Configure a finite positive
`GATEWAY_RATE_LIMIT_RPS` and positive integer `GATEWAY_RATE_LIMIT_BURST`.

Each process keeps at most 10,000 limiter buckets. At capacity it removes
only buckets idle for at least 15 minutes whose budgets have fully refilled.
Otherwise new identities receive 429; active buckets are not evicted to make
room. Replicas have independent budgets and restart resets state. This is
not a global quota or a shared rate-limit service.

| HTTP status | Meaning |
|-------------|---------|
| 401 | Missing, invalid, expired, or wrong-audience token; includes a Bearer challenge |
| 403 | Verified ServiceAccount not allowed for the resolved server, or a tool-restricted account using a non-MCP endpoint |
| 200 + JSON-RPC error `-32090` | Verified, allowed account called a tool outside its `allowedTools` |
| 400 / 413 / 415 | MCP POST body that cannot be safely inspected: ambiguous or malformed, batch / too large / compressed |
| 404 / 409 | Unknown server / ambiguous legacy server name |
| 429 | Verified agent exhausted its budget, or limiter identity capacity reached |
| 503 | TokenReview unavailable/overloaded, stale policy, or authorized backend unready |
| 502 | Connection to an authorized, cached-ready backend failed |

`gateway_request` logs use the verified principal, or an empty agent for
authentication failures; tokens are never logged. Explicit HTTP metrics
also count unauthenticated attempts under a fixed aggregate label.
Rate-limit counters retain the `agent` label with bounded verified identities
and an overflow aggregate. Actual tool executions are measured in the backend;
see the [observability contract](observability.md). `/readyz` and `/metrics` remain
unauthenticated operational endpoints; keep them on a trusted network.
