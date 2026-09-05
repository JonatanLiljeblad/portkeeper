package gateway

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type listClient struct {
	client.Client
	err error
}

func (c *listClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return c.err
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
