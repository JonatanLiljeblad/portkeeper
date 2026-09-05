# Kubernetes MCP walkthrough

## Prerequisites

Use Docker with a running daemon, kind v0.33.0, kubectl v1.35 or v1.36,
and Bash. The demo pins the Kubernetes v1.35.8 kind node image by digest.
Allow approximately 4 GiB of Docker memory and outbound access for the
initial image and Go dependency downloads.

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

It creates a unique `portkeeper-e2e-<process-id>` cluster and a dedicated
kubeconfig under an ignored `artifacts/e2e.*/` directory. It does not use or
modify your normal kubeconfig. `E2E_CLUSTER_NAME` can select a name in the
`portkeeper-e2e` family, but the script refuses to reuse an existing cluster.

The captured walkthrough shows these stages:

1. Install the CRD, namespaces, controller, ServiceAccounts, and role bindings.
2. Apply `config/samples/mcp_v1alpha1_runbooks.yaml`. The controller creates
   an owned Deployment and Service in `portkeeper-demo`. Wait for the CR's
   current-generation Ready condition as well as the Deployment.
3. Wait for the backend Deployment, then start the gateway in
   `portkeeper-system`. Its readiness probe waits for its first successful
   registry list. Confirm that its service account can list MCPServers but
   cannot create them or read Secrets.
4. Run `deploy/demo-client-job.yaml`. The SDK client connects to
   `http://mcp-gateway.portkeeper-system.svc.cluster.local:8080/portkeeper-demo/runbooks/mcp`,
   discovers `read_runbook`, and reads `gateway-routing`.
5. Declare another `runbooks` server in `portkeeper-other`. Call it by
   namespace, observe 409 for the ambiguous old URL and 404 for a missing
   server.
6. Change the first server to a missing image, then an unreachable port.
   Observe `Ready=False` and 503, restore each change, and call the recovered
   backend.
7. Delete the managed Deployment and Service, confirm replacement UIDs,
   and call the recreated backend. Delete the second CR with foreground
   propagation and observe its children being garbage-collected. Its
   namespaced URL becomes 404; the remaining unique legacy URL works again.

The client Job has no Kubernetes API token, no automatic Job retry, and no
application-level tool-call retry. It uses the gateway Service, not direct
backend access or a host port-forward. A failed tool response causes a
nonzero exit rather than a success-shaped transcript.
Lifecycle client Pods also have no API token. The `-expect-status` mode
polls HTTP GET only, with a deadline, to wait for the registry's asynchronous
changes. It never retries a tool call. A routed GET without a session returns
400 from this stateful SDK backend; the demo uses that response before
starting a new SDK session after recovery.

See [demo.txt](demo.txt) for an actual captured run. It is a terminal
transcript, not a benchmark. Pod timing and Service IPs vary between runs.

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

# In another terminal:
go run ./hack/mcp-client \
  -endpoint http://127.0.0.1:8080/portkeeper-demo/runbooks/mcp \
  -agent-id local-demo \
  -topic rate-limiting
```

The SDK client's `-agent-id` sets `X-Agent-ID` on every request, including
initialization and notifications. `-timeout` bounds the full operation
(default `30s`). Other MCP clients must also support custom HTTP headers.
The identity remains self-reported, not authenticated.

Use the exact `kind delete cluster --name ... --kubeconfig ...` command printed
by the script to remove a retained cluster. Do not use `make kind-down` for
this workflow; that legacy target addresses the separate default kind demo.

## Diagnostics and CI

Every run retains logs under `artifacts/e2e.*/logs/`: image builds, cluster
events, pod status, controller, gateway, backend, client, and `demo.txt`.
On failure, the script prints the diagnostic location and the latest client
output/events before removing its cluster, unless `KEEP_CLUSTER=1` is set.

The GitHub Actions workflow runs Go checks before the same `make e2e` command
on Linux/amd64. It installs pinned kind/kubectl versions with checksum
verification and uploads only the logs directory, never the kubeconfig.
Local macOS/arm64 runs build native images and use the multi-architecture
kind node image.

## Current boundaries

Gateway readiness indicates initial registry synchronization, not backend
health or continuing API availability. Backend Ready conditions represent
current-generation Deployment availability and a successful TCP probe, not
MCP application semantics. Cache updates occur every five seconds; API
errors retain the previous snapshot.

The current controller and gateway can observe resources across namespaces,
and identical server names are isolated by their namespace. Legacy URLs
require a unique name. RBAC limits the gateway's
Kubernetes API access; it does not authenticate MCP clients or prevent
clients from reaching backend Services directly. Those are checkpoint 4.
