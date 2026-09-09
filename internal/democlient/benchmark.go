package democlient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonatan/portkeeper/internal/runbookmcp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BenchmarkConfig describes a closed-loop workload: each worker has one SDK
// session and at most one in-flight call. Zero values select the documented defaults.
type BenchmarkConfig struct {
	DirectEndpoint  string      `json:"-"`
	GatewayEndpoint string      `json:"-"`
	Credentials     Credentials `json:"-"`
	Concurrency     []int       `json:"concurrency"`
	CallsPerWorker  int         `json:"calls_per_worker"`
	WarmupPerWorker int         `json:"warmup_per_worker"`
	Repetitions     int         `json:"repetitions"`
	Topic           string      `json:"topic"`
}

type LatencySummary struct {
	Samples int     `json:"samples"`
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
}

type BenchmarkSample struct {
	Worker               int     `json:"worker"`
	Call                 int     `json:"call"`
	DurationSeconds      float64 `json:"duration_seconds"`
	FirstProgressSeconds float64 `json:"first_progress_seconds,omitempty"`
	ProgressUpdates      int     `json:"progress_updates,omitempty"`
	ErrorCategory        string  `json:"error_category,omitempty"`
	JSONRPCErrorCode     *int64  `json:"jsonrpc_error_code,omitempty"`
}

type BenchmarkMeasurement struct {
	Target                      string            `json:"target"`
	Tool                        string            `json:"tool"`
	Concurrency                 int               `json:"concurrency"`
	Repetition                  int               `json:"repetition"`
	PlannedCalls                int               `json:"planned_calls"`
	Attempts                    int               `json:"attempts"`
	Successes                   int               `json:"successes"`
	Errors                      int               `json:"errors"`
	ErrorCategories             map[string]int    `json:"error_categories"`
	InitializationErrors        map[string]int    `json:"initialization_errors"`
	CloseErrors                 map[string]int    `json:"close_errors"`
	WarmupAttempts              int               `json:"warmup_attempts"`
	WarmupErrors                map[string]int    `json:"warmup_errors"`
	WallSeconds                 float64           `json:"wall_seconds"`
	AttemptsPerSecond           float64           `json:"attempts_per_second"`
	SuccessesPerSecond          float64           `json:"successes_per_second"`
	CompletionLatencySeconds    LatencySummary    `json:"completion_latency_seconds"`
	SuccessLatencySeconds       LatencySummary    `json:"success_latency_seconds"`
	FirstProgressLatencySeconds LatencySummary    `json:"first_progress_latency_seconds"`
	Samples                     []BenchmarkSample `json:"samples"`
}

type BenchmarkOverhead struct {
	Tool                      string         `json:"tool"`
	Concurrency               int            `json:"concurrency"`
	Repetition                int            `json:"repetition"`
	CompletionDeltaSeconds    LatencySummary `json:"completion_delta_seconds"`
	P50CompletionPercent      float64        `json:"p50_completion_percent"`
	FirstProgressDeltaSeconds LatencySummary `json:"first_progress_delta_seconds"`
}

type BenchmarkReport struct {
	SchemaVersion int                    `json:"schema_version"`
	Successful    bool                   `json:"successful"`
	GoVersion     string                 `json:"go_version"`
	GOOS          string                 `json:"goos"`
	GOARCH        string                 `json:"goarch"`
	NumCPU        int                    `json:"num_cpu"`
	Config        BenchmarkConfig        `json:"config"`
	Measurements  []BenchmarkMeasurement `json:"measurements"`
	Overhead      []BenchmarkOverhead    `json:"overhead,omitempty"`
}

// ErrBenchmarkFailed means a complete JSON failure report was already written.
var ErrBenchmarkFailed = errors.New("benchmark failed; see sanitized error categories in JSON report")

