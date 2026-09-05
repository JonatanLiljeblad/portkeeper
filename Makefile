.PHONY: tidy build test manifests generate toy-image kind-up kind-down install-crds run-controller run-gateway run-runbook-server apply-sample monitoring-up grafana-forward

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

tidy:
	go mod tidy

build:
	go build ./...

test:
	go test -race ./...

# Regenerate config/crd/bases from the Go types in api/v1alpha1.
manifests:
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd/bases

# Regenerate api/v1alpha1/zz_generated.deepcopy.go.
generate:
	$(CONTROLLER_GEN) object paths=./api/...

toy-image:
	docker build -t toy-mcp-server:0.1 hack/toy-mcp-server

kind-up:
	kind create cluster --config deploy/kind-config.yaml

kind-down:
	kind delete cluster

install-crds:
	kubectl apply -f config/crd/bases/

run-controller:
	go run ./cmd/controller

run-gateway:
	go run ./cmd/gateway

run-runbook-server:
	go run ./hack/runbook-mcp-server

apply-sample:
	kubectl apply -f config/samples/mcp_v1alpha1_mcpserver.yaml

# Prometheus + Grafana for the local demo (see README "Metrics dashboard").
monitoring-up:
	kubectl apply -f deploy/prometheus.yaml -f deploy/grafana.yaml

grafana-forward:
	kubectl port-forward svc/grafana 3000:3000
