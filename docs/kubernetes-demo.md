# Kubernetes MCP walkthrough

## Prerequisites

Use Docker with a running daemon, kind v0.33.0, kubectl v1.35 or v1.36,
and Bash, curl, and shasum. The demo pins the Kubernetes v1.35.8 kind node
image by digest and the Calico v3.32.0 installation manifest by SHA-256.
Allow at least 4 GiB of Docker memory and outbound access for the initial
images, Calico manifest, and Go dependency downloads. Docker Desktop on
macOS runs the Linux kind nodes and Calico inside its Linux VM; no host
macOS network-policy implementation or Helm installation is needed.

`make e2e` builds the Go binaries inside Docker; a host Go installation is
only needed for `make build`, `make test`, or the optional host-run client.
The root module and build image use Go 1.25.

## Run the complete workflow

```bash
make e2e
```

The script builds non-root controller, gateway, runbook-server, and SDK-client
images from the root Dockerfile and loads them directly into kind. No image
registry credentials are required.

Unlike the legacy toy cluster, `deploy/kind-e2e-config.yaml` disables kind's
default CNI (which does not enforce NetworkPolicy). The script installs
Calico, configured for VXLAN without BGP/IPIP, and waits for Calico, nodes,
and CoreDNS before deploying workloads. It fails rather than silently
falling back to unenforced policies. The legacy `deploy/kind-config.yaml`
is unchanged and is **not** sufficient for this isolation demonstration.
References: [Calico on kind][calico-kind], [Calico requirements][calico-requirements],
and the pinned release's [Kubernetes 1.35 build/test metadata][calico-metadata]
and [installation manifest][calico-manifest].

It creates a unique `portkeeper-e2e-<process-id>` cluster and a dedicated
kubeconfig under an ignored `artifacts/e2e.*/` directory. It does not use or
modify your normal kubeconfig. `E2E_CLUSTER_NAME` can select a name in the
`portkeeper-e2e` family, but the script refuses to reuse an existing cluster.

The captured walkthrough shows these stages:

1. Install the enforcing CNI, CRD, namespaces, backend NetworkPolicies,
   controller, ServiceAccounts, and role bindings.
2. Apply `config/samples/mcp_v1alpha1_runbooks.yaml`. The controller creates
   an owned Deployment and Service in `portkeeper-demo`. Wait for the CR's
   current-generation Ready condition as well as the Deployment.
3. Wait for the backend Deployment, then start the gateway in
   `portkeeper-system`. Its readiness probe waits for its first successful
   registry list. Confirm that its service account can list MCPServers and
   create `tokenreviews.authentication.k8s.io`, but cannot create MCPServers
   or read Secrets.
4. Run `deploy/demo-client-job.yaml`. The SDK client connects to
   `http://mcp-gateway.portkeeper-system.svc.cluster.local:8080/portkeeper-demo/runbooks/mcp`,
   authenticates with its projected ServiceAccount token, discovers
   `read_runbook`, and reads `gateway-routing`. Verify 403 for a denied
   ServiceAccount, 401 for missing/invalid/wrong-audience tokens, and 403
   when the denied identity spoofs `X-Agent-ID`. Remove the allowlist and
   verify deny-by-default on both namespaced and unique legacy URLs.
5. Declare another `runbooks` server in `portkeeper-other`. Call it by
   namespace, observe 409 for the ambiguous old URL and 404 for a missing
   server. Verify denied identities cannot access the other namespace.
   In both backend namespaces, verify direct Service DNS and PodIP access
   times out from client Pods, while gateway access works.
6. Change the first server to a missing image, then an unreachable port.
   Observe `Ready=False` and 503, restore each change, and call the recovered
   backend.
7. Delete the managed Deployment and Service, confirm replacement UIDs,
   and call the recreated backend. Delete the second CR with foreground
   propagation and observe its children being garbage-collected. Its
   namespaced URL becomes 404; the remaining unique legacy URL works again.

The client Job uses `serviceAccountName: mcp-demo-client` with
`automountServiceAccountToken: false`. Its explicitly projected token has
audience `portkeeper` and `expirationSeconds: 600`; it is not a default
Kubernetes API credential. The denied test account is `mcp-denied-client`.
Neither client account has a Kubernetes RBAC role binding.
There is no automatic Job retry or application-level tool-call retry.
The SDK workflow uses the gateway Service, not direct backend access or a
host port-forward. A failed tool response produces a nonzero exit.
Lifecycle Pods use the same projection. The `-expect-status` mode
polls HTTP GET only, with a deadline, to wait for the registry's asynchronous
changes. It never retries a tool call. A routed GET without a session returns
400 from this stateful SDK backend; the demo uses that response before
starting a new SDK session after recovery.

