package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonatan/portkeeper/internal/democlient"
	"github.com/jonatan/portkeeper/internal/runbookmcp"
)

func TestBenchmarkCLIFailurePreservesJSON(t *testing.T) {
	if os.Getenv("PORTKEEPER_BENCHMARK_CLI_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet("mcp-client", flag.ExitOnError)
		os.Args = []string{"mcp-client", "-benchmark",
			"-direct-endpoint=" + os.Getenv("BENCHMARK_TEST_DIRECT"),
			"-endpoint=" + os.Getenv("BENCHMARK_TEST_GATEWAY"),
			"-token-file=" + os.Getenv("BENCHMARK_TEST_TOKEN"),
			"-concurrency=1", "-calls-per-worker=1", "-warmup=1", "-repetitions=1", "-timeout=10s"}
		main()
		return
	}
	direct := httptest.NewServer(runbookmcp.NewHandler())
	defer direct.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer gateway.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test-only-token"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBenchmarkCLIFailurePreservesJSON$")
	cmd.Env = append(os.Environ(), "PORTKEEPER_BENCHMARK_CLI_CHILD=1",
		"BENCHMARK_TEST_DIRECT="+direct.URL, "BENCHMARK_TEST_GATEWAY="+gateway.URL, "BENCHMARK_TEST_TOKEN="+token)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("failed benchmark exited successfully")
	}
	var report democlient.BenchmarkReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("missing or invalid JSON failure report: %v; stderr=%s", err, &stderr)
	}
	if report.Successful || stderr.Len() != 0 {
		t.Fatalf("failure report contradicted or interrupted: successful=%t stderr=%q", report.Successful, &stderr)
	}
}
