package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/jonatan/portkeeper/internal/democlient"
)

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080/portkeeper-demo/runbooks/mcp", "Gateway MCP endpoint")
	agent := flag.String("agent-id", "demo-agent", "Self-reported X-Agent-ID sent on every request")
	topic := flag.String("topic", "gateway-routing", "Runbook topic to read")
	timeout := flag.Duration("timeout", 30*time.Second, "Deadline for the complete MCP workflow")
	expectStatus := flag.Int("expect-status", 0, "Poll HTTP GET for this status instead of calling an MCP tool (0 disables)")
	flag.Parse()
	if *timeout <= 0 || flag.NArg() != 0 {
		log.Fatal("timeout must be positive and positional arguments are not supported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var err error
	if *expectStatus != 0 {
		err = democlient.WaitForStatus(ctx, *endpoint, *agent, *expectStatus, os.Stdout)
	} else {
		err = democlient.Run(ctx, *endpoint, *agent, *topic, os.Stdout)
	}
	if err != nil {
		log.Fatal(err)
	}
}
