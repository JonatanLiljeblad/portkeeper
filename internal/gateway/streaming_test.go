package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/jonatan/portkeeper/internal/democlient"
	"github.com/jonatan/portkeeper/internal/runbookmcp"
)

func TestMCPBenchmarkThroughGateway(t *testing.T) {
	t.Setenv("GATEWAY_DEBUG_TRANSPORT", "1")
	backend := httptest.NewServer(runbookmcp.NewHandler())
	defer backend.Close()
	reg := &Registry{backends: map[types.NamespacedName]Backend{
		{Namespace: "test", Name: "demo"}: {
			Namespace: "test", Name: "demo", Ready: true,
			Address:                strings.TrimPrefix(backend.URL, "http://"),
			AllowedServiceAccounts: testAccounts, PolicyObservedAt: time.Now(),
		},
	}}
	server := httptest.NewServer(NewRouter(reg, NewAgentLimiter(10000, 10000), testAuthenticator(t)))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	var output bytes.Buffer
	err := democlient.Benchmark(ctx, democlient.BenchmarkConfig{
		DirectEndpoint: backend.URL, GatewayEndpoint: server.URL + "/test/demo/mcp",
		Credentials: democlient.Credentials{TokenFile: token},
		Concurrency: []int{8}, CallsPerWorker: 20, WarmupPerWorker: 3, Repetitions: 1,
	}, &output)
	if err != nil {
		var report democlient.BenchmarkReport
		if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		for _, phase := range report.Measurements {
			if phase.Errors != 0 || len(phase.WarmupErrors)+len(phase.InitializationErrors)+len(phase.CloseErrors) != 0 {
				t.Logf("%s/%s: errors=%v warmup=%v init=%v close=%v",
					phase.Target, phase.Tool, phase.ErrorCategories, phase.WarmupErrors, phase.InitializationErrors, phase.CloseErrors)
			}
		}
		t.Fatal(err)
	}
}

func TestConcurrentMCPProgress(t *testing.T) {
	server := newTestGateway(t, runbookmcp.NewHandler())
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			var progress atomic.Int64
			client := mcp.NewClient(&mcp.Implementation{Name: "concurrent-stream-test", Version: "1"}, &mcp.ClientOptions{
				ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) {
					progress.Add(1)
				},
				MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
			})
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             server.URL + "/test/demo/mcp",
				HTTPClient:           &http.Client{Transport: agentTransport{base: server.Client().Transport}},
				DisableStandaloneSSE: true, MaxRetries: -1,
			}, nil)
			if err != nil {
				t.Errorf("worker %d connect: %v", worker, err)
				return
			}
			defer session.Close()
			for call := range 40 {
				before := progress.Load()
				params := &mcp.CallToolParams{Name: runbookmcp.StreamTool, Arguments: map[string]any{"topic": "gateway-routing"}}
				params.SetProgressToken(call + 1)
				result, err := session.CallTool(ctx, params)
				if err != nil {
					t.Errorf("worker %d call %d: %T: %v", worker, call, err, err)
					return
				}
				if result.IsError || progress.Load()-before != 3 {
					t.Errorf("worker %d call %d: error=%t, progress=%d", worker, call, result.IsError, progress.Load()-before)
					return
				}
			}
		}()
	}
	workers.Wait()
}
