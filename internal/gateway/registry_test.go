package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
)

type listClient struct {
	client.Client
	err   error
	items []mcpv1alpha1.MCPServer
}

func (c *listClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if c.err != nil {
		return c.err
	}
	out, ok := list.(*mcpv1alpha1.MCPServerList)
	if !ok {
		return fmt.Errorf("unexpected list type %T", list)
	}
	out.Items = c.items
	return nil
}

func TestRegistryNamespacedLifecycle(t *testing.T) {
	t.Setenv("GATEWAY_BACKEND_HOST", "")
	ready := mcpv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "shared", Generation: 2},
		Spec:       mcpv1alpha1.MCPServerSpec{Port: 9001},
		Status: mcpv1alpha1.MCPServerStatus{
			ObservedGeneration: 2,
			Conditions: []metav1.Condition{{
				Type: mcpv1alpha1.MCPServerReady, Status: metav1.ConditionTrue, ObservedGeneration: 2,
			}},
		},
	}
	stale := *ready.DeepCopy()
	stale.Namespace = "team-b"
	stale.Generation = 3
	c := &listClient{items: []mcpv1alpha1.MCPServer{ready, stale}}
	reg := &Registry{k8sClient: c}
	reg.refresh(t.Context())
	b, err := reg.Resolve("team-a", "shared")
	if err != nil || !b.Ready || b.Address != "shared-svc.team-a.svc.cluster.local:9001" {
		t.Fatalf("ready backend = %+v, error = %v", b, err)
	}
	b, err = reg.Resolve("team-b", "shared")
	if err != nil || b.Ready {
		t.Fatalf("stale backend = %+v, error = %v", b, err)
	}
	if _, err := reg.Resolve("", "shared"); !errors.Is(err, ErrAmbiguousServer) {
		t.Fatalf("legacy resolution = %v, want ambiguity even with one unready backend", err)
	}

	c.err = errors.New("API unavailable")
	reg.refresh(t.Context())
	if _, err := reg.Resolve("", "shared"); !errors.Is(err, ErrAmbiguousServer) {
		t.Fatal("failed refresh lost existing namespace identities")
	}
	c.err = nil
	c.items = []mcpv1alpha1.MCPServer{ready}
	reg.refresh(t.Context())
	b, err = reg.Resolve("", "shared")
	if err != nil || b.Namespace != "team-a" {
		t.Fatalf("unique legacy backend = %+v, error = %v", b, err)
	}

	t.Setenv("GATEWAY_BACKEND_HOST", "localhost")
	reg.refresh(t.Context())
	b, err = reg.Resolve("team-a", "shared")
	if err != nil || b.Address != "localhost:9001" {
		t.Fatalf("host override = %+v, error = %v", b, err)
	}
	c.items = nil
	reg.refresh(t.Context())
	if _, err := reg.Resolve("team-a", "shared"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("deleted backend remained: %v", err)
	}
}

func TestRegistryHasSynced(t *testing.T) {
	c := &listClient{err: errors.New("API unavailable")}
	reg := &Registry{k8sClient: c}
	if reg.HasSynced() {
		t.Fatal("registry was ready before its first list")
	}
	reg.refresh(t.Context())
	if reg.HasSynced() {
		t.Fatal("failed list marked registry ready")
	}

	c.err = nil
	reg.refresh(t.Context())
	if !reg.HasSynced() {
		t.Fatal("successful empty list did not mark registry ready")
	}

	c.err = errors.New("API unavailable again")
	reg.refresh(t.Context())
	if !reg.HasSynced() {
		t.Fatal("transient list failure reset the initial-sync signal")
	}
}

func TestRegistryAuthorizationUpdates(t *testing.T) {
	item := mcpv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "demo"},
		Spec:       mcpv1alpha1.MCPServerSpec{Port: 9001, AllowedServiceAccounts: []mcpv1alpha1.ServiceAccountReference{{Namespace: "test", Name: "agent"}}},
	}
	c := &listClient{items: []mcpv1alpha1.MCPServer{item}}
	reg := &Registry{k8sClient: c}
	reg.refresh(t.Context())
	b, err := reg.Resolve("test", "demo")
	if err != nil || !b.allows(testPrincipal) || time.Since(b.PolicyObservedAt) > policyMaxAge {
		t.Fatalf("initial policy=%+v, error=%v", b, err)
	}
	c.items[0].Spec.AllowedServiceAccounts[0].Name = "different"
	if !b.allows(testPrincipal) {
		t.Fatal("API list mutation changed published snapshot")
	}
	c.err = errors.New("API unavailable")
	reg.refresh(t.Context())
	retained, err := reg.Resolve("test", "demo")
	if err != nil || !retained.PolicyObservedAt.Equal(b.PolicyObservedAt) {
		t.Fatal("failed refresh extended authorization freshness")
	}
	c.err = nil
	reg.refresh(t.Context())
	b, err = reg.Resolve("", "demo")
	if err != nil || b.allows(testPrincipal) {
		t.Fatal("revoked account retained access through legacy route")
	}
}
