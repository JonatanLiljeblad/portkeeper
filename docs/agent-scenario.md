# Agent scenario: an incident worked under tool policy

An agent is given an incident and a runbook. The runbook tells it to restart
a Deployment and then scale it up. The gateway lets this agent restart but
not scale. The scenario shows what that looks like from the agent's side and
from the operator's side, on a real Deployment in a kind cluster.

The point is the gap between instruction and permission. The agent does what
its instructions say; the gateway decides what its identity may do. When the
two disagree, the call is refused before it reaches the backend, the agent
sees a failed tool call it can report, and the operator has a record.

## Run it

```bash
make agent-demo
```

This needs Docker, kind and kubectl, and no model or API key. It builds the
images, creates a disposable cluster, runs only this scenario, and deletes
the cluster. `make e2e` and CI run the same scenario as step 4d.

A [captured run](agent-demo.txt) shows the output.

## What is deployed

| Piece | Where | Role |
|-------|-------|------|
| `checkout` Deployment | `portkeeper-sandbox` | The workload with the incident |
| `runbooks` MCP server | `portkeeper-demo` | Serves the `checkout-not-ready` runbook |
| `ops` MCP server | `portkeeper-demo` | `get_deployment_status`, `restart_deployment`, `scale_deployment` |
| Agent pod | `portkeeper-demo` | Runs as ServiceAccount `mcp-incident-agent` |

`checkout` is a stand-in workload. It reuses the runbooks image because that
image takes its listen address from the environment: started with the wrong
address, the pod runs but fails its readiness probe. The script then corrects
the ConfigMap. Pods read it only at startup, so the fix needs a restart,
which is the incident.

The agent's permissions come from two `MCPServer` resources:

```yaml
# runbooks
- namespace: portkeeper-demo
  name: mcp-incident-agent
  allowedTools: [read_runbook]
# ops
- namespace: portkeeper-demo
  name: mcp-incident-agent
  allowedTools: [get_deployment_status, restart_deployment]
```

`scale_deployment` exists on the ops server and appears in tool discovery.
It is simply not in this identity's list.

## What happens

1. The agent discovers five tools across the two servers.
2. It reads the runbook, checks `checkout` (0 of 1 ready), and restarts it.
   The gateway allows each call.
3. It waits for the restart and sees 1 of 1 ready.
4. Following the runbook, it tries to scale to 3 replicas. The gateway
   answers with a JSON-RPC error; the ops server never receives the request.
5. The runbook says to stop when a step is refused. The agent reports what
   it did and what was refused.

The script then checks the result as an operator would, and fails the run if
any check does not hold:

- `checkout` is ready and still has 1 replica.
- The Deployment's `portkeeper.dev/restarted-by` annotation names the
  agent's verified ServiceAccount. The ops server writes the identity the
  gateway put in `X-Agent-ID`, not anything the agent claimed.
- The gateway audit trail has an allowed `restart_deployment` and a denied
  `scale_deployment` for that identity, and no allowed scale.

## Two ways to choose the steps

The agent (`internal/agent`) separates deciding from doing. A planner picks
the next tool calls; the runner executes them through the gateway.

**Script planner (default).** A fixed replay of the five calls above. It is
deterministic and free, which is why CI uses it. It proves the enforcement
path. It does not prove that a model would behave this way.

**Anthropic planner.** A language model reads the task and chooses the
calls, through the Anthropic Messages API:

```bash
AGENT_LLM_KEY_FILE=/path/outside/the/repo/key make agent-demo-live
```

This runs the scripted scenario first, restages the incident, and runs it
again with the model. `AGENT_MODEL` overrides the default,
`claude-sonnet-5-5`. A model is not deterministic: it may skip the scale-up,
in which case the run reports that no denial was recorded rather than
failing. The run does fail if the incident is left unresolved or if the
replica count changes.

**Status: the live path has not been run against the real API.** The planner
is tested against a local stand-in for the API (request shape, tool loop,
retry, key handling), and the scripted path is proven in kind. No recording
of a live run exists yet.

## Limits on the agent

- **Authority.** The agent has a gateway token and nothing else: no
  Kubernetes API token, no access to the backends except through the
  gateway. NetworkPolicy blocks it from reaching the ops server directly.
- **Steps.** At most 12 planner steps and 8 tool calls per step, inside a
  five-minute deadline. Tool calls are never retried.
- **Cost.** Each model call is capped at 1,024 output tokens and each tool
  result at 4 KiB before it is sent to the model. With the step limit this
  bounds a run to 12 model calls. The run prints its token usage. Actual
  cost per run has not been measured.
- **The key.** It is read from a file and never passed on a command line or
  printed. The script refuses a key file inside the repository, places it in
  a Secret in the disposable cluster, mounts it only into the agent pod, and
  deletes the Secret after the run even when `KEEP_CLUSTER=1`. Error
  messages from the model API are redacted for the key. Anyone with the
  cluster's kubeconfig can read the Secret while the run is in progress.
- **Prompt injection.** Tool output reaches the model. The system prompt
  tells it to treat that output as data, but a prompt is not a control. The
  control is the gateway: whatever the model is persuaded to try, it can
  only call the tools its identity is allowed.

## Limits on the ops server

The gateway is the first boundary, not the only one.

- The pod runs as ServiceAccount `ops-mcp`, whose Role allows `get` and
  `patch` on the one Deployment `checkout` in `portkeeper-sandbox`. It
  cannot list, delete, or touch anything else; the workflow asserts this
  with `kubectl auth can-i`.
- The server acts in one namespace, set when it starts, and only on
  Deployments labelled `portkeeper.dev/agent-managed=true`.
- Scale is limited to 1 through 5 replicas.

Because restart and scale are both a `patch`, Kubernetes RBAC cannot tell
them apart. An identity allowed to restart could scale if it reached the
backend directly. Tool-level policy at the gateway is what separates them,
which is why NetworkPolicy isolation of the backend matters.

## What this does not show

- The scripted run is a replay, not model behaviour.
- One scenario, one agent, one policy. It is a demonstration, not an
  evaluation of how reliably models respect refusals.
- The audit trail records decisions. An allowed event still does not prove
  the backend executed the call; here the Deployment annotation and state
  provide that evidence separately.
- The ops server has no execution metrics of its own.
