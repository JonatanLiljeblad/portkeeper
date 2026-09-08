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
	agent := flag.String("agent-id", "", "Optional spoof-test X-Agent-ID (ignored by the gateway)")
	tokenFile := flag.String("token-file", "", "Bearer token file, reopened on every HTTP request")
	topic := flag.String("topic", "gateway-routing", "Runbook topic to read")
	timeout := flag.Duration("timeout", 30*time.Second, "Deadline for the complete MCP workflow")
	expectStatus := flag.Int("expect-status", 0, "Poll HTTP GET for this status instead of calling an MCP tool (0 disables)")
	expectNetwork := flag.String("expect-network", "", "Probe TCP connectivity: allowed or blocked (requires healthy-target controls)")
	flag.Parse()
	if *timeout <= 0 || flag.NArg() != 0 {
		log.Fatal("timeout must be positive and positional arguments are not supported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var err error
	credentials := democlient.Credentials{TokenFile: *tokenFile, AgentID: *agent}
	if *expectNetwork != "" {
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