func normalizeBenchmark(c BenchmarkConfig) (BenchmarkConfig, error) {
	if err := validateTarget(c.DirectEndpoint); err != nil {
		return c, errors.New("invalid direct endpoint")
	}
	if err := validateTarget(c.GatewayEndpoint); err != nil {
		return c, errors.New("invalid gateway endpoint")
	}
	if c.DirectEndpoint == c.GatewayEndpoint || c.Credentials.TokenFile == "" {
		return c, errors.New("benchmark requires distinct endpoints and a gateway token file")
	}
	if len(c.Concurrency) == 0 {
		c.Concurrency = []int{1, 4, 8}
	}
	if c.CallsPerWorker == 0 {
		c.CallsPerWorker = 20
	}
	if c.WarmupPerWorker == 0 {
		c.WarmupPerWorker = 3
	}
	if c.Repetitions == 0 {
		c.Repetitions = 2
	}
	if c.Topic == "" {
		c.Topic = "gateway-routing"
	}
	if c.Topic != "gateway-routing" && c.Topic != "rate-limiting" {
		return c, errors.New("benchmark topic must be an embedded runbook")
	}
	if len(c.Concurrency) > 8 || c.CallsPerWorker < 1 || c.CallsPerWorker > 1000 ||
		c.WarmupPerWorker < 1 || c.WarmupPerWorker > 100 || c.Repetitions < 1 || c.Repetitions > 10 {
		return c, errors.New("benchmark workload exceeds bounds")
	}
	total := 0
	seen := make(map[int]bool)
	for _, n := range c.Concurrency {
		if n < 1 || n > 64 || seen[n] {
			return c, errors.New("concurrency values must be distinct integers from 1 to 64")
		}
		seen[n] = true
		total += n * c.CallsPerWorker * c.Repetitions * 4
	}
	if total > 100000 {
		return c, errors.New("benchmark is limited to 100000 measured calls")
	}
	return c, nil
}

// Benchmark emits one bounded JSON report, then returns an error if any setup,
// warm-up, call, stream validation, or close failed. It never emits credentials,
// endpoint URLs, token paths, or raw error strings. The caller controls its deadline.
func Benchmark(ctx context.Context, config BenchmarkConfig, output io.Writer) error {
	c, err := normalizeBenchmark(config)
	if err != nil {
		return err
	}
	report := BenchmarkReport{
		SchemaVersion: 1, Successful: true, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, NumCPU: runtime.NumCPU(), Config: c,
		Measurements: []BenchmarkMeasurement{},
	}
phases:
	for _, concurrency := range c.Concurrency {
		for _, tool := range []string{runbookmcp.ReadTool, runbookmcp.StreamTool} {
			for repetition := 1; repetition <= c.Repetitions; repetition++ {
				targets := []string{"direct", "gateway"}
				if repetition%2 == 0 {
					targets[0], targets[1] = targets[1], targets[0]
				}
				for _, target := range targets {
					measurement := benchmarkPhase(ctx, c, target, tool, concurrency, repetition)
					report.Measurements = append(report.Measurements, measurement)
					if measurement.Attempts != measurement.PlannedCalls || measurement.Errors > 0 ||
						len(measurement.InitializationErrors)+len(measurement.CloseErrors)+len(measurement.WarmupErrors) > 0 {
						report.Successful = false
					}
					if ctx.Err() != nil {
						report.Successful = false
						break phases
					}
				}
			}
		}
	}
	if report.Successful {
		report.Overhead = benchmarkOverhead(report.Measurements)
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return fmt.Errorf("write benchmark report: %w", err)
	}
	if !report.Successful {
		return ErrBenchmarkFailed
	}
	return nil
}

// statusTransport captures rejected HTTP requests without parsing SDK errors,
// which may include an upstream response body or a credential file path.
type statusTransport struct {
	base   http.RoundTripper
	ctx    context.Context
	status atomic.Int32
}

func (t *statusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	if t.ctx.Err() != nil {
		cancel()
	}
	response, err := t.base.RoundTrip(req.Clone(ctx))
	if response != nil {
		t.status.Store(int32(response.StatusCode))
	}
	if err != nil {
		stop()
		cancel()
	} else {
		response.Body = &benchmarkBody{ReadCloser: response.Body, stop: stop, cancel: cancel}
	}
	return response, err
}

type benchmarkBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
}

func (b *benchmarkBody) Close() error {
	defer b.cancel()
	defer b.stop()
	return b.ReadCloser.Close()
}

type benchmarkWorker struct {
	session   *mcp.ClientSession
	transport *statusTransport
	mu        sync.Mutex
	token     string
	first     time.Time
	updates   int
}

func (w *benchmarkWorker) progress(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if req.Params.ProgressToken != w.token || w.token == "" {
		return
	}
	if w.first.IsZero() {
		w.first = time.Now()
	}
	w.updates++
}

func parallelWorkers(count int, action func(int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			action(i)
		}()
	}
	close(start)
	wg.Wait()
}

