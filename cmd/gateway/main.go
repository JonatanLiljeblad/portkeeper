// Command gateway is the single entrypoint clients/agents talk to. It
// reads the live MCPServer registry from the Kubernetes API and proxies
// each request to the right backend, logging and recording metrics for
// every call.
package main

import (
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	authenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

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
	audience := os.Getenv("GATEWAY_AUTH_AUDIENCE")
	if audience == "" {
		audience = "portkeeper"
	}
	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatalf("loading authentication kube config: %v", err)
	}
	cfg.QPS = envFloat32("GATEWAY_TOKEN_REVIEW_QPS", 5)
	cfg.Burst = envInt("GATEWAY_TOKEN_REVIEW_BURST", 10)
	reviews, err := authenticationv1.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("creating authentication client: %v", err)
	}
	auth, err := gateway.NewServiceAccountAuthenticator(reviews.TokenReviews(), audience)
	if err != nil {
		log.Fatal(err)
	}

	reg, err := gateway.NewRegistry()
	if err != nil {
		log.Fatalf("failed to start registry watcher: %v", err)
	}
	reg.Start()

	limiter := gateway.NewAgentLimiter(rps, burst)
	router := gateway.NewRouter(reg, limiter, auth)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !reg.HasSynced() {
			http.Error(w, "waiting for initial registry sync", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", router)

	log.Printf("gateway listening on %s (rate limit: %.3g req/s, burst %d per agent)", addr, rps, burst)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("gateway server error: %v", err)
	}
}

func envFloat32(name string, def float64) float32 {
	v := envFloat(name, def)
	if v > math.MaxFloat32 || float32(v) == 0 {
		log.Fatalf("%s must fit in a positive float32", name)
	}
	return float32(v)
}

func envFloat(name string, def float64) float64 {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
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
