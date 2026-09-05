package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jonatan/portkeeper/internal/runbookmcp"
)

func main() {
	addr := os.Getenv("RUNBOOK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9001"
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", runbookmcp.NewHandler())
	server := &http.Server{
		Addr:              addr,
		Handler:           http.NewCrossOriginProtection().Handler(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("runbook MCP server listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}
