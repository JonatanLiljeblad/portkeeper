// Command ops-mcp-server serves the state-changing demo backend. It acts on
// Deployments in exactly one namespace, using the pod's own ServiceAccount.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/jonatan/portkeeper/internal/opsmcp"
)

func main() {
	addr := os.Getenv("OPS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9002"
	}
	namespace := os.Getenv("OPS_NAMESPACE")
	if namespace == "" {
		log.Fatal("OPS_NAMESPACE is required: the one namespace this server may act in")
	}
	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatalf("loading kube config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("creating Kubernetes client: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", opsmcp.NewHandler(client, namespace))
	server := &http.Server{
		Addr:              addr,
		Handler:           http.NewCrossOriginProtection().Handler(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("ops MCP server listening on %s for namespace %s", addr, namespace)
	log.Fatal(server.ListenAndServe())
}
