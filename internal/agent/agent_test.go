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

func TestOpenAIPlannerToolLoop(t *testing.T) {
	const firstMessage = `{"role":"assistant","content":null,"reasoning":"plan",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"ops__scale_deployment","arguments":"{\"name\":\"checkout\",\"replicas\":3}"}}]}`
	var requests []map[string]json.RawMessage
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		auth = append(auth, r.Header.Get("Authorization"))
		var request map[string]json.RawMessage
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		switch len(requests) {
		case 1:
			// Hosted reasoning models refuse the older limit parameter.
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"Unsupported parameter: 'max_tokens'. Use 'max_completion_tokens' instead."}}`)
		case 2:
			io.WriteString(w, `{"choices":[{"message":`+firstMessage+`,"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":90,"completion_tokens":15}}`)
		default:
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Scaling was refused."},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":8}}`)
		}
	}))
	defer server.Close()

	planner := &OpenAI{BaseURL: server.URL + "/v1", Model: "local-model", RetryDelay: time.Millisecond}
	planner.Begin("resolve it", []ToolSpec{{Name: "ops__scale_deployment", Description: "scale", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	step, err := planner.Next(t.Context(), nil)
	if err != nil || len(step.Calls) != 1 || step.Calls[0].ID != "call_1" || step.Calls[0].Name != "ops__scale_deployment" ||
		string(step.Calls[0].Input) != `{"name":"checkout","replicas":3}` {
		t.Fatalf("unexpected first step: %+v %v", step, err)
	}
	planner.KeyFile = keyFile(t)
	step, err = planner.Next(t.Context(), []ToolResult{{ID: "call_1", Content: "by gateway policy: no", IsError: true, Denied: true}})
	if err != nil || len(step.Calls) != 0 || step.Text != "Scaling was refused." {
		t.Fatalf("unexpected final step: %+v %v", step, err)
	}
	if usage := planner.Usage(); usage != (Usage{ModelCalls: 2, InputTokens: 210, OutputTokens: 23}) {
		t.Fatalf("usage = %+v", usage)
	}
	// A keyless endpoint gets no credential header; a key becomes a bearer token.
	if len(auth) != 3 || auth[0] != "" || auth[1] != "" || auth[2] != "Bearer "+testKey {
		t.Fatalf("authorization headers = %q", auth)
	}
	if requests[0]["max_tokens"] == nil || requests[1]["max_completion_tokens"] == nil || requests[1]["max_tokens"] != nil ||
		requests[2]["max_completion_tokens"] == nil || !strings.Contains(string(requests[1]["tools"]), `"parameters":{"type":"object"}`) {
		t.Fatalf("unexpected request parameters: %v", requests)
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(requests[2]["messages"], &messages); err != nil || len(messages) != 4 {
		t.Fatalf("conversation = %s", requests[2]["messages"])
	}
	if string(messages[2]) != firstMessage {
		t.Fatalf("assistant turn was not echoed verbatim: %s", messages[2])
	}
	if want := `{"content":"DENIED by gateway policy: no","role":"tool","tool_call_id":"call_1"}`; string(messages[3]) != want {
		t.Fatalf("tool result = %s", messages[3])
	}
}

func TestOpenAIPlannerAssignsMissingCallIDs(t *testing.T) {
	var second map[string]json.RawMessage
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"ops__get_deployment_status","arguments":""}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &second)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	planner := &OpenAI{BaseURL: server.URL, Model: "local-model"}
	planner.Begin("task", nil)
	step, err := planner.Next(t.Context(), nil)
	if err != nil || len(step.Calls) != 1 || step.Calls[0].ID == "" || string(step.Calls[0].Input) != "{}" {
		t.Fatalf("unexpected step: %+v %v", step, err)
	}
	if _, err := planner.Next(t.Context(), []ToolResult{{ID: step.Calls[0].ID, Content: "ok"}}); err != nil {
		t.Fatal(err)
	}
	// The assigned id must appear on both the stored call and its result.
	if strings.Count(string(second["messages"]), `"`+step.Calls[0].ID+`"`) != 2 {
		t.Fatalf("call id not consistent in conversation: %s", second["messages"])
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