The network checks resolve DNS before dialing each resolved IP and accept
only TCP dial timeouts as evidence of blocking. DNS failures, connection
refusals, exhausted overall deadlines, and successful connections fail the
negative test. Immediately before and after each negative Service/PodIP
check, an administrator-created control Pod with the gateway's label in
`portkeeper-system` verifies HTTP 400 from that **exact healthy MCP target**.
Real SDK calls through the actual gateway also succeed. Additional probes
prove that the gateway label in the wrong namespace, and the right namespace
without the gateway label, are both blocked. These controls prevent an
unready/nonexistent backend or broken DNS from producing a false success.

Each run captures its current transcript under `artifacts/e2e.*/logs/demo.txt`.
The [captured successful run](demo.txt) includes checkpoint 4 authentication,
authorization, enforced network isolation, and the existing lifecycle workflow.

## Authentication and authorization contract

Every routed HTTP request needs `Authorization: Bearer <token>`.
The gateway performs Kubernetes TokenReview on every request, requiring the
`portkeeper` audience (`GATEWAY_AUTH_AUDIENCE` defaults to `portkeeper`).
The authenticated identity is the verified namespaced ServiceAccount;
`X-Agent-ID` is ignored/overwritten and cannot grant access. The incoming
gateway bearer credential is stripped before proxying to the backend.
`Proxy-Authorization` is stripped too. TokenReview has a three-second timeout
and a maximum of 32 concurrent reviews; timeout, API failure, or saturation
fails closed with 503 rather than reusing a cached authentication result.

Each MCPServer explicitly grants access:

```yaml
spec:
  allowedServiceAccounts:
    - namespace: portkeeper-demo
      name: mcp-demo-client
```

An absent or empty allowlist denies everyone. Both runbook samples allow this
same identity, including when it accesses `portkeeper-other`. Unique legacy
URLs enforce the same authorization; duplicate server names remain ambiguous.
`authType` and `authSecretRef` are reserved, unimplemented **backend credential**
metadata, not client authentication controls.

This is Kubernetes workload-token authentication, **not MCP OAuth discovery
or an OAuth authorization server**. Generic desktop MCP clients must support
the required bearer header and credential refresh; merely supporting MCP
does not supply a Kubernetes identity.

The kubelet rotates projected tokens. The demo SDK transport reopens
`-token-file` for **every HTTP request**, following projection replacement
rather than caching token contents. No token is printed by the client or
placed in command-line flags, manifests, or logs. It refuses all redirects,
including same-origin redirects, so credentials cannot be forwarded to a
redirect destination. SDK runs require a token file; status probes may omit
it for unauthenticated tests. Tokens are checked when HTTP requests begin,
not continuously during an already-open stream.

### Expected failures

| Outcome | Meaning |
| --- | --- |
| 401 | Missing, malformed, expired, invalid, or wrong-audience bearer token |
| 403 | Verified identity is not in the selected server's allowlist |
| 409 | Legacy name matches multiple registered namespaces |
| 404 | Authenticated request addresses an absent server |
| 503 | Backend unready, TokenReview unavailable/saturated, or authorization snapshot older than 15 seconds |
| 429 | Per-identity rate limit exceeded or the local identity limiter is at capacity |
| 502 | Gateway cannot connect to the selected backend |
| Direct-backend TCP timeout | Expected NetworkPolicy denial, only with healthy-target controls |

Fix TokenReview RBAC/API availability rather than bypassing authentication
when review fails. Registry polling remains asynchronous; status probes
wait for changed allowlists/routes instead of retrying a tool invocation.

## Inspect the deployment or use a host client

