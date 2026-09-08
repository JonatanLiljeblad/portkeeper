package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
)

// Backend is the router's snapshot of a server's identity, address, and readiness.
type Backend struct {
	Namespace              string
	Name                   string
	Address                string // "<service>.<namespace>.svc.cluster.local:<port>"
	Ready                  bool
	AllowedServiceAccounts []mcpv1alpha1.ServiceAccountReference
	PolicyObservedAt       time.Time
}

// Cached routing survives API errors, but stale authorization must fail closed.
const policyMaxAge = 15 * time.Second

func (b Backend) allows(principal string) bool {
	for _, account := range b.AllowedServiceAccounts {
		if principal == "system:serviceaccount:"+account.Namespace+":"+account.Name {
			return true
		}
	}
	return false
}

var (
	ErrServerNotFound  = errors.New("unknown MCP server")
	ErrAmbiguousServer = errors.New("server name exists in multiple namespaces; use /<namespace>/<server>/<endpoint>")
)

// Registry is the gateway's live view of MCPServer objects in the
// cluster. v0.1 deliberately keeps this simple: poll on an interval
// rather than a full watch/informer, since correctness matters more than
// latency for the MVP. Swapping in a real informer is a natural v0.2 step.
type Registry struct {
	mu       sync.RWMutex
	backends map[types.NamespacedName]Backend
	synced   bool

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
		backends:  make(map[types.NamespacedName]Backend),
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
	observedAt := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
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

	next := make(map[types.NamespacedName]Backend, len(list.Items))
	for _, item := range list.Items {
		addr := fmt.Sprintf("%s-svc.%s.svc.cluster.local:%d", item.Name, item.Namespace, item.Spec.Port)
		if hostOverride != "" {
			addr = fmt.Sprintf("%s:%d", hostOverride, item.Spec.Port)
		}
		next[types.NamespacedName{Namespace: item.Namespace, Name: item.Name}] = Backend{
			Namespace:              item.Namespace,
			Name:                   item.Name,
			Address:                addr,
			Ready:                  item.IsReady(),
			AllowedServiceAccounts: slices.Clone(item.Spec.AllowedServiceAccounts),
			PolicyObservedAt:       observedAt,
		}
	}

	r.mu.Lock()
	r.backends = next
	r.synced = true
	r.mu.Unlock()
}

// HasSynced reports whether the initial registry list succeeded. This is a
// startup readiness signal, not a guarantee of backend health or freshness.
func (r *Registry) HasSynced() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.synced
}

// Resolve uses exact namespaced identity, or accepts a legacy name only when
// it is unique across all registered servers, including unready ones.
func (r *Registry) Resolve(namespace, name string) (Backend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if namespace != "" {
		if b, ok := r.backends[types.NamespacedName{Namespace: namespace, Name: name}]; ok {
			return b, nil
		}
		return Backend{}, ErrServerNotFound
	}
	var match Backend
	found := false
	for key, b := range r.backends {
		if key.Name != name {
			continue
		}
		if found {
			return Backend{}, ErrAmbiguousServer
		}
		match, found = b, true
	}
	if !found {
		return Backend{}, ErrServerNotFound
	}
	return match, nil
}
