package democlient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonatan/portkeeper/internal/runbookmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBenchmarkAggregation(t *testing.T) {
	values := make([]float64, 100)
	for i := range values {
		values[i] = float64(100 - i)
	}
	summary := latencySummary(values)
	if summary != (LatencySummary{Samples: 100, P50: 50, P95: 95, P99: 99}) || values[0] != 100 {
		t.Fatalf("nearest-rank percentiles or input mutation: %+v", summary)
	}
	if latencySummary(nil) != (LatencySummary{}) {
		t.Fatal("empty sample set should have zero percentiles")
	}
	m := BenchmarkMeasurement{
		WallSeconds: 2, ErrorCategories: map[string]int{},
		Samples: []BenchmarkSample{
			{DurationSeconds: 1, FirstProgressSeconds: .1, ProgressUpdates: 3},
			{DurationSeconds: 3, FirstProgressSeconds: .2, ProgressUpdates: 3},
			{DurationSeconds: .01, ErrorCategory: "http_429"},
		},
	}
	aggregateMeasurement(&m)
	if m.Attempts != 3 || m.Successes != 2 || m.Errors != 1 || m.ErrorCategories["http_429"] != 1 ||
		m.AttemptsPerSecond != 1.5 || m.SuccessesPerSecond != 1 ||
		m.CompletionLatencySeconds.P50 != 1 || m.SuccessLatencySeconds.P95 != 3 ||
		m.FirstProgressLatencySeconds.P95 != .2 {
		t.Fatalf("bad aggregation: %+v", m)
	}
}

func smallBenchmarkConfig(direct, gateway, path string) BenchmarkConfig {
	return BenchmarkConfig{
		DirectEndpoint: direct, GatewayEndpoint: gateway, Credentials: Credentials{TokenFile: path, AgentID: "spoof"},
		Concurrency: []int{1}, CallsPerWorker: 1, WarmupPerWorker: 1, Repetitions: 1,
	}
}

func decodeBenchmark(t *testing.T, output *bytes.Buffer) BenchmarkReport {
	t.Helper()
	var report BenchmarkReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("invalid report: %v", err)
	}
	return report
}

func TestBenchmarkSDKConcurrencyAndCredentialSeparation(t *testing.T) {
	path := tokenFile(t, "benchmark-secret")
	var calls, active, peak, gatewayRequests, directRequests atomic.Int32
	handler := runbookmcp.NewHandler()
	wrap := func(gateway bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if gateway {
				gatewayRequests.Add(1)
				if r.Header.Get("Authorization") != "Bearer benchmark-secret" || r.Header.Get("X-Agent-ID") != "spoof" {
					t.Error("gateway SDK exchange missing credentials")
				}
			} else {
				directRequests.Add(1)
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Agent-ID") != "" {
					t.Error("direct endpoint received gateway credentials")
				}
			}
			if r.Method == http.MethodPost {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(body))
				var request struct {
					Method string `json:"method"`
				}
				json.Unmarshal(body, &request)
				if request.Method == "tools/call" {
					calls.Add(1)
					current := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); current > old; old = peak.Load() {
						if peak.CompareAndSwap(old, current) {
							break
						}
					}
				}
			}
			handler.ServeHTTP(w, r)
		})
	}
	direct := httptest.NewServer(wrap(false))
	defer direct.Close()
	gateway := httptest.NewServer(wrap(true))
	defer gateway.Close()
	config := smallBenchmarkConfig(direct.URL, gateway.URL, path)
	config.Concurrency, config.CallsPerWorker, config.Repetitions = []int{1, 2}, 2, 2
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := Benchmark(ctx, config, &output); err != nil {
		t.Fatalf("%v: %s", err, &output)
	}
	report := decodeBenchmark(t, &output)
	if !report.Successful || len(report.Measurements) != 16 || len(report.Overhead) != 8 {
		t.Fatalf("incomplete report: %+v", report)
	}
	for i, measurement := range report.Measurements {
		if measurement.Attempts != measurement.Concurrency*2 || measurement.Successes != measurement.Attempts ||
			measurement.Errors != 0 || measurement.WarmupAttempts != measurement.Concurrency ||
			len(measurement.Samples) != measurement.Attempts || measurement.WallSeconds <= 0 {
			t.Fatalf("bad sample accounting: %+v", measurement)
		}
		wantTarget := []string{"direct", "gateway", "gateway", "direct"}[i%4]
		if measurement.Target != wantTarget {
			t.Fatal("target ordering did not reverse between repetitions")
		}
		if measurement.Tool == runbookmcp.StreamTool &&
			measurement.FirstProgressLatencySeconds.Samples != measurement.Successes {
			t.Fatal("stream progress was not captured for every successful call")
		}
	}
	// Sum(concurrency) * (warmup + measured) * tools * targets * repetitions.
	if calls.Load() != 3*3*2*2*2 || peak.Load() < 2 || directRequests.Load() == 0 || gatewayRequests.Load() == 0 {
		t.Fatalf("unexpected execution/concurrency counts: calls=%d peak=%d", calls.Load(), peak.Load())
	}
	for _, secret := range []string{"benchmark-secret", path, direct.URL, gateway.URL, "spoof"} {
		if strings.Contains(output.String(), secret) {
			t.Error("report contains credentials, paths, or endpoints")
		}
	}
}

