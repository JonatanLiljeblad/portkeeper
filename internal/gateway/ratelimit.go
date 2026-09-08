package gateway

import (
	"sync"
	"time"
)

// AgentLimiter enforces one global rate limit per agent ID (not per
// tool/server) using an in-memory token bucket per agent.
//
// KNOWN LIMITATION: state is local to this process. Running multiple
// gateway replicas gives each agent replicas × the intended limit, and
// restarts reset all buckets. Fixing that means shared state (e.g.
// Redis) — deliberately out of scope for now.
//
// At capacity, new identities are denied until an idle, fully replenished
// bucket can be evicted. Active buckets are never reset to make room.
type AgentLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	rate      float64 // tokens added per second
	burst     float64 // bucket capacity
	maxAgents int
	idleTTL   time.Duration
}

const maxLimiterAgents = 10000

type bucket struct {
	tokens float64
	last   time.Time
}

// NewAgentLimiter returns a limiter allowing `rps` requests per second
// with bursts up to `burst` per agent.
func NewAgentLimiter(rps float64, burst int) *AgentLimiter {
	return &AgentLimiter{
		buckets:   make(map[string]*bucket),
		rate:      rps,
		burst:     float64(burst),
		maxAgents: maxLimiterAgents,
		idleTTL:   15 * time.Minute,
	}
}

// Allow reports whether the given agent may make a request now, consuming
// one token if so.
func (l *AgentLimiter) Allow(agentID string) bool {
	return l.allowAt(agentID, time.Now())
}

func (l *AgentLimiter) allowAt(agentID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[agentID]
	if !ok {
		if len(l.buckets) >= l.maxAgents {
			for id, existing := range l.buckets {
				elapsed := now.Sub(existing.last)
				if elapsed >= l.idleTTL && existing.tokens+elapsed.Seconds()*l.rate >= l.burst {
					delete(l.buckets, id)
				}
			}
			if len(l.buckets) >= l.maxAgents {
				return false
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[agentID] = b
	}

	// Refill based on elapsed time, capped at burst.
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
