#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
root="$PWD"
cluster="${E2E_CLUSTER_NAME:-portkeeper-e2e-$$}"
keep="${KEEP_CLUSTER:-0}"
node_image="kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"

if [[ ! "$cluster" =~ ^portkeeper-e2e(-[a-z0-9-]+)?$ ]]; then
  echo "E2E_CLUSTER_NAME must be portkeeper-e2e or start with portkeeper-e2e-" >&2
  exit 1
fi
if [[ "$keep" != 0 && "$keep" != 1 ]]; then
  echo "KEEP_CLUSTER must be 0 or 1" >&2
  exit 1
fi
for tool in docker kind kubectl; do
  if ! command -v "$tool" >/dev/null; then
    echo "Missing prerequisite: $tool" >&2
    exit 1
  fi
done
docker info >/dev/null
clusters="$(kind get clusters)"
if grep -Fxq "$cluster" <<<"$clusters"; then
  echo "Refusing to reuse or delete existing cluster: $cluster" >&2
  exit 1
fi

mkdir -p "$root/artifacts"
work_dir="$(mktemp -d "$root/artifacts/e2e.XXXXXX")"
log_dir="$work_dir/logs"
mkdir "$log_dir"
kubeconfig="$work_dir/kubeconfig"
created=0

k() {
  kubectl --kubeconfig "$kubeconfig" --context "kind-$cluster" "$@"
}

collect_logs() {
  k get pods -A -o wide >"$log_dir/pods.log" 2>&1 || echo "Could not collect pod status" >&2
  k get events -A --sort-by=.metadata.creationTimestamp >"$log_dir/events.log" 2>&1 || echo "Could not collect events" >&2
  k logs -n portkeeper-system deployment/portkeeper-controller >"$log_dir/controller.log" 2>&1 || echo "Controller logs unavailable" >&2
  k logs -n portkeeper-system deployment/mcp-gateway >"$log_dir/gateway.log" 2>&1 || echo "Gateway logs unavailable" >&2
  k logs -n portkeeper-demo deployment/runbooks >"$log_dir/runbooks.log" 2>&1 || echo "Runbook logs unavailable" >&2
  k logs -n portkeeper-demo job/mcp-demo-client >"$log_dir/client.log" 2>&1 || echo "Demo client logs unavailable" >&2
}

cleanup() {
  status=$?
  trap - EXIT
  if [[ "$created" == 1 ]]; then
    collect_logs
    if [[ "$status" != 0 ]]; then
      echo "Workflow failed. Diagnostics: $log_dir" >&2
      tail -n 30 "$log_dir/client.log" "$log_dir/events.log" >&2
    fi
    if [[ "$keep" == 1 ]]; then
      echo "Kept cluster $cluster. Use: export KUBECONFIG=$kubeconfig"
      echo "Cleanup: kind delete cluster --name $cluster --kubeconfig $kubeconfig"
    else
      if kind delete cluster --name "$cluster" --kubeconfig "$kubeconfig"; then
        rm -f "$kubeconfig"
      else
        echo "Failed to delete demo cluster $cluster" >&2
        status=1
      fi
    fi
  fi
  echo "Artifacts: $log_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "Building local images (build logs: $log_dir)"
for component in controller gateway runbooks demo-client; do
  echo "Building portkeeper/$component:e2e"
  if ! docker build --target "$component" -t "portkeeper/$component:e2e" . >"$log_dir/build-$component.log" 2>&1; then
    tail -n 60 "$log_dir/build-$component.log" >&2
    exit 1
  fi
done

kind create cluster --name "$cluster" --kubeconfig "$kubeconfig" \
  --config deploy/kind-config.yaml --image "$node_image" --wait 120s
# A failed creation must not make a pre-existing cluster eligible for cleanup.
created=1
kind load docker-image --name "$cluster" \
  portkeeper/controller:e2e portkeeper/gateway:e2e portkeeper/runbooks:e2e portkeeper/demo-client:e2e

demo() {
  echo "=== 1. Install the CRD, namespaces, and controller ==="
  k apply -f config/crd/bases/
  k wait --for=condition=Established crd/mcpservers.mcp.portkeeper.dev --timeout=60s
  k apply -f deploy/namespaces.yaml
  k apply -f config/rbac/role.yaml -f deploy/rbac.yaml -f deploy/controller-deployment.yaml
  k rollout status -n portkeeper-system deployment/portkeeper-controller --timeout=120s

  echo "=== 2. Declare a real MCP backend; wait for its Deployment ==="
  k apply -f config/samples/mcp_v1alpha1_runbooks.yaml
  k wait -n portkeeper-demo --for=create deployment/runbooks --timeout=90s
  k rollout status -n portkeeper-demo deployment/runbooks --timeout=120s
  k get -n portkeeper-demo mcpservers,deployments,services
  for resource in deployment/runbooks service/runbooks-svc; do
    if [[ "$(k get -n portkeeper-demo "$resource" -o 'jsonpath={.metadata.ownerReferences[0].kind}')" != MCPServer ]]; then
      echo "$resource is not owned by an MCPServer" >&2
      exit 1
    fi
  done

  echo "=== 3. Start the gateway and verify its read-only registry access ==="
  k apply -f deploy/gateway-deployment.yaml
  k rollout status -n portkeeper-system deployment/mcp-gateway --timeout=120s
  gateway_identity="system:serviceaccount:portkeeper-system:mcp-gateway"
  k auth can-i list mcpservers --all-namespaces --as="$gateway_identity"
  for permission in "create mcpservers" "get secrets"; do
    read -r verb resource <<<"$permission"
    if [[ "$(k auth can-i "$verb" "$resource" --all-namespaces --as="$gateway_identity")" != no ]]; then
      echo "Gateway unexpectedly allowed to $permission" >&2
      exit 1
    fi
    echo "Gateway cannot $permission."
  done

  echo "=== 4. Run the SDK client inside Kubernetes through the gateway Service ==="
  k apply -f deploy/demo-client-job.yaml
  k wait -n portkeeper-demo --for=condition=complete job/mcp-demo-client --timeout=90s
  k logs -n portkeeper-demo job/mcp-demo-client | tee "$log_dir/client.log"
  grep -Fq 'Discovered tool: read_runbook' "$log_dir/client.log"
  grep -Fq '# Gateway routing' "$log_dir/client.log"
  grep -Fq 'MCP discovery and tool call completed.' "$log_dir/client.log"
  echo "=== Kubernetes MCP workflow completed ==="
}

demo | tee "$log_dir/demo.txt"