func benchmarkPhase(ctx context.Context, c BenchmarkConfig, target, tool string, concurrency, repetition int) BenchmarkMeasurement {
	m := BenchmarkMeasurement{
		Target: target, Tool: tool, Concurrency: concurrency, Repetition: repetition,
		PlannedCalls:    concurrency * c.CallsPerWorker,
		ErrorCategories: map[string]int{}, InitializationErrors: map[string]int{},
		CloseErrors: map[string]int{}, WarmupErrors: map[string]int{},
		Samples: []BenchmarkSample{},
	}
	workers := make([]*benchmarkWorker, concurrency)
	setup := make([]string, concurrency)
	parallelWorkers(concurrency, func(i int) {
		endpoint, credentials := c.DirectEndpoint, Credentials{}
		if target == "gateway" {
			endpoint, credentials = c.GatewayEndpoint, c.Credentials
		}
		http := httpClient(credentials)
		// SDK session.Close uses its own context; bound that HTTP exchange too.
		http.Timeout = 30 * time.Second
		worker := &benchmarkWorker{transport: &statusTransport{base: http.Transport, ctx: ctx}}
		http.Transport = worker.transport
		workers[i] = worker
		client := mcp.NewClient(&mcp.Implementation{Name: "portkeeper-benchmark", Version: "0.1.0"}, &mcp.ClientOptions{
			ProgressNotificationHandler: worker.progress,
			MultiRoundTrip:              &mcp.MultiRoundTripOptions{Disabled: true},
		})
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: http, MaxRetries: -1, DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			setup[i] = benchmarkError(ctx, err, int(worker.transport.status.Load()))
			return
		}
		worker.session = session
		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			setup[i] = benchmarkError(ctx, err, int(worker.transport.status.Load()))
			return
		}
		for _, advertised := range tools.Tools {
			if advertised.Name == tool {
				return
			}
		}
		setup[i] = "missing_tool"
	})
	for _, category := range setup {
		if category != "" {
			m.InitializationErrors[category]++
		}
	}
	if len(m.InitializationErrors) == 0 {
		warmup := make([][]BenchmarkSample, concurrency)
		parallelWorkers(concurrency, func(i int) {
			for call := range c.WarmupPerWorker {
				if ctx.Err() != nil {
					break
				}
				warmup[i] = append(warmup[i], workers[i].call(ctx, tool, c.Topic, i, call, "warmup"))
			}
		})
		for _, samples := range warmup {
			for _, sample := range samples {
				m.WarmupAttempts++
				if sample.ErrorCategory != "" {
					m.WarmupErrors[sample.ErrorCategory]++
				}
			}
		}
		samples := make([][]BenchmarkSample, concurrency)
		start := time.Now()
		parallelWorkers(concurrency, func(i int) {
			for call := range c.CallsPerWorker {
				if ctx.Err() != nil {
					break
				}
				samples[i] = append(samples[i], workers[i].call(ctx, tool, c.Topic, i, call, "measured"))
			}
		})
		m.WallSeconds = time.Since(start).Seconds()
		for _, workerSamples := range samples {
			m.Samples = append(m.Samples, workerSamples...)
		}
	}
	closeErrors := make([]string, concurrency)
	parallelWorkers(concurrency, func(i int) {
		if worker := workers[i]; worker.session != nil {
			worker.transport.status.Store(0)
			if err := worker.session.Close(); err != nil {
				closeErrors[i] = benchmarkError(ctx, err, int(worker.transport.status.Load()))
			} else if status := worker.transport.status.Load(); status >= 300 && status != http.StatusMethodNotAllowed {
				// DELETE 405 is allowed by MCP, but do not silently accept auth failures.
				closeErrors[i] = benchmarkError(ctx, errors.New("close rejected"), int(status))
			}
		}
	})
	for _, category := range closeErrors {
		if category != "" {
			m.CloseErrors[category]++
		}
	}
	aggregateMeasurement(&m)
	return m
}

