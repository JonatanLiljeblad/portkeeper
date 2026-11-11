package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey = "sk-test-secret-value"

func keyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(testKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnthropicPlannerToolLoop(t *testing.T) {
	const firstReply = `[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"Checking."},` +
		`{"type":"tool_use","id":"toolu_1","name":"ops__scale_deployment","input":{"name":"checkout","replicas":3}}]`
	var requests []map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != testKey || r.Header.Get("anthropic-version") == "" {
			t.Errorf("unexpected request: %s %v", r.URL.Path, r.Header)
		}
		var request map[string]json.RawMessage
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		switch len(requests) {
		case 1:
			// One overload response must be retried, not surfaced.
			w.WriteHeader(529)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
		case 2:
			io.WriteString(w, `{"content":`+firstReply+`,"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":20}}`)
		default:
			io.WriteString(w, `{"content":[{"type":"text","text":"Scaling was refused."}],"stop_reason":"end_turn","usage":{"input_tokens":150,"output_tokens":10}}`)
		}
	}))
	defer server.Close()

	planner := &Anthropic{BaseURL: server.URL, Model: "test-model", KeyFile: keyFile(t), RetryDelay: time.Millisecond}
	planner.Begin("resolve it", []ToolSpec{{Name: "ops__scale_deployment", Description: "scale", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	step, err := planner.Next(t.Context(), nil)
	if err != nil || step.Text != "Checking." || len(step.Calls) != 1 || step.Calls[0].ID != "toolu_1" ||
		step.Calls[0].Name != "ops__scale_deployment" || string(step.Calls[0].Input) != `{"name":"checkout","replicas":3}` {
		t.Fatalf("unexpected first step: %+v %v", step, err)
	}
	step, err = planner.Next(t.Context(), []ToolResult{{ID: "toolu_1", Name: "ops__scale_deployment", Content: "by gateway policy: no", IsError: true, Denied: true}})
	if err != nil || len(step.Calls) != 0 || step.Text != "Scaling was refused." {
		t.Fatalf("unexpected final step: %+v %v", step, err)
	}
	if usage := planner.Usage(); usage != (Usage{ModelCalls: 2, InputTokens: 250, OutputTokens: 30}) {
		t.Fatalf("usage = %+v", usage)
	}

	if len(requests) != 3 || string(requests[1]["model"]) != `"test-model"` || !strings.Contains(string(requests[1]["tools"]), `"input_schema":{"type":"object"}`) {
		t.Fatalf("unexpected requests: %v", requests)
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(requests[2]["messages"], &messages); err != nil || len(messages) != 3 {
		t.Fatalf("conversation = %s", requests[2]["messages"])
	}
	// Uninterpreted blocks go back unchanged, and the denial is an error result.
	if messages[1].Role != "assistant" || string(messages[1].Content) != firstReply {
		t.Fatalf("assistant turn was not echoed verbatim: %s", messages[1].Content)
	}
	if want := `[{"type":"tool_result","tool_use_id":"toolu_1","content":"DENIED by gateway policy: no","is_error":true}]`; string(messages[2].Content) != want {
		t.Fatalf("tool result = %s", messages[2].Content)
	}
}

func TestAnthropicPlannerFailures(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		// A provider echoing the credential must not leak it through the error.
		io.WriteString(w, `{"error":{"type":"authentication_error","message":"invalid x-api-key `+r.Header.Get("x-api-key")+`"}}`)
	}))
	defer server.Close()
	planner := &Anthropic{BaseURL: server.URL, KeyFile: keyFile(t), RetryDelay: time.Millisecond}
	planner.Begin("task", nil)
	_, err := planner.Next(t.Context(), nil)
	if err == nil || calls != 1 || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}

	missing := &Anthropic{BaseURL: server.URL, KeyFile: filepath.Join(t.TempDir(), "absent")}
	missing.Begin("task", nil)
	if _, err := missing.Next(t.Context(), nil); err == nil || calls != 1 {
		t.Fatalf("missing key file reached the API: calls=%d err=%v", calls, err)
	}
}

func TestScriptStopsAtFirstFailure(t *testing.T) {
	script := IncidentScript(0)
	step, _ := script.Next(t.Context(), nil)
	step, _ = script.Next(t.Context(), []ToolResult{{Name: step.Calls[0].Name}})
	if len(step.Calls) != 1 || step.Calls[0].Name != "ops__get_deployment_status" {
		t.Fatalf("unexpected second step: %+v", step)
	}
	step, _ = script.Next(t.Context(), []ToolResult{{Name: step.Calls[0].Name, IsError: true, Denied: true}})
	if len(step.Calls) != 0 || !strings.Contains(step.Text, "ops__get_deployment_status: refused by gateway policy") ||
		!strings.Contains(step.Text, "Stopped at ops__get_deployment_status") {
		t.Fatalf("script continued after a refusal: %+v", step)
	}
}
