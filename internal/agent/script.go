package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// IncidentTask is the prompt for the checkout scenario, for either planner.
const IncidentTask = "The checkout Deployment in the sandbox has no ready replicas. " +
	"Read the checkout-not-ready runbook and resolve the incident by following it."

// Script replays a fixed sequence of tool calls without a model. It stops at
// the first call that fails or is refused, as the runbook instructs.
type Script struct {
	calls   []ToolCall
	next    int
	history []ToolResult
}

// IncidentScript is the deterministic replay of the checkout scenario: read
// the runbook, inspect, restart, verify, then attempt the scale-up.
func IncidentScript(waitSeconds int) *Script {
	call := func(name string, input any) ToolCall {
		data, _ := json.Marshal(input)
		return ToolCall{Name: name, Input: data}
	}
	return &Script{calls: []ToolCall{
		call("runbooks__read_runbook", map[string]any{"topic": "checkout-not-ready"}),
		call("ops__get_deployment_status", map[string]any{"name": "checkout"}),
		call("ops__restart_deployment", map[string]any{"name": "checkout"}),
		call("ops__get_deployment_status", map[string]any{"name": "checkout", "waitSeconds": waitSeconds}),
		call("ops__scale_deployment", map[string]any{"name": "checkout", "replicas": 3}),
	}}
}

func (s *Script) Begin(string, []ToolSpec) {}

func (s *Script) Usage() Usage { return Usage{} }

func (s *Script) Next(_ context.Context, results []ToolResult) (Step, error) {
	s.history = append(s.history, results...)
	failed := len(results) != 0 && results[len(results)-1].IsError
	if failed || s.next == len(s.calls) {
		return Step{Text: s.summary()}, nil
	}
	call := s.calls[s.next]
	call.ID = fmt.Sprintf("script-%d", s.next+1)
	s.next++
	return Step{Calls: []ToolCall{call}}, nil
}

func (s *Script) summary() string {
	var lines []string
	for _, result := range s.history {
		outcome := "completed"
		if result.Denied {
			outcome = "refused by gateway policy"
		} else if result.IsError {
			outcome = "failed"
		}
		lines = append(lines, fmt.Sprintf("%s: %s", result.Name, outcome))
	}
	if n := len(s.history); n != 0 && s.history[n-1].IsError {
		lines = append(lines, fmt.Sprintf("Stopped at %s as the runbook requires; a human must decide the next step.", s.history[n-1].Name))
	}
	return "Scripted replay finished.\n" + strings.Join(lines, "\n")
}