func (w *benchmarkWorker) call(ctx context.Context, tool, topic string, worker, call int, phase string) BenchmarkSample {
	sample := BenchmarkSample{Worker: worker, Call: call}
	params := &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"topic": topic}}
	w.mu.Lock()
	w.token, w.first, w.updates = "", time.Time{}, 0
	if tool == runbookmcp.StreamTool {
		w.token = fmt.Sprintf("%s-%d-%d", phase, worker, call)
		params.SetProgressToken(w.token)
	}
	w.mu.Unlock()
	w.transport.status.Store(0)
	start := time.Now()
	result, err := w.session.CallTool(ctx, params)
	end := time.Now()
	sample.DurationSeconds = end.Sub(start).Seconds()
	w.mu.Lock()
	first := w.first
	sample.ProgressUpdates = w.updates
	w.token = ""
	w.mu.Unlock()
	if !first.IsZero() {
		sample.FirstProgressSeconds = first.Sub(start).Seconds()
	}
	switch {
	case err != nil:
		sample.ErrorCategory = benchmarkError(ctx, err, int(w.transport.status.Load()))
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			code := rpcErr.Code
			sample.JSONRPCErrorCode = &code
		}
	case result == nil:
		sample.ErrorCategory = "invalid_content"
	case result.IsError:
		sample.ErrorCategory = "tool_error"
	case result.NeedsInput():
		sample.ErrorCategory = "input_required"
	default:
		if len(result.Content) == 0 {
			sample.ErrorCategory = "invalid_content"
		}
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); !ok || text.Text == "" {
				sample.ErrorCategory = "invalid_content"
			}
		}
	}
	if sample.ErrorCategory == "" && tool == runbookmcp.StreamTool {
		if first.IsZero() || sample.ProgressUpdates != 3 {
			sample.ErrorCategory = "stream_missing_progress"
		} else if first.Before(start) || end.Sub(first) < 25*time.Millisecond {
			// A buffered SSE response may replay progress immediately before the
			// final result. The demo deliberately leaves 150ms after its first update.
			sample.ErrorCategory = "stream_late_progress"
		}
	}
	return sample
}

func benchmarkError(ctx context.Context, err error, status int) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return "cancelled"
	case status == http.StatusUnauthorized:
		return "http_401"
	case status == http.StatusForbidden:
		return "http_403"
	case status == http.StatusTooManyRequests:
		return "http_429"
	case status >= 500:
		return "http_5xx"
	case status >= 300:
		return "http_other"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return "jsonrpc_error"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "transport_timeout"
		}
		return "transport"
	}
	return "protocol_or_transport"
}

func latencySummary(values []float64) LatencySummary {
	if len(values) == 0 {
		return LatencySummary{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	percentile := func(p float64) float64 { return sorted[int(math.Ceil(p*float64(len(sorted))))-1] }
	return LatencySummary{Samples: len(sorted), P50: percentile(.50), P95: percentile(.95), P99: percentile(.99)}
}

func aggregateMeasurement(m *BenchmarkMeasurement) {
	all, successful, progress := []float64{}, []float64{}, []float64{}
	for _, sample := range m.Samples {
		m.Attempts++
		all = append(all, sample.DurationSeconds)
		if sample.ErrorCategory != "" {
			m.Errors++
			m.ErrorCategories[sample.ErrorCategory]++
			continue
		}
		m.Successes++
		successful = append(successful, sample.DurationSeconds)
		if sample.ProgressUpdates > 0 {
			progress = append(progress, sample.FirstProgressSeconds)
		}
	}
	m.CompletionLatencySeconds = latencySummary(all)
	m.SuccessLatencySeconds = latencySummary(successful)
	m.FirstProgressLatencySeconds = latencySummary(progress)
	if m.WallSeconds > 0 {
		m.AttemptsPerSecond = float64(m.Attempts) / m.WallSeconds
		m.SuccessesPerSecond = float64(m.Successes) / m.WallSeconds
	}
}

func benchmarkOverhead(measurements []BenchmarkMeasurement) []BenchmarkOverhead {
	var overhead []BenchmarkOverhead
	for _, direct := range measurements {
		if direct.Target != "direct" {
			continue
		}
		for _, gateway := range measurements {
			if gateway.Target != "gateway" || gateway.Tool != direct.Tool ||
				gateway.Concurrency != direct.Concurrency || gateway.Repetition != direct.Repetition {
				continue
			}
			delta := func(a, b LatencySummary) LatencySummary {
				return LatencySummary{Samples: a.Samples, P50: b.P50 - a.P50, P95: b.P95 - a.P95, P99: b.P99 - a.P99}
			}
			o := BenchmarkOverhead{
				Tool: direct.Tool, Concurrency: direct.Concurrency, Repetition: direct.Repetition,
				CompletionDeltaSeconds:    delta(direct.CompletionLatencySeconds, gateway.CompletionLatencySeconds),
				FirstProgressDeltaSeconds: delta(direct.FirstProgressLatencySeconds, gateway.FirstProgressLatencySeconds),
			}
			if direct.CompletionLatencySeconds.P50 > 0 {
				o.P50CompletionPercent = 100 * o.CompletionDeltaSeconds.P50 / direct.CompletionLatencySeconds.P50
			}
			overhead = append(overhead, o)
		}
	}
	return overhead
}
