// Package agent is a small tool-using agent: it discovers tools on MCP
// servers, lets a Planner choose calls, executes them, and reports what
// happened. The planner is either a recorded script or a language model.
//
// The agent holds no authority of its own. Everything it can do is whatever
// the gateway permits its identity to do, which is the point of the demo.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// codeToolNotPermitted mirrors the gateway's JSON-RPC denial code.
	codeToolNotPermitted = -32090

	DefaultMaxSteps  = 12
	maxCallsPerStep  = 8
	maxResultBytes   = 4096
	maxToolNameBytes = 64
	separator        = "__"
)

var serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// ToolSpec describes one tool to a planner. Name is "<server>__<tool>".
type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ToolResult struct {
	ID      string
	Name    string
	Content string
	IsError bool
	// Denied is set when the gateway refused the call by policy.
	Denied bool
}

// Step is a planner's decision. A step without calls ends the run.
type Step struct {
	Text  string
	Calls []ToolCall
}

type Usage struct {
	ModelCalls   int
	InputTokens  int
	OutputTokens int
}

// Planner decides what to do next from the results of its previous step.
type Planner interface {
	Begin(task string, tools []ToolSpec)
	Next(ctx context.Context, results []ToolResult) (Step, error)
	Usage() Usage
}

// Server is an MCP endpoint, normally a gateway route.
type Server struct {
	Name     string
	Endpoint string
}

type Config struct {
	Servers    []Server
	HTTPClient *http.Client
	Planner    Planner
	Task       string
	MaxSteps   int
	Output     io.Writer
}

type Report struct {
	Results []ToolResult
	Final   string
	Usage   Usage
}

// Run executes one task. It stops when the planner finishes, when the step
// budget is spent, or when ctx ends. Tool calls are never retried.
func Run(ctx context.Context, cfg Config) (report Report, err error) {
	if cfg.Planner == nil || cfg.Output == nil || len(cfg.Servers) == 0 {
		return report, errors.New("planner, output and at least one server are required")
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = DefaultMaxSteps
	}
	sessions := map[string]*mcp.ClientSession{}
	defer func() {
		for _, session := range sessions {
			err = errors.Join(err, session.Close())
		}
	}()
	known := map[string]bool{}
	var tools []ToolSpec
	client := mcp.NewClient(&mcp.Implementation{Name: "portkeeper-agent", Version: "0.1.0"}, nil)
	for _, server := range cfg.Servers {
		if !serverName.MatchString(server.Name) || sessions[server.Name] != nil {
			return report, fmt.Errorf("invalid or duplicate server name %q", server.Name)
		}
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: server.Endpoint, HTTPClient: cfg.HTTPClient, MaxRetries: -1, DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			return report, fmt.Errorf("connect to %s: %w", server.Name, err)
		}
		sessions[server.Name] = session
		listed, err := session.ListTools(ctx, nil)
		if err != nil {
			return report, fmt.Errorf("discover tools on %s: %w", server.Name, err)
		}
		for _, tool := range listed.Tools {
			name := server.Name + separator + tool.Name
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil || len(name) > maxToolNameBytes {
				fmt.Fprintf(cfg.Output, "skip   %s: unusable name or schema\n", name)
				continue
			}
			known[name] = true
			tools = append(tools, ToolSpec{Name: name, Description: tool.Description, InputSchema: schema})
			fmt.Fprintf(cfg.Output, "tool   %s\n", name)
		}
	}

	cfg.Planner.Begin(cfg.Task, tools)
	fmt.Fprintf(cfg.Output, "task   %s\n", cfg.Task)
	var results []ToolResult
	for step := 0; step < cfg.MaxSteps; step++ {
		next, err := cfg.Planner.Next(ctx, results)
		report.Usage = cfg.Planner.Usage()
		if err != nil {
			return report, fmt.Errorf("planner: %w", err)
		}
		if len(next.Calls) == 0 {
			report.Final = next.Text
			fmt.Fprintf(cfg.Output, "final  %s\n", indent(next.Text))
			return report, nil
		}
		if next.Text != "" {
			fmt.Fprintf(cfg.Output, "agent  %s\n", indent(next.Text))
		}
		results = results[:0:0]
		for i, call := range next.Calls {
			var result ToolResult
			if i >= maxCallsPerStep {
				result = ToolResult{ID: call.ID, Name: call.Name, IsError: true, Content: "too many tool calls in one step; not executed"}
			} else {
				fmt.Fprintf(cfg.Output, "call   %s %s\n", call.Name, compact(call.Input))
				result = execute(ctx, sessions, known, call)
			}
			switch {
			case result.Denied:
				fmt.Fprintf(cfg.Output, "result DENIED %s\n", result.Content)
			case result.IsError:
				fmt.Fprintf(cfg.Output, "result error %s\n", result.Content)
			default:
				fmt.Fprintf(cfg.Output, "result ok %s\n", firstLine(result.Content))
			}
			results = append(results, result)
			report.Results = append(report.Results, result)
		}
	}
	return report, fmt.Errorf("step budget of %d exhausted before the planner finished", cfg.MaxSteps)
}

func execute(ctx context.Context, sessions map[string]*mcp.ClientSession, known map[string]bool, call ToolCall) ToolResult {
	result := ToolResult{ID: call.ID, Name: call.Name}
	server, tool, ok := strings.Cut(call.Name, separator)
	if !ok || !known[call.Name] {
		result.IsError, result.Content = true, "unknown tool; not executed"
		return result
	}
	args := map[string]any{}
	if len(call.Input) != 0 {
		if err := json.Unmarshal(call.Input, &args); err != nil {
			result.IsError, result.Content = true, "tool input must be a JSON object; not executed"
			return result
		}
	}
	res, err := sessions[server].CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		var rpcErr *jsonrpc.Error
		result.IsError = true
		if errors.As(err, &rpcErr) && rpcErr.Code == codeToolNotPermitted {
			result.Denied = true
			result.Content = "by gateway policy: " + rpcErr.Message
			return result
		}
		result.Content = bound(err.Error())
		return result
	}
	var text []string
	for _, content := range res.Content {
		if t, ok := content.(*mcp.TextContent); ok {
			text = append(text, t.Text)
		}
	}
	result.IsError = res.IsError
	result.Content = bound(strings.Join(text, "\n"))
	return result
}

// bound keeps one tool result from dominating the model's context and cost.
func bound(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxResultBytes], "") + "\n[truncated]"
}

func compact(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return bound(string(raw))
}

func firstLine(s string) string {
	line, rest, _ := strings.Cut(s, "\n")
	if len(line) > 200 {
		return line[:200] + " ..."
	}
	if rest != "" {
		return line + " ..."
	}
	return line
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n       ")
}
