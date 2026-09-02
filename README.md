# portkeeper

A Kubernetes-native gateway and registry for MCP (Model Context Protocol) servers.

## Why this exists

Teams running multiple MCP servers today have no shared way to discover them,
authenticate calls to them, or see who's calling what. This project turns
"a pile of MCP servers" into a managed fleet — the same shift Kubernetes made
for containers.

- **Registry**: MCP servers are declared as a Kubernetes Custom Resource
  (`MCPServer`), so `kubectl get mcpservers` just works.
- **Gateway**: a single entrypoint that all clients/agents talk to. It looks
  up the live registry via the Kubernetes API and routes each request to the
  right backend.
- **Observability**: every proxied tool call is logged (and exported as a
  Prometheus metric) — server, tool, latency, status.

See [PLAN.md](./PLAN.md) for the full design rationale and roadmap, and
[docs/architecture.md](./docs/architecture.md) for how the pieces fit
together.

## Repo layout

```
cmd/
  controller/     # entrypoint for the MCPServer controller (operator)
  gateway/        # entrypoint for the gateway/proxy
api/v1alpha1/     # the MCPServer CRD Go types
internal/
  controller/     # reconcile loop: MCPServer -> Deployment + Service
  gateway/        # registry lookup, routing, proxying, metrics
config/
  crd/            # generated CRD YAML
  samples/        # example MCPServer objects
  rbac/           # controller's RBAC permissions
  manager/        # Deployment manifest for the controller itself
deploy/           # kind cluster config + gateway deployment manifest
docs/             # architecture notes
```

## Status

v0.1 (MVP) in progress — see PLAN.md for exactly what's in and out of scope.

## Local development

This is built to run on [`kind`](https://kind.sigs.k8s.io/) (Kubernetes-in-Docker)
so you don't need real cloud infra to iterate.

```bash
# 0. Build the toy demo MCP server image (used by config/samples/)
docker build -t toy-mcp-server:0.1 hack/toy-mcp-server

# 1. Spin up a local cluster and load the toy image into it
kind create cluster --config deploy/kind-config.yaml
kind load docker-image toy-mcp-server:0.1

# 2. Install the CRD
kubectl apply -f config/crd/bases/

# 3. Run the controller locally (against the kind cluster)
go run ./cmd/controller

# 4. Register a sample MCP server
kubectl apply -f config/samples/mcp_v1alpha1_mcpserver.yaml

# 5. In another terminal, make the backend reachable from the host and
#    run the gateway. (In-cluster the gateway uses cluster DNS; locally,
#    GATEWAY_BACKEND_HOST + a port-forward stand in for it.)
#    Optional: GATEWAY_RATE_LIMIT_RPS / GATEWAY_RATE_LIMIT_BURST tune the
#    per-agent rate limit (defaults: 5 req/s, burst 10).
kubectl port-forward svc/demo-server-svc 9000:9000 &
BACKEND_FORWARD_PID=$!
GATEWAY_BACKEND_HOST=localhost go run ./cmd/gateway

# 6. Call a tool through the gateway. X-Agent-ID identifies the calling
#    agent (self-reported, not auth) and is required — the per-agent rate
#    limiter and logs key off it.
curl -X POST -H 'X-Agent-ID: demo-agent' -d 'hello portkeeper' \
  http://localhost:8080/demo-server/echo
```

### Metrics dashboard (Prometheus + Grafana)

Optional, for watching the system work: a minimal in-cluster Prometheus +
Grafana that visualize the gateway's `/metrics` (call rate, latency
quantiles, error rate). No persistence, no auth — demo only.

With the demo above already running (gateway on the host at :8080):

```bash
# 1. Deploy Prometheus + Grafana into the kind cluster
kubectl apply -f deploy/prometheus.yaml -f deploy/grafana.yaml
kubectl rollout status deploy/prometheus deploy/grafana

# 2. Open Grafana (no login needed)
kubectl port-forward svc/grafana 3000:3000
# -> http://localhost:3000/d/portkeeper

# 3. Generate some traffic and watch the panels move
while true; do
  curl -s -X POST -H 'X-Agent-ID: demo-agent' -d hi \
    http://localhost:8080/demo-server/echo > /dev/null
  sleep 1
done
```

Local-dev wrinkle: the gateway runs on the *host* (`go run`), so in-cluster
Prometheus scrapes it at `host.docker.internal:8080` — a name Docker Desktop
resolves to the host from inside containers, which kind pods inherit via
CoreDNS. If you deploy the gateway in-cluster instead, point the scrape
config in `deploy/prometheus.yaml` at the gateway Service.

### Stop the local demo

Press `Ctrl-C` in the terminals running the controller, gateway, Grafana
port-forward, or traffic loop. Then stop the background backend port-forward
and delete the cluster:

```bash
# Run this in the shell where BACKEND_FORWARD_PID was set above.
kill "$BACKEND_FORWARD_PID"

# Deletes the kind cluster and everything deployed in it, including the
# demo server, Prometheus, and Grafana.
kind delete cluster --name kind
```

To keep the cluster but remove only the monitoring stack:

```bash
kubectl delete -f deploy/grafana.yaml -f deploy/prometheus.yaml
```

See `Makefile` for shortcuts to the above.
