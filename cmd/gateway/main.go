// Command gateway is the single entrypoint clients/agents talk to. It
// reads the live MCPServer registry from the Kubernetes API and proxies
// each request to the right backend, logging and recording metrics for
// every call.
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jonatan/portkeeper/internal/gateway"
)

func main() {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// Per-agent rate limit: sustained requests/sec plus burst capacity.
	rps := envFloat("GATEWAY_RATE_LIMIT_RPS", 5)
	burst := envInt("GATEWAY_RATE_LIMIT_BURST", 10)

	reg, err := gateway.NewRegistry()
	if err != nil {
		log.Fatalf("failed to start registry watcher: %v", err)
	}
	reg.Start()

	limiter := gateway.NewAgentLimiter(rps, burst)
	router := gateway.NewRouter(reg, limiter)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", router)

	log.Printf("gateway listening on %s (rate limit: %.3g req/s, burst %d per agent)", addr, rps, burst)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("gateway server error: %v", err)
	}
}

func envFloat(name string, def float64) float64 {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		log.Fatalf("invalid %s=%q: want a positive number", name, s)
	}
	return v
}

func envInt(name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 {
		log.Fatalf("invalid %s=%q: want a positive integer", name, s)
	}
	return v
}
