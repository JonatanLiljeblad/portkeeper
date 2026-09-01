# Project plan

## What this is

A Kubernetes controller + gateway that turns "MCP servers scattered across a
cluster" into a single, secure, observable fleet — the same shift Kubernetes
made for containers.

**Who it's for:** platform/infra teams running multiple MCP servers for
internal AI agents.

**Why it's worth building now:** MCP is barely a year old and its ecosystem
norms are still forming. No one has "won" the infra layer the way Kubernetes
won container orchestration. A working version of this — even modest — sits
ahead of where the market's tooling actually is.

## Design principles

1. **K8s-native, not K8s-adjacent.** The registry is a CRD (`MCPServer`), not
   a bolted-on database. This means it fits existing RBAC/GitOps workflows
   and `kubectl` just works against it.
2. **Gateway as the only door in.** Clients/agents never talk to MCP servers
   directly. Everything routes through the gateway — this is what makes
   central auth, rate limiting, and observability possible at all.
3. **Boring, provable core first.** v0.1 proves the control loop (declare →
   deploy → discover → route). That loop working end-to-end is the hard,
   credible part. Rate limiting and dashboards are easy to add later; a shaky
   core undermines everything.
4. **Observability from day one.** Even a single structured log line per
   tool call. This is one of the most resume-worthy angles of the whole
   project — cutting it now loses the strongest demo.

## Why this stack

| Piece | Choice | Why |
|---|---|---|
| Controller/operator | Go + controller-runtime (kubebuilder) | The idiomatic way K8s controllers are built — what Kubernetes itself and most CNCF projects use. |
| CRD schema | `MCPServer` custom resource | Native discovery via the K8s API, no separate registry to keep in sync. |
| Gateway/proxy | Go (net/http + a lightweight router) | Shares types/language with the controller; one less context switch. |
| Auth | Simple token-based to start | Prove routing works before adding auth complexity. |
| Observability | Structured logs + Prometheus metrics | Prometheus is the de facto K8s-world standard. |
| Local dev | kind (Kubernetes-in-Docker) | Fast iteration with no cloud cost. |

## v0.1 MVP scope

**In scope:**
- `MCPServer` CRD (image, port, exposed tools/metadata, auth type)
- Controller: watches `MCPServer` objects, creates/manages the matching
  Deployment + Service
- Gateway: reads the live `MCPServer` registry from the K8s API, proxies
  incoming requests to the right backend by name/path
- One structured log line per proxied tool call (tool, server, latency,
  status)
- End-to-end demo on `kind`: register a toy MCP server, call a tool through
  the gateway, see it routed and logged

**Explicitly out of scope for v0.1:**
- Auto-scaling
- Rate limiting / quotas
- OAuth or per-agent identity
- Any UI/dashboard
- Multi-cluster support

The MVP's only job: prove that declaring a server results in it being
deployed, discovered, and routable — with visibility into what happened.
Everything else is a v0.2+ feature added once that loop is solid.

## After MVP (roughly in order of resume-value per effort)

1. Prometheus dashboard (cheap, great screenshot)
2. Rate limiting per agent
3. OAuth-based auth
4. Auto-scaling based on call volume
5. Minimal web UI for browsing the registry
