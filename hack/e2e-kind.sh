#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
root="$PWD"
cluster="${E2E_CLUSTER_NAME:-portkeeper-e2e-$$}"
keep="${KEEP_CLUSTER:-0}"
benchmark="${RUN_BENCHMARK:-0}"
node_image="kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"
calico_version="v3.32.0"
calico_sha256="bccabc607685551db918f66da724893eca3e69a50c5a3e3077029b02dbab8d35"

if [[ ! "$cluster" =~ ^portkeeper-e2e(-[a-z0-9-]+)?$ ]]; then
  echo "E2E_CLUSTER_NAME must be portkeeper-e2e or start with portkeeper-e2e-" >&2
  exit 1
fi
if [[ "$keep" != 0 && "$keep" != 1 ]]; then
  echo "KEEP_CLUSTER must be 0 or 1" >&2
  exit 1
fi
if [[ "$benchmark" != 0 && "$benchmark" != 1 ]]; then
  echo "RUN_BENCHMARK must be 0 or 1" >&2
  exit 1
fi
for tool in docker kind kubectl curl shasum; do
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

wait_ready() {
  local namespace name value generation
  namespace="$1"
  name="$2"
  value="$3"
  generation="$(k get -n "$namespace" mcpserver "$name" -o 'jsonpath={.metadata.generation}')"
  k wait -n "$namespace" --for="jsonpath={.status.observedGeneration}=$generation" "mcpserver/$name" --timeout=90s
  k wait -n "$namespace" --for="condition=Ready=$value" "mcpserver/$name" --timeout=120s
}

