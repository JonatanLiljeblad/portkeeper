// Command toy-mcp-server is a throwaway HTTP server used to demo the
// portkeeper v0.1 loop. It is not a real MCP server — it just exposes a
// couple of tool-shaped routes the gateway can proxy to.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}

	mux := http.NewServeMux()

	// /echo echoes the request body back verbatim.
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
	})

	// /reverse-string reverses the request body (bytewise).
	mux.HandleFunc("/reverse-string", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		for i, j := 0, len(body)-1; i < j; i, j = i+1, j-1 {
			body[i], body[j] = body[j], body[i]
		}
		w.Write(body)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("toy-mcp-server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
