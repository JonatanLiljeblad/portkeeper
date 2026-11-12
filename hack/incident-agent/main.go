// Command incident-agent works the checkout incident through the gateway,
// either as a recorded script or with a language model choosing the steps.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jonatan/portkeeper/internal/agent"
	"github.com/jonatan/portkeeper/internal/democlient"
)

func main() {
	runbooks := flag.String("runbooks-endpoint", "", "Gateway MCP endpoint of the runbooks server")
	ops := flag.String("ops-endpoint", "", "Gateway MCP endpoint of the ops server")
	tokenFile := flag.String("token-file", "", "Gateway bearer token file, reread on every request")
	planner := flag.String("planner", "script", "script (deterministic replay, no model), anthropic, or openai (any Chat Completions compatible API, including a local model server)")
	model := flag.String("model", "", "Model name; required for openai, defaults to "+agent.DefaultAnthropicModel+" for anthropic")
	keyFile := flag.String("llm-key-file", "", "File holding the model API key; required for anthropic, optional for openai")
	baseURL := flag.String("llm-base-url", "", "Model API base URL; defaults to the provider's hosted API")
	maxSteps := flag.Int("max-steps", agent.DefaultMaxSteps, "Maximum planner steps; bounds model calls and cost")
	wait := flag.Int("script-wait-seconds", 60, "How long the script waits for the restart to finish (0-60)")
	timeout := flag.Duration("timeout", 10*time.Minute, "Deadline for the whole run")
	flag.Parse()
	if flag.NArg() != 0 || *runbooks == "" || *ops == "" || *tokenFile == "" || *timeout <= 0 || *maxSteps < 1 {
		log.Fatal("runbooks-endpoint, ops-endpoint and token-file are required; timeout and max-steps must be positive")
	}

	var plan agent.Planner
	switch *planner {
	case "script":
		plan = agent.IncidentScript(*wait)
	case "anthropic":
		if *keyFile == "" {
			log.Fatal("llm-key-file is required for the anthropic planner")
		}
		if *model == "" {
			*model = agent.DefaultAnthropicModel
		}
		plan = &agent.Anthropic{BaseURL: *baseURL, Model: *model, KeyFile: *keyFile}
		fmt.Printf("model  %s (anthropic)\n", *model)
	case "openai":
		if *model == "" {
			log.Fatal("model is required for the openai planner")
		}
		plan = &agent.OpenAI{BaseURL: *baseURL, Model: *model, KeyFile: *keyFile}
		fmt.Printf("model  %s (openai-compatible)\n", *model)
	default:
		log.Fatal("planner must be script, anthropic or openai")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := agent.Run(ctx, agent.Config{
		Servers:    []agent.Server{{Name: "runbooks", Endpoint: *runbooks}, {Name: "ops", Endpoint: *ops}},
		HTTPClient: democlient.HTTPClient(democlient.Credentials{TokenFile: *tokenFile}),
		Planner:    plan, Task: agent.IncidentTask, MaxSteps: *maxSteps, Output: os.Stdout,
	})
	denied := 0
	for _, result := range report.Results {
		if result.Denied {
			denied++
		}
	}
	fmt.Printf("usage  tool_calls=%d denied=%d model_calls=%d input_tokens=%d output_tokens=%d\n",
		len(report.Results), denied, report.Usage.ModelCalls, report.Usage.InputTokens, report.Usage.OutputTokens)
	if err != nil {
		log.Fatal(err)
	}
}