run_client() {
  local pod route namespace token_mode token_arg arg
  pod="$1"
  route="$2"
  shift 2
  namespace="${CLIENT_NAMESPACE:-portkeeper-demo}"
  token_mode="${CLIENT_TOKEN_MODE:-valid}"
  token_arg=""
  case "$token_mode" in
    valid) token_arg="-token-file=/var/run/portkeeper/token" ;;
    invalid) token_arg="-token-file=/var/run/portkeeper/invalid" ;;
    none) ;;
    *) echo "Unknown token mode: $token_mode" >&2; return 1 ;;
  esac
  if [[ "$route" == /* ]]; then
    route="http://mcp-gateway.portkeeper-system.svc.cluster.local:8080$route"
  fi
  {
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $namespace
  labels:
    app: mcp-demo-client
    mcp.portkeeper.dev/gateway: "${CLIENT_GATEWAY_LABEL:-false}"
  annotations:
    portkeeper.dev/invalid-token: deliberately-invalid
spec:
  restartPolicy: Never
  serviceAccountName: ${CLIENT_SA:-mcp-demo-client}
  automountServiceAccountToken: false
  activeDeadlineSeconds: 60
  securityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  volumes:
    - name: gateway-token
      projected:
        sources:
          - serviceAccountToken:
              path: token
              audience: ${CLIENT_AUDIENCE:-portkeeper}
              expirationSeconds: 600
          - downwardAPI:
              items:
                - path: invalid
                  fieldRef:
                    fieldPath: metadata.annotations['portkeeper.dev/invalid-token']
  containers:
    - name: client
      image: portkeeper/demo-client:e2e
      imagePullPolicy: IfNotPresent
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: gateway-token
          mountPath: /var/run/portkeeper
          readOnly: true
      args:
        - "-endpoint=$route"
        - "-timeout=45s"
EOF
    if [[ -n "$token_arg" ]]; then printf '        - "%s"\n' "$token_arg"; fi
    for arg in "$@"; do printf '        - "%s"\n' "$arg"; done
  } | k apply -f -
  if [[ "${CLIENT_START_ONLY:-0}" == 1 ]]; then
    k wait -n "$namespace" --for=condition=Ready "pod/$pod" --timeout=60s
    return
  fi
  if ! k wait -n "$namespace" --for=jsonpath='{.status.phase}'=Succeeded "pod/$pod" --timeout=75s; then
    k logs -n "$namespace" "$pod" | tee "$log_dir/$pod.log" >&2
    return 1
  fi
  k logs -n "$namespace" "$pod" | tee "$log_dir/$pod.log"
}

probe() {
  run_client "$1" "$2" "-expect-status=$3"
}

gateway_metrics() {
  k get --raw '/api/v1/namespaces/portkeeper-system/services/mcp-gateway:8080/proxy/metrics'
}

backend_metrics() {
  k get --raw '/api/v1/namespaces/portkeeper-demo/services/runbooks-svc:9001/proxy/metrics'
}

wait_scrape() {
  local job want response attempt
  job="$1"
  want="$2"
  for attempt in {1..30}; do
    response="$(k get --raw "/api/v1/namespaces/portkeeper-system/services/prometheus:9090/proxy/api/v1/query?query=up%7Bjob%3D%22$job%22%7D")"
    if grep -Eq "\"value\":\\[[^]]*,\"$want\"\\]" <<<"$response"; then
      printf '%s\n' "$response"
      return
    fi
    sleep 1
  done
  echo "Prometheus never observed up=$want for $job: $response" >&2
  return 1
}

failure_demo() (
  local node old_pod
  node="$(k get nodes -o 'jsonpath={.items[0].metadata.name}')"
  old_pod="$(k get pod -n portkeeper-demo -l mcp.portkeeper.dev/server=runbooks -o 'jsonpath={.items[0].metadata.name}')"
  k get -n portkeeper-demo mcpserver/runbooks -o yaml >"$log_dir/failure-before-status.yaml"
  gateway_metrics >"$log_dir/failure-before-gateway.prom"
  backend_metrics >"$log_dir/failure-before-backend.prom"
  # Keep a client running before the scheduling pause so it can observe downtime.
  CLIENT_START_ONLY=1 probe pod-loss-client /portkeeper-demo/runbooks/mcp 503
  trap 'status=$?; if ! k uncordon "$node"; then exit 1; fi; exit "$status"' EXIT
  k cordon "$node"
  k delete pod -n portkeeper-demo "$old_pod" --wait=true --timeout=30s
  wait_ready portkeeper-demo runbooks False
  k wait -n portkeeper-demo --for=jsonpath='{.status.phase}'=Succeeded pod/pod-loss-client --timeout=30s
  k logs -n portkeeper-demo pod/pod-loss-client | tee "$log_dir/failure-client.log"
  k get -n portkeeper-demo mcpserver/runbooks -o yaml >"$log_dir/failure-unavailable-status.yaml"
  k get -n portkeeper-demo pods -o wide >"$log_dir/failure-unavailable-pods.log"
  gateway_metrics >"$log_dir/failure-unavailable-gateway.prom"
  wait_scrape portkeeper-runbooks 0 >"$log_dir/failure-backend-down.json"
  k uncordon "$node"
  k rollout status -n portkeeper-demo deployment/runbooks --timeout=120s
  wait_ready portkeeper-demo runbooks True
  probe pod-loss-route /portkeeper-demo/runbooks/mcp 400
  run_client pod-loss-recovered /portkeeper-demo/runbooks/mcp
  wait_scrape portkeeper-runbooks 1 >"$log_dir/failure-backend-recovered.json"
  k get -n portkeeper-demo mcpserver/runbooks -o yaml >"$log_dir/failure-recovered-status.yaml"
  gateway_metrics >"$log_dir/failure-recovered-gateway.prom"
  backend_metrics >"$log_dir/failure-recovered-backend.prom"
  k logs -n portkeeper-system deployment/portkeeper-controller >"$log_dir/failure-controller.log"
  k logs -n portkeeper-system deployment/mcp-gateway >"$log_dir/failure-gateway.log"
  echo "Pod loss produced Ready=False and HTTP 503; a replacement pod restored Ready=True and MCP execution."
)

restore_benchmark() {
  k apply -f config/samples/mcp_v1alpha1_runbooks.yaml &&
    k set env -n portkeeper-system deployment/mcp-gateway \
      GATEWAY_RATE_LIMIT_RPS- GATEWAY_RATE_LIMIT_BURST- \
      GATEWAY_TOKEN_REVIEW_QPS- GATEWAY_TOKEN_REVIEW_BURST- &&
    k rollout status -n portkeeper-system deployment/mcp-gateway --timeout=120s &&
    wait_ready portkeeper-demo runbooks True
}

wait_for_benchmark() {
  local deadline conditions
  deadline=$((SECONDS + 600))
  while (( SECONDS < deadline )); do
    conditions="$(k get -n portkeeper-system job/mcp-benchmark \
      -o 'jsonpath={range .status.conditions[*]}{.type}={.status}{"\n"}{end}')" || return 1
    if grep -Eq '^(Failed|FailureTarget)=True$' <<<"$conditions"; then
      echo "Benchmark Job failed." >&2
      return 1
    fi
    if grep -Fxq 'Complete=True' <<<"$conditions"; then
      return 0
    fi
    sleep 2
  done
  echo "Timed out waiting for benchmark Job." >&2
  return 1
}

capture_benchmark_diagnostics() {
  k logs -n portkeeper-system deployment/mcp-gateway >"$log_dir/benchmark-gateway.log" 2>&1 ||
    echo "Benchmark gateway logs unavailable" >&2
  gateway_metrics >"$log_dir/benchmark-gateway.prom" ||
    echo "Benchmark gateway metrics unavailable" >&2
  backend_metrics >"$log_dir/benchmark-backend.prom" ||
    echo "Benchmark backend metrics unavailable" >&2
}

run_benchmark() (
  {
    date -u '+measured_at=%Y-%m-%dT%H:%M:%SZ'
    git rev-parse HEAD
    git diff --stat
    uname -srm
    if [[ "$(uname -s)" == Darwin ]]; then
      sysctl -n machdep.cpu.brand_string hw.memsize hw.logicalcpu
    else
      grep -m1 'model name' /proc/cpuinfo
      grep MemTotal /proc/meminfo
    fi
    docker info --format 'Docker {{.ServerVersion}}; CPUs={{.NCPU}}; memory_bytes={{.MemTotal}}; architecture={{.Architecture}}'
    echo "kind_node=$node_image; calico=$calico_version"
    echo 'Benchmark-only profile: per-agent RPS/burst=1000/1000; TokenReview client QPS/burst=1000/1000; max in-flight reviews=32'
  } >"$log_dir/benchmark-environment.txt"
  trap 'status=$?; capture_benchmark_diagnostics; if ! restore_benchmark; then echo "Failed to restore ordinary benchmark policy/limits" >&2; exit 1; fi; exit "$status"' EXIT
  k set env -n portkeeper-system deployment/mcp-gateway \
    GATEWAY_RATE_LIMIT_RPS=1000 GATEWAY_RATE_LIMIT_BURST=1000 \
    GATEWAY_TOKEN_REVIEW_QPS=1000 GATEWAY_TOKEN_REVIEW_BURST=1000
  k rollout status -n portkeeper-system deployment/mcp-gateway --timeout=120s
  k patch -n portkeeper-demo mcpserver runbooks --type=json \
    -p '[{"op":"add","path":"/spec/allowedServiceAccounts/-","value":{"namespace":"portkeeper-system","name":"mcp-benchmark-client"}}]'
  wait_ready portkeeper-demo runbooks True
  k apply -f deploy/benchmark-job.yaml
  CLIENT_NAMESPACE=portkeeper-system CLIENT_SA=mcp-benchmark-client \
    probe benchmark-policy-ready /portkeeper-demo/runbooks/mcp 400
  k get -n portkeeper-system deployment/mcp-gateway -o yaml >"$log_dir/benchmark-gateway-deployment.yaml"
  k get -n portkeeper-demo deployment/runbooks -o yaml >"$log_dir/benchmark-backend-deployment.yaml"
  k patch -n portkeeper-system job/mcp-benchmark --type=merge -p '{"spec":{"suspend":false}}'
  if ! wait_for_benchmark; then
    k logs -n portkeeper-system job/mcp-benchmark >"$log_dir/benchmark.json"
    echo "Benchmark failed; see $log_dir/benchmark.json" >&2
    return 1
  fi
  k logs -n portkeeper-system job/mcp-benchmark >"$log_dir/benchmark.json"
  echo "Raw benchmark report and environment captured in $log_dir."
)

collect_logs() {
  k get pods -A -o wide >"$log_dir/pods.log" 2>&1 || echo "Could not collect pod status" >&2
  k get events -A --sort-by=.metadata.creationTimestamp >"$log_dir/events.log" 2>&1 || echo "Could not collect events" >&2
  k logs -n portkeeper-system deployment/portkeeper-controller >"$log_dir/controller.log" 2>&1 || echo "Controller logs unavailable" >&2
  k logs -n portkeeper-system deployment/mcp-gateway >"$log_dir/gateway.log" 2>&1 || echo "Gateway logs unavailable" >&2
  k logs -n portkeeper-demo deployment/runbooks >"$log_dir/runbooks.log" 2>&1 || echo "Runbook logs unavailable" >&2
  k logs -n portkeeper-demo job/mcp-demo-client >"$log_dir/client.log" 2>&1 || echo "Demo client logs unavailable" >&2
  k logs -n kube-system daemonset/calico-node --all-containers --tail=150 >"$log_dir/calico.log" 2>&1 || echo "Calico logs unavailable" >&2
  k get networkpolicy -A >"$log_dir/network-policies.log" 2>&1 || echo "Network policies unavailable" >&2
  k logs -n portkeeper-system deployment/prometheus --tail=100 >"$log_dir/prometheus.log" 2>&1 || echo "Prometheus logs unavailable" >&2
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
  --config deploy/kind-e2e-config.yaml --image "$node_image"
# A failed creation must not make a pre-existing cluster eligible for cleanup.
created=1
curl --fail --location --silent --show-error --max-time 120 \
  "https://raw.githubusercontent.com/projectcalico/calico/$calico_version/manifests/calico.yaml" \
  -o "$work_dir/calico.yaml"
echo "$calico_sha256  $work_dir/calico.yaml" | shasum -a 256 --check
# Use VXLAN without BGP/IPIP in Docker's Linux VM, on both macOS and Linux.
sed -e 's/calico_backend: "bird"/calico_backend: "vxlan"/' \
  -e '/- name: CALICO_IPV4POOL_IPIP/{n;s/value: "Always"/value: "Never"/;}' \
  -e '/- name: CALICO_IPV4POOL_VXLAN/{n;s/value: "Never"/value: "Always"/;}' \
  -e '/- -bird-live/d' -e '/- -bird-ready/d' \
  "$work_dir/calico.yaml" >"$work_dir/calico-vxlan.yaml"
k create -f "$work_dir/calico-vxlan.yaml"
k rollout status -n kube-system daemonset/calico-node --timeout=240s
k rollout status -n kube-system deployment/calico-kube-controllers --timeout=180s
k wait --for=condition=Ready nodes --all --timeout=120s
k rollout status -n kube-system deployment/coredns --timeout=120s
kind load docker-image --name "$cluster" \
  portkeeper/controller:e2e portkeeper/gateway:e2e portkeeper/runbooks:e2e portkeeper/demo-client:e2e

demo() {
  echo "=== 1. Install the CRD, namespaces, and controller ==="
  k apply -f config/crd/bases/
  k wait --for=condition=Established crd/mcpservers.mcp.portkeeper.dev --timeout=60s
  k apply -f deploy/namespaces.yaml
  k apply -f deploy/backend-network-policy.yaml
  k apply -f config/rbac/role.yaml -f deploy/rbac.yaml -f deploy/controller-deployment.yaml
  k rollout status -n portkeeper-system deployment/portkeeper-controller --timeout=120s

  echo "=== 2. Declare a real MCP backend; wait for its Deployment ==="
  k apply -f config/samples/mcp_v1alpha1_runbooks.yaml
  k wait -n portkeeper-demo --for=create deployment/runbooks --timeout=90s
  k rollout status -n portkeeper-demo deployment/runbooks --timeout=120s
  wait_ready portkeeper-demo runbooks True
  k get -n portkeeper-demo mcpservers,deployments,services
  for resource in deployment/runbooks service/runbooks-svc; do
    if [[ "$(k get -n portkeeper-demo "$resource" -o 'jsonpath={.metadata.ownerReferences[0].kind}')" != MCPServer ]]; then
      echo "$resource is not owned by an MCPServer" >&2
      exit 1
    fi
  done

  echo "=== 3. Start the gateway; verify registry and TokenReview RBAC ==="
  k apply -f deploy/gateway-deployment.yaml
  k rollout status -n portkeeper-system deployment/mcp-gateway --timeout=120s
  gateway_identity="system:serviceaccount:portkeeper-system:mcp-gateway"
  k auth can-i list mcpservers --all-namespaces --as="$gateway_identity"
  k auth can-i create tokenreviews.authentication.k8s.io --as="$gateway_identity"
  for permission in "create mcpservers" "get secrets"; do
    read -r verb resource <<<"$permission"
    if [[ "$(k auth can-i "$verb" "$resource" --all-namespaces --as="$gateway_identity")" != no ]]; then
      echo "Gateway unexpectedly allowed to $permission" >&2
      exit 1
    fi
    echo "Gateway cannot $permission."
  done

  echo "=== 4. Run the SDK client inside Kubernetes through the gateway Service ==="
  # Deployment readiness can precede Service dataplane convergence.
  probe initial-route-ready /portkeeper-demo/runbooks/mcp 400
  k apply -f deploy/demo-client-job.yaml
  k wait -n portkeeper-demo --for=condition=complete job/mcp-demo-client --timeout=90s
  k logs -n portkeeper-demo job/mcp-demo-client | tee "$log_dir/client.log"
  grep -Fq 'Discovered tool: read_runbook' "$log_dir/client.log"
  grep -Fq '# Gateway routing' "$log_dir/client.log"
  grep -Fq 'MCP discovery and tool call completed.' "$log_dir/client.log"

  echo "=== 4a. Separate HTTP telemetry from executed MCP tools ==="
  k apply -k deploy/
  k rollout status -n portkeeper-system deployment/prometheus --timeout=180s
  k rollout status -n portkeeper-system deployment/grafana --timeout=180s
  wait_scrape portkeeper-gateway 1 >"$log_dir/gateway-scrape.json"
  wait_scrape portkeeper-runbooks 1 >"$log_dir/backend-scrape.json"
  gateway_metrics >"$log_dir/gateway-http.prom"
  backend_metrics >"$log_dir/backend-tools.prom"
  grep -q '^mcp_gateway_http_requests_total{' "$log_dir/gateway-http.prom"
  grep -q '^mcp_backend_tool_invocations_total{' "$log_dir/backend-tools.prom"
  grep -Eq '^mcp_backend_tool_invocations_total\{.*tool="read_runbook".*\} 1$' "$log_dir/backend-tools.prom"
  awk '/^mcp_gateway_http_requests_total.*server="runbooks"/ { requests += $NF } END { exit !(requests > 1) }' "$log_dir/gateway-http.prom"
  echo "One executed read_runbook invocation is distinct from multiple MCP transport requests."
  k get --raw '/api/v1/namespaces/portkeeper-system/services/grafana:3000/proxy/api/dashboards/uid/portkeeper' >"$log_dir/dashboard.json"
  grep -q mcp_backend_tool_invocations_total "$log_dir/dashboard.json"

  echo "=== 4b. Authenticate tokens and authorize namespaced ServiceAccounts ==="
  CLIENT_SA=mcp-denied-client probe denied-client /portkeeper-demo/runbooks/mcp 403
  CLIENT_TOKEN_MODE=none probe missing-token /portkeeper-demo/runbooks/mcp 401
  CLIENT_TOKEN_MODE=invalid probe invalid-token /portkeeper-demo/runbooks/mcp 401
  CLIENT_AUDIENCE=not-portkeeper probe wrong-audience /portkeeper-demo/runbooks/mcp 401
  CLIENT_SA=mcp-denied-client run_client spoofed-agent /portkeeper-demo/runbooks/mcp \
    -expect-status=403 -agent-id=system:serviceaccount:portkeeper-demo:mcp-demo-client
  CLIENT_SA=mcp-denied-client probe denied-legacy /runbooks/mcp 403
  k patch -n portkeeper-demo mcpserver runbooks --type=merge -p '{"spec":{"allowedServiceAccounts":[]}}'
  probe deny-by-default /portkeeper-demo/runbooks/mcp 403
  probe deny-by-default-legacy /runbooks/mcp 403
  k apply -f config/samples/mcp_v1alpha1_runbooks.yaml
  wait_ready portkeeper-demo runbooks True
  probe restored-allowlist /portkeeper-demo/runbooks/mcp 400

  echo "=== 5. Isolate same-name backends in different namespaces ==="
  k apply -f config/samples/mcp_v1alpha1_runbooks_other.yaml
  wait_ready portkeeper-other runbooks True
  # Stateful MCP GET without a session returns 400 once the backend is routed.
  probe other-ready /portkeeper-other/runbooks/mcp 400
  run_client other-client /portkeeper-other/runbooks/mcp
  CLIENT_SA=mcp-denied-client probe denied-other /portkeeper-other/runbooks/mcp 403
  probe ambiguous-name /runbooks/mcp 409
  probe missing-server /portkeeper-demo/missing/mcp 404
  CLIENT_TOKEN_MODE=none probe missing-token-legacy /runbooks/mcp 401

  echo "=== 5a. Prove healthy Service/PodIP isolation in both backend namespaces ==="
  for namespace in portkeeper-demo portkeeper-other; do
    k rollout status -n "$namespace" deployment/runbooks --timeout=60s
    pod_ip="$(k get pod -n "$namespace" -l mcp.portkeeper.dev/server=runbooks -o 'jsonpath={.items[0].status.podIP}')"
    test -n "$pod_ip"
    service="http://runbooks-svc.$namespace.svc.cluster.local:9001/mcp"
    pod_endpoint="http://$pod_ip:9001/mcp"
    for target in service pod; do
      endpoint="$service"
      if [[ "$target" == pod ]]; then endpoint="$pod_endpoint"; fi
      # Positive HTTP control proves this exact address is healthy before/after
      # the negative test, not merely a connectable unrelated TCP listener.
      CLIENT_NAMESPACE=portkeeper-system CLIENT_SA=default CLIENT_GATEWAY_LABEL=true CLIENT_TOKEN_MODE=none \
        run_client "$namespace-$target-before" "$endpoint" -expect-status=400
      CLIENT_TOKEN_MODE=none run_client "$namespace-$target-blocked" "$endpoint" -expect-network=blocked
      CLIENT_NAMESPACE=portkeeper-system CLIENT_SA=default CLIENT_GATEWAY_LABEL=true CLIENT_TOKEN_MODE=none \
        run_client "$namespace-$target-after" "$endpoint" -expect-status=400
    done
    # A gateway label alone or membership of its namespace alone is insufficient.
    CLIENT_GATEWAY_LABEL=true CLIENT_TOKEN_MODE=none \
      run_client "$namespace-label-only" "$pod_endpoint" -expect-network=blocked
    CLIENT_NAMESPACE=portkeeper-system CLIENT_SA=default CLIENT_TOKEN_MODE=none \
      run_client "$namespace-namespace-only" "$pod_endpoint" -expect-network=blocked
    run_client "$namespace-gateway-allowed" "/$namespace/runbooks/mcp"
  done

  echo "=== 6. Reject stale readiness during image and port updates, then recover ==="
  k patch -n portkeeper-demo mcpserver runbooks --type=merge -p '{"spec":{"image":"portkeeper/runbooks:missing"}}'
  wait_ready portkeeper-demo runbooks False
  probe unavailable-image /portkeeper-demo/runbooks/mcp 503
  k patch -n portkeeper-demo mcpserver runbooks --type=merge -p '{"spec":{"image":"portkeeper/runbooks:e2e"}}'
  wait_ready portkeeper-demo runbooks True
  k patch -n portkeeper-demo mcpserver runbooks --type=merge -p '{"spec":{"port":9002}}'
  wait_ready portkeeper-demo runbooks False
  probe unavailable-port /portkeeper-demo/runbooks/mcp 503
  k patch -n portkeeper-demo mcpserver runbooks --type=merge -p '{"spec":{"port":9001}}'
  wait_ready portkeeper-demo runbooks True
  probe recovered-route /portkeeper-demo/runbooks/mcp 400
  run_client recovered-client /portkeeper-demo/runbooks/mcp

  echo "=== 6a. Delete a backend pod; capture downtime, conditions, metrics and recovery ==="
  failure_demo

  echo "=== 7. Recreate owned resources and garbage-collect deleted servers ==="
  old_deployment="$(k get -n portkeeper-demo deployment/runbooks -o 'jsonpath={.metadata.uid}')"
  old_service="$(k get -n portkeeper-demo service/runbooks-svc -o 'jsonpath={.metadata.uid}')"
  k delete -n portkeeper-demo deployment/runbooks service/runbooks-svc --wait=true
  k wait -n portkeeper-demo --for=create deployment/runbooks --timeout=90s
  k wait -n portkeeper-demo --for=create service/runbooks-svc --timeout=90s
  k rollout status -n portkeeper-demo deployment/runbooks --timeout=120s
  wait_ready portkeeper-demo runbooks True
  test "$(k get -n portkeeper-demo deployment/runbooks -o 'jsonpath={.metadata.uid}')" != "$old_deployment"
  test "$(k get -n portkeeper-demo service/runbooks-svc -o 'jsonpath={.metadata.uid}')" != "$old_service"
  probe recreated-route /portkeeper-demo/runbooks/mcp 400
  run_client recreated-client /portkeeper-demo/runbooks/mcp
  k delete -n portkeeper-other mcpserver/runbooks --cascade=foreground --wait=true --timeout=90s
  k wait -n portkeeper-other --for=delete deployment/runbooks service/runbooks-svc --timeout=90s
  probe deleted-route /portkeeper-other/runbooks/mcp 404
  probe legacy-unique /runbooks/mcp 400
  run_client legacy-client /runbooks/mcp
  if [[ "$benchmark" == 1 ]]; then
    echo "=== 8. Measure direct and authenticated gateway MCP traffic ==="
    run_benchmark
  fi
  echo "=== Kubernetes MCP workflow completed ==="
}

demo | tee "$log_dir/demo.txt"
