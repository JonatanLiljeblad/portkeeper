package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultOpenAIBaseURL is the hosted API. Any server that implements the
// Chat Completions tool-calling API can be used instead, including a local
// model server.
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

// OpenAI plans with the Chat Completions API, which many providers and local
// model servers implement. KeyFile is optional: a local server needs no key.
type OpenAI struct {
	BaseURL    string
	Model      string
	KeyFile    string
	HTTPClient *http.Client
	RetryDelay time.Duration

	tools    []openAITool
	messages []json.RawMessage
	usage    Usage
	// Newer hosted models reject max_tokens and require this name instead.
	completionTokensParam bool
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (o *OpenAI) Begin(task string, tools []ToolSpec) {
	o.tools = o.tools[:0]
	for _, tool := range tools {
		o.tools = append(o.tools, openAITool{Type: "function", Function: openAIFunction{
			Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema,
		}})
	}
	o.messages = []json.RawMessage{
		chatMessage(map[string]string{"role": "system", "content": systemPrompt}),
		chatMessage(map[string]string{"role": "user", "content": task}),
	}
}

func (o *OpenAI) Usage() Usage { return o.usage }

func (o *OpenAI) Next(ctx context.Context, results []ToolResult) (Step, error) {
	if o.Model == "" {
		return Step{}, errors.New("a model name is required")
	}
	for _, result := range results {
		content := result.Content
		switch {
		case result.Denied:
			content = "DENIED " + content
		case result.IsError:
			content = "ERROR " + content
		}
		o.messages = append(o.messages, chatMessage(map[string]string{
			"role": "tool", "tool_call_id": result.ID, "content": content,
		}))
	}
	var reply struct {
		Choices []struct {
			Message      json.RawMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := o.post(ctx, &reply); err != nil {
		return Step{}, err
	}
	o.usage.ModelCalls++
	o.usage.InputTokens += reply.Usage.PromptTokens
	o.usage.OutputTokens += reply.Usage.CompletionTokens
	if len(reply.Choices) == 0 {
		return Step{}, errors.New("model reply has no choices")
	}
	var message struct {
		Content   *string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(reply.Choices[0].Message, &message); err != nil {
		return Step{}, errors.New("model reply has an unreadable message")
	}
	if reply.Choices[0].FinishReason == "length" {
		return Step{}, errors.New("model reply was cut off at the output token limit")
	}
	var step Step
	if message.Content != nil {
		step.Text = strings.TrimSpace(*message.Content)
	}
	// Tool results are matched to calls by id. A server that omits ids gets
	// them assigned here, in the stored turn as well as in the step.
	stored := reply.Choices[0].Message
	missingID := false
	for i, call := range message.ToolCalls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call-%d-%d", o.usage.ModelCalls, i+1)
			missingID = true
		}
		input := json.RawMessage(call.Function.Arguments)
		if strings.TrimSpace(call.Function.Arguments) == "" {
			input = json.RawMessage("{}")
		}
		step.Calls = append(step.Calls, ToolCall{ID: id, Name: call.Function.Name, Input: input})
	}
	if missingID {
		calls := make([]map[string]any, 0, len(step.Calls))
		for _, call := range step.Calls {
			calls = append(calls, map[string]any{"id": call.ID, "type": "function",
				"function": map[string]string{"name": call.Name, "arguments": string(call.Input)}})
		}
		stored = chatMessage(map[string]any{"role": "assistant", "content": step.Text, "tool_calls": calls})
	}
	// Otherwise the turn is stored verbatim, so fields this client does not
	// interpret are returned to the API unchanged.
	o.messages = append(o.messages, stored)
	return step, nil
}

func (o *OpenAI) post(ctx context.Context, out any) error {
	base := o.BaseURL
	if base == "" {
		base = DefaultOpenAIBaseURL
	}
	endpoint := modelEndpoint{
		baseURL: base, keyFile: o.KeyFile, client: o.HTTPClient, retryDelay: o.RetryDelay,
		authorize: func(req *http.Request, key string) {
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
		},
	}
	for {
		limit := "max_tokens"
		if o.completionTokensParam {
			limit = "max_completion_tokens"
		}
		body, err := json.Marshal(map[string]any{
			"model": o.Model, "messages": o.messages, "tools": o.tools, limit: maxOutputTokens,
		})
		if err != nil {
			return err
		}
		err = endpoint.post(ctx, "/chat/completions", body, out)
		var apiErr *modelAPIError
		if !o.completionTokensParam && errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest &&
			strings.Contains(apiErr.Detail, "max_completion_tokens") {
			o.completionTokensParam = true
			continue
		}
		return err
	}
}

func chatMessage(message any) json.RawMessage {
	data, _ := json.Marshal(message)
	return data
}
