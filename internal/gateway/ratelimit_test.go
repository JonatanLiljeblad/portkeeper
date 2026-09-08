package gateway

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLimiterCapacityAndSafeEviction(t *testing.T) {
	now := time.Now()
	l := NewAgentLimiter(1, 2)
	l.maxAgents = 2
	if !l.allowAt("a", now) || !l.allowAt("a", now) || l.allowAt("a", now) {
		t.Fatal("burst not enforced")
	}
	if !l.allowAt("b", now) || l.allowAt("c", now) || len(l.buckets) != 2 {
		t.Fatal("capacity not enforced")
	}
	if l.allowAt("c", now.Add(time.Second)) {
		t.Fatal("active bucket evicted")
	}
	if !l.allowAt("c", now.Add(l.idleTTL)) || len(l.buckets) != 1 {
		t.Fatal("idle, refilled buckets not evicted")
	}
}

func TestLimiterDoesNotResetDepletedIdleBucket(t *testing.T) {
	now := time.Now()
	l := NewAgentLimiter(0.000001, 1)
	l.maxAgents = 1
	if !l.allowAt("a", now) || l.allowAt("b", now.Add(l.idleTTL)) {
		t.Fatal("depleted bucket evicted before it could refill")
	}
	if l.allowAt("a", now.Add(l.idleTTL)) {
		t.Fatal("depleted bucket was reset")
	}
}

func TestLimiterConcurrentCapacity(t *testing.T) {
	l := NewAgentLimiter(1, 1)
	l.maxAgents = 10
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Allow(fmt.Sprintf("agent-%d", i))
		}()
	}
	wg.Wait()
	if len(l.buckets) != l.maxAgents {
		t.Fatalf("bucket count=%d", len(l.buckets))
	}
}