func TestBenchmarkCredentialRotation(t *testing.T) {
	path := tokenFile(t, "before-rotation")
	direct := httptest.NewServer(runbookmcp.NewHandler())
	defer direct.Close()
	handler := runbookmcp.NewHandler()
	var seen atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "after-rotation"
		if seen.Add(1) == 1 {
			want = "before-rotation"
			if err := os.WriteFile(path, []byte("after-rotation"), 0600); err != nil {
				t.Error(err)
			}
		}
		if r.Header.Get("Authorization") != "Bearer "+want {
			t.Error("benchmark did not reopen the rotated credential file")
		}
		handler.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	var output bytes.Buffer
	if err := Benchmark(t.Context(), smallBenchmarkConfig(direct.URL, gateway.URL, path), &output); err != nil {
		t.Fatalf("%v: %s", err, &output)
	}
}

func TestBenchmarkReportsSetupFailureWithoutSecrets(t *testing.T) {
	direct := httptest.NewServer(runbookmcp.NewHandler())
	defer direct.Close()
	var rejected atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rejected.Add(1)
		http.Error(w, "sensitive-upstream-detail", http.StatusUnauthorized)
	}))
	defer gateway.Close()
	path := tokenFile(t, "secret-token-value")
	var output bytes.Buffer
	if err := Benchmark(t.Context(), smallBenchmarkConfig(direct.URL, gateway.URL, path), &output); err == nil {
		t.Fatal("initialization rejection did not fail benchmark")
	}
	report := decodeBenchmark(t, &output)
	// SDK 1.7 probes server/discover before falling back to legacy initialize.
	// Neither is a tool execution or a retry of tools/call.
	if report.Successful || len(report.Overhead) != 0 || rejected.Load() != 4 {
		t.Fatalf("failed benchmark claimed success or retried initialization: successful=%t overhead=%d rejected=%d", report.Successful, len(report.Overhead), rejected.Load())
	}
	for _, m := range report.Measurements {
		if m.Target == "gateway" && (m.InitializationErrors["http_401"] != 1 || m.Attempts != 0) {
			t.Fatalf("setup failures not separated: %+v", m)
		}
	}
	if strings.Contains(output.String(), "sensitive-upstream-detail") || strings.Contains(output.String(), path) ||
		strings.Contains(output.String(), "secret-token-value") {
		t.Fatal("sensitive error details leaked")
	}
}

