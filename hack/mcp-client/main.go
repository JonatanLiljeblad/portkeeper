package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jonatan/portkeeper/internal/democlient"
)

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080/portkeeper-demo/runbooks/mcp", "Gateway MCP endpoint")
	agent := flag.String("agent-id", "", "Optional spoof-test X-Agent-ID (ignored by the gateway)")
	tokenFile := flag.String("token-file", "", "Bearer token file, reopened on every HTTP request")
	topic := flag.String("topic", "gateway-routing", "Runbook topic to read")
	timeout := flag.Duration("timeout", 30*time.Second, "Deadline for the complete MCP workflow")
	expectStatus := flag.Int("expect-status", 0, "Poll HTTP GET for this status instead of calling an MCP tool (0 disables)")
	expectNetwork := flag.String("expect-network", "", "Probe TCP connectivity: allowed or blocked (requires healthy-target controls)")
	benchmark := flag.Bool("benchmark", false, "Run SDK direct/gateway measurements and emit a JSON report")
	directEndpoint := flag.String("direct-endpoint", "", "Direct backend MCP endpoint, never sent gateway credentials")
	concurrency := flag.String("concurrency", "1,4,8", "Comma-separated benchmark worker counts")
	calls := flag.Int("calls-per-worker", 20, "Measured calls per worker, per tool and endpoint")
	warmup := flag.Int("warmup", 3, "Excluded warm-up calls per worker, per tool and endpoint")
	repetitions := flag.Int("repetitions", 2, "Benchmark repetitions, alternating direct/gateway order")
	flag.Parse()
	if *benchmark {
		timeoutSet := false
		flag.Visit(func(f *flag.Flag) { timeoutSet = timeoutSet || f.Name == "timeout" })
		if !timeoutSet {
			*timeout = 10 * time.Minute
		}
	}
	if *timeout <= 0 || flag.NArg() != 0 {
		log.Fatal("timeout must be positive and positional arguments are not supported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var err error
	credentials := democlient.Credentials{TokenFile: *tokenFile, AgentID: *agent}
	if *benchmark {
		if *expectStatus != 0 || *expectNetwork != "" {
			log.Fatal("benchmark, expect-status and expect-network are mutually exclusive")
		}
		var workers []int
		for _, value := range strings.Split(*concurrency, ",") {
			n, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				log.Fatal("concurrency must be comma-separated integers")
			}
			workers = append(workers, n)
		}
		err = democlient.Benchmark(ctx, democlient.BenchmarkConfig{
			DirectEndpoint: *directEndpoint, GatewayEndpoint: *endpoint,
			Credentials: credentials, Concurrency: workers, CallsPerWorker: *calls,
			WarmupPerWorker: *warmup, Repetitions: *repetitions, Topic: *topic,
		}, os.Stdout)
	} else if *expectNetwork != "" {
		if (*expectNetwork != "allowed" && *expectNetwork != "blocked") || *expectStatus != 0 || *tokenFile != "" {
			log.Fatal("expect-network must be allowed or blocked, without expect-status or token-file")
		}
		err = democlient.ProbeNetwork(ctx, *endpoint, *expectNetwork == "blocked", os.Stdout)
	} else if *expectStatus != 0 {
		err = democlient.WaitForStatus(ctx, *endpoint, credentials, *expectStatus, os.Stdout)
	} else {
		err = democlient.Run(ctx, *endpoint, credentials, *topic, os.Stdout)
	}
	if err != nil {
		log.Fatal(err)
	}
}
