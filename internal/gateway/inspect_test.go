package gateway

import (
	"errors"
	"testing"
)

func TestInspectMCPRequest(t *testing.T) {
	for _, tt := range []struct {
		name, body   string
		method, tool string
		err          error
	}{
		{name: "tools-call", body: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read","arguments":{"name":"inner"}}}`, method: "tools/call", tool: "read"},
		{name: "params-before-method", body: `{"params":{"arguments":{},"name":"read"},"id":"a","method":"tools/call"}`, method: "tools/call", tool: "read"},
		{name: "escaped-member-and-value", body: `{"id":1,"\u006dethod":"tools\/call","params":{"n\u0061me":"read"}}`, method: "tools/call", tool: "read"},
		{name: "other-method", body: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, method: "tools/list"},
		{name: "notification", body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, method: "notifications/initialized"},
		{name: "client-response", body: `{"jsonrpc":"2.0","id":3,"result":{"name":"x"}}`},
		{name: "unrelated-nested-duplicates", body: `{"id":1,"method":"ping","params":{"a":{"name":1,"name":2}}}`, method: "ping"},

		{name: "batch", body: `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read"}}]`, err: errMCPBatch},
		{name: "duplicate-method", body: `{"id":1,"method":"tools/list","method":"tools/call","params":{"name":"read"}}`, err: errMCPMalformed},
		{name: "case-variant-method", body: `{"id":1,"method":"tools/list","Method":"tools/call","params":{"name":"read"}}`, err: errMCPMalformed},
		{name: "unicode-fold-params", body: `{"id":1,"method":"tools/call","params":{"name":"read"},"paramſ":{"name":"write"}}`, err: errMCPMalformed},
		{name: "duplicate-params", body: `{"id":1,"method":"tools/call","params":{"name":"read"},"params":{"name":"write"}}`, err: errMCPMalformed},
		{name: "duplicate-tool-name", body: `{"id":1,"method":"tools/call","params":{"name":"read","name":"write"}}`, err: errMCPMalformed},
		{name: "case-variant-tool-name", body: `{"id":1,"method":"tools/call","params":{"name":"read","Name":"write"}}`, err: errMCPMalformed},
		{name: "tools-call-without-id", body: `{"method":"tools/call","params":{"name":"read"}}`, err: errMCPMalformed},
		{name: "null-id", body: `{"id":null,"method":"tools/call","params":{"name":"read"}}`, err: errMCPMalformed},
		{name: "non-string-tool", body: `{"id":1,"method":"tools/call","params":{"name":5}}`, err: errMCPMalformed},
		{name: "empty-tool", body: `{"id":1,"method":"tools/call","params":{"name":""}}`, err: errMCPMalformed},
		{name: "array-params", body: `{"id":1,"method":"tools/call","params":["read"]}`, err: errMCPMalformed},
		{name: "missing-params", body: `{"id":1,"method":"tools/call"}`, err: errMCPMalformed},
		{name: "trailing-message", body: `{"id":1,"method":"ping"} {"id":2,"method":"tools/call","params":{"name":"read"}}`, err: errMCPMalformed},
		{name: "truncated", body: `{"id":1,"method":"tools/call","params":{"name":"read"}`, err: errMCPMalformed},
		{name: "empty", body: ``, err: errMCPMalformed},
		{name: "scalar", body: `"tools/call"`, err: errMCPMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inspectMCPRequest([]byte(tt.body))
			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			if err == nil && (got.Method != tt.method || got.Tool != tt.tool) {
				t.Fatalf("inspected (%q, %q), want (%q, %q)", got.Method, got.Tool, tt.method, tt.tool)
			}
		})
	}
}

func BenchmarkInspectMCPRequest(b *testing.B) {
	body := []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"read_runbook","arguments":{"topic":"gateway-routing"},"_meta":{"progressToken":"p-1"}}}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := inspectMCPRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}
