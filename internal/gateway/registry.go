package gateway

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
)

// Backend is what the router needs to proxy a request: where the server
// lives and which tools it claims to expose.
type Backend struct {
	Name    string
	Address string // "<service>.<namespace>.svc.cluster.local:<port>"
	Tools   []string
}

// Registry is the gateway's live view of MCPServer objects in the
// cluster. v0.1 deliberately keeps this simple: poll on an interval
// rather than a full watch/informer, since correctness matters more than
// latency for the MVP. Swapping in a real informer is a natural v0.2 step.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend // keyed by server name

	k8sClient client.Client
}

func NewRegistry() (*Registry, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kube config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := mcpv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating k8s client: %w", err)
	}

	return &Registry{
		backends:  make(map[string]Backend),
		k8sClient: c,
	}, nil
}

func restConfig() (*rest.Config, error) {
	// Uses in-cluster config when running as a pod, falls back to
	// ~/.kube/config for local `go run` against a kind cluster.
	return config.GetConfig()
}

// Start begins polling the MCPServer list on a fixed interval. Blocking
// work happens in a goroutine; callers get a non-blocking start.
func (r *Registry) Start() {
	go func() {
		ctx := context.Background()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		r.refresh(ctx) // populate immediately, don't wait for first tick
		for range ticker.C {
			r.refresh(ctx)
		}
	}()
}

func (r *Registry) refresh(ctx context.Context) {
	var list mcpv1alpha1.MCPServerList
	if err := r.k8sClient.List(ctx, &list); err != nil {
		log.Printf("registry: failed to list MCPServers: %v", err)
		return
	}

	// When the gateway runs inside the cluster, backends are reached via
	// cluster DNS. When running locally (`go run ./cmd/gateway`) those
	// names don't resolve from the host, so GATEWAY_BACKEND_HOST lets you
	// point at a host reachable address (e.g. "localhost" combined with
	// `kubectl port-forward svc/<name>-svc <port>:<port>`).
	hostOverride := os.Getenv("GATEWAY_BACKEND_HOST")

	next := make(map[string]Backend, len(list.Items))
	for _, item := range list.Items {
		addr := fmt.Sprintf("%s-svc.%s.svc.cluster.local:%d", item.Name, item.Namespace, item.Spec.Port)
		if hostOverride != "" {
			addr = fmt.Sprintf("%s:%d", hostOverride, item.Spec.Port)
		}
		next[item.Name] = Backend{
			Name:    item.Name,
			Address: addr,
			Tools:   item.Spec.Tools,
		}
	}

	r.mu.Lock()
	r.backends = next
	r.mu.Unlock()
}

// Lookup returns the backend that owns the given tool name, if any.
func (r *Registry) Lookup(tool string) (Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, b := range r.backends {
		for _, t := range b.Tools {
			if t == tool {
				return b, true
			}
		}
	}
	return Backend{}, false
}

// ByName returns a backend directly by MCPServer name, useful when a
// client addresses a server explicitly rather than by tool name.
func (r *Registry) ByName(name string) (Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.backends[name]
	return b, ok
}