func TestBenchmarkReportsRejectedCalls(t *testing.T) {
	direct := httptest.NewServer(runbookmcp.NewHandler())
	defer direct.Close()
	handler := runbookmcp.NewHandler()
	var rejected atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Method string `json:"method"`
			}
			json.Unmarshal(body, &request)
			if request.Method == "tools/call" {
				rejected.Add(1)
				http.Error(w, "do not publish this detail", http.StatusTooManyRequests)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	var output bytes.Buffer
	if err := Benchmark(t.Context(), smallBenchmarkConfig(direct.URL, gateway.URL, tokenFile(t, "secret")), &output); err == nil {
		t.Fatal("rejected calls did not fail benchmark")
	}
	report := decodeBenchmark(t, &output)
	if report.Successful || len(report.Overhead) != 0 || rejected.Load() != 4 {
		t.Fatalf("calls retried or errors hidden: rejected=%d", rejected.Load())
	}
	for _, m := range report.Measurements {
		if m.Target == "gateway" && (m.Attempts != 1 || m.Errors != 1 || m.Successes != 0 ||
			m.ErrorCategories["http_429"] != 1 || m.WarmupErrors["http_429"] != 1) {
			t.Fatalf("rejection accounting incorrect: %+v", m)
		}
	}
}

func TestBenchmarkRejectsMissingOrBufferedStreaming(t *testing.T) {
	for _, mode := range []string{"missing", "late", "tool_error"} {
		t.Run(mode, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			type input struct {
				Topic string `json:"topic"`
			}
			for _, tool := range []string{runbookmcp.ReadTool, runbookmcp.StreamTool} {
				mcp.AddTool(server, &mcp.Tool{Name: tool}, func(ctx context.Context, req *mcp.CallToolRequest, _ input) (*mcp.CallToolResult, any, error) {
					if mode == "tool_error" {
						return nil, nil, errors.New("private-tool-error")
					}
					if tool == runbookmcp.StreamTool && mode == "late" {
						for step := 1; step <= 3; step++ {
							if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
								ProgressToken: req.Params.GetProgressToken(), Progress: float64(step), Total: 3,
							}); err != nil {
								return nil, nil, err
							}
						}
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "content"}}}, nil, nil
				})
			}
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			direct := httptest.NewServer(handler)
			defer direct.Close()
			gateway := httptest.NewServer(handler)
			defer gateway.Close()
			var output bytes.Buffer
			if err := Benchmark(t.Context(), smallBenchmarkConfig(direct.URL, gateway.URL, tokenFile(t, "secret")), &output); err == nil {
				t.Fatal("invalid stream or tool error accepted")
			}
			report := decodeBenchmark(t, &output)
			want := map[string]string{"missing": "stream_missing_progress", "late": "stream_late_progress", "tool_error": "tool_error"}[mode]
			for _, m := range report.Measurements {
				rejected := m.ErrorCategories[want]
				if mode == "late" {
					// When progress and the result arrive together, the SDK may
					// return before all notification callbacks finish.
					rejected += m.ErrorCategories["stream_missing_progress"]
				}
				if (m.Tool == runbookmcp.StreamTool || mode == "tool_error") && rejected != 1 {
					t.Fatalf("unexpected stream failure: %+v", m)
				}
			}
			if strings.Contains(output.String(), "private-tool-error") {
				t.Fatal("raw tool error leaked")
			}
		})
	}
}

func TestBenchmarkBoundsAndCancellation(t *testing.T) {
	c := smallBenchmarkConfig("http://direct/mcp", "http://gateway/mcp", "do-not-open")
	for _, concurrency := range [][]int{{0}, {-1}, {65}, {1, 1}} {
		invalid := c
		invalid.Concurrency = concurrency
		if _, err := normalizeBenchmark(invalid); err == nil {
			t.Errorf("accepted concurrency %v", concurrency)
		}
	}
	c.Concurrency, c.CallsPerWorker, c.Repetitions = []int{64}, 1000, 10
	if _, err := normalizeBenchmark(c); err == nil {
		t.Fatal("unbounded sample count accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer
	if err := Benchmark(ctx, smallBenchmarkConfig("http://127.0.0.1:1", "http://127.0.0.1:2", "do-not-open"), &output); err == nil {
		t.Fatal("cancelled benchmark succeeded")
	}
	report := decodeBenchmark(t, &output)
	if report.Successful || len(report.Measurements) != 1 || report.Measurements[0].InitializationErrors["cancelled"] != 1 {
		t.Fatalf("cancelled setup not reported: %+v", report)
	}
}