```bash
KEEP_CLUSTER=1 make e2e

# Use the exact kubeconfig path printed by the script.
export KUBECONFIG=/absolute/path/printed/by/the/script/kubeconfig
kubectl get mcpservers -A
kubectl get deploy,svc -n portkeeper-demo
kubectl logs -n portkeeper-system deploy/mcp-gateway

# In a separate terminal using that kubeconfig:
kubectl port-forward -n portkeeper-system svc/mcp-gateway 8080:8080

# In another terminal, create a short-lived host-test token in an ignored
# project directory, never on stdout. This requires TokenRequest permission.
mkdir -p artifacts/host-client
chmod 700 artifacts/host-client
(
  umask 077
  kubectl create token mcp-demo-client -n portkeeper-demo \
    --audience=portkeeper --duration=10m > artifacts/host-client/token
)
go run ./hack/mcp-client \
  -endpoint http://127.0.0.1:8080/portkeeper-demo/runbooks/mcp \
  -token-file artifacts/host-client/token \
  -topic rate-limiting
rm -f artifacts/host-client/token
```

The host-test token is not kubelet-rotated; recreate it when it expires.
Use a kubelet-projected volume for in-cluster clients.
`-agent-id` is optional and only useful for demonstrating that spoofed
`X-Agent-ID` values do not override the authenticated identity.
`-timeout` bounds the complete operation (default `30s`).
`-expect-network=allowed|blocked` performs a credential-free TCP probe;
never interpret `blocked` as enforcement without independent health controls.

Use the exact `kind delete cluster --name ... --kubeconfig ...` command printed
by the script to remove a retained cluster. Do not use `make kind-down` for
this workflow; that legacy target addresses the separate default kind demo.

## Diagnostics and CI

Every run retains logs under `artifacts/e2e.*/logs/`: image builds, cluster
events, pod status, controller, gateway, backend, Calico, policies, client,
and `demo.txt`.
On failure, the script prints the diagnostic location and the latest client
output/events before removing its cluster, unless `KEEP_CLUSTER=1` is set.

The GitHub Actions workflow runs Go checks before the same `make e2e` command
on Linux/amd64. It installs pinned kind/kubectl versions with checksum
verification and uploads only the logs directory, never kubeconfigs or
token files. The e2e script separately verifies the pinned Calico manifest.
Local macOS/arm64 runs build native images and use the multi-architecture
kind node image.

## Current boundaries

Gateway readiness indicates initial registry synchronization, not backend
health or continuing API availability. Backend Ready conditions represent
current-generation Deployment availability and a successful TCP probe, not
MCP application semantics. Cache updates occur every five seconds; API
errors retain the previous snapshot.
Authorization fails closed with 503 when that snapshot is older than
15 seconds, so retained stale policy cannot continue granting access.
Rate limiting is replica-local and tracks at most 10,000 identities. At
capacity, new identities receive 429 unless an entry has been idle for at
least 15 minutes and its bucket is fully refilled, making it eligible for
eviction.

The current controller and gateway can observe resources across namespaces,
and identical server names are isolated by namespace. Legacy URLs require
a unique name.

`deploy/backend-network-policy.yaml` isolates ingress to **every Pod with
the `mcp.portkeeper.dev/server` label** in `portkeeper-demo` and
`portkeeper-other`, irrespective of server name. Its single allowed peer
combines the `mcp.portkeeper.dev/gateway: "true"` Pod label **AND** the
`portkeeper-system` namespace. This network-identity label is separate from
the gateway Service's `app: mcp-gateway` selector, so positive-control Pods
never become gateway Service endpoints.
New backend namespaces need equivalent policies. This is ingress isolation,
not backend egress control or a general multi-tenant Kubernetes security layer.
Other NetworkPolicies are additive and may broaden access.

Cluster/node administrators and workload deployers are trusted. A party able
to deploy gateway-labeled Pods in the system namespace, alter backend labels
or policies, impersonate ServiceAccounts, use host networking, or access
node/port-forward interfaces can bypass these boundaries. The e2e's
administrator-created positive control Pods intentionally exercise that
trusted-deployer capability. ServiceAccount authentication is not a substitute
for controlling who can create Pods using that account.

This isolated local demo uses HTTP, including gateway-to-backend traffic.
NetworkPolicy is not encryption. Use TLS at externally accessible gateway
endpoints before transmitting bearer credentials and protect internal
transport appropriately for the deployment's threat model. Do not expose this
HTTP demo or its demo-only monitoring to untrusted networks.

[calico-kind]: https://docs.tigera.io/calico/latest/getting-started/kubernetes/kind
[calico-requirements]: https://docs.tigera.io/calico/latest/getting-started/kubernetes/requirements
[calico-metadata]: https://github.com/projectcalico/calico/blob/v3.32.0/metadata.mk
[calico-manifest]: https://github.com/projectcalico/calico/blob/v3.32.0/manifests/calico.yaml
