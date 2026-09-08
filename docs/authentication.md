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

Entries refer to client ServiceAccounts, not backend ServiceAccounts. Each
entry authorizes all endpoints on that one namespaced MCP server. There are
no wildcards or tool-level permissions. A legacy name-only route resolves
the exact server first and applies that server's policy; it cannot bypass
namespace authorization. Ambiguous legacy names still return HTTP 409.

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
| 403 | Verified ServiceAccount not allowed for the resolved server |
| 404 / 409 | Unknown server / ambiguous legacy server name |
| 429 | Verified agent exhausted its budget, or limiter identity capacity reached |
| 503 | TokenReview unavailable/overloaded, stale policy, or authorized backend unready |
| 502 | Connection to an authorized, cached-ready backend failed |

`tool_call` logs use the verified principal, or an empty agent for
authentication failures; tokens are never logged. Existing tool-call metrics
cover authenticated routed and rejected requests, not unauthenticated
attempts. Rate-limit counters retain the `agent` label, now containing the
verified username. Protocol-aware tool metrics and a broader cardinality
policy remain checkpoint 5 work. `/readyz` and `/metrics` remain
unauthenticated operational endpoints; keep them on a trusted network.
