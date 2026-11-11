// Package opsmcp is a deliberately narrow MCP backend that changes state: it
// inspects, restarts and scales Deployments in one namespace. It exists so
// that gateway tool policy has a consequential action to permit or refuse.
package opsmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	StatusTool  = "get_deployment_status"
	RestartTool = "restart_deployment"
	ScaleTool   = "scale_deployment"

	// ManagedLabel must be "true" on a Deployment before any tool will touch it.
	ManagedLabel = "portkeeper.dev/agent-managed"
	// RestartedByAnnotation records the gateway-verified caller on the pod template.
	RestartedByAnnotation = "portkeeper.dev/restarted-by"

	restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
	maxWaitSeconds        = 60
	minReplicas           = 1
	maxReplicas           = 5
)

var deploymentName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

type statusInput struct {
	Name        string `json:"name" jsonschema:"Deployment name"`
	WaitSeconds int    `json:"waitSeconds,omitempty" jsonschema:"Optional: wait up to this many seconds (maximum 60) for the Deployment to become ready before answering"`
}

type restartInput struct {
	Name string `json:"name" jsonschema:"Deployment name"`
}

type scaleInput struct {
	Name     string `json:"name" jsonschema:"Deployment name"`
	Replicas int    `json:"replicas" jsonschema:"Desired replica count, from 1 to 5"`
}

// Status is the JSON document returned by every tool.
type Status struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	DesiredReplicas   int32  `json:"desiredReplicas"`
	UpdatedReplicas   int32  `json:"updatedReplicas"`
	ReadyReplicas     int32  `json:"readyReplicas"`
	AvailableReplicas int32  `json:"availableReplicas"`
	Ready             bool   `json:"ready"`
	LastRestartedAt   string `json:"lastRestartedAt,omitempty"`
	LastRestartedBy   string `json:"lastRestartedBy,omitempty"`
}

type server struct {
	client    kubernetes.Interface
	namespace string
	poll      time.Duration
}

// NewHandler serves the ops tools over MCP for Deployments in one namespace.
// The Kubernetes credential behind client should itself be limited to that
// namespace; the checks here are a second boundary, not the only one.
func NewHandler(client kubernetes.Interface, namespace string) http.Handler {
	s := &server{client: client, namespace: namespace, poll: time.Second}
	srv := mcp.NewServer(&mcp.Implementation{Name: "portkeeper-ops", Version: "0.1.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        StatusTool,
		Description: "Read-only. Report desired, updated, ready and available replicas of a Deployment in the sandbox namespace.",
	}, s.status)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        RestartTool,
		Description: "Changes state. Restart a sandbox Deployment by rolling its pods, so they start again with current configuration.",
	}, s.restart)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        ScaleTool,
		Description: "Changes state. Set the replica count of a sandbox Deployment (1 to 5).",
	}, s.scale)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		SessionTimeout:               5 * time.Minute,
		PropagateRequestCancellation: true,
	})
}

func (s *server) status(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, any, error) {
	if in.WaitSeconds < 0 || in.WaitSeconds > maxWaitSeconds {
		return nil, nil, fmt.Errorf("waitSeconds must be between 0 and %d", maxWaitSeconds)
	}
	deadline := time.Now().Add(time.Duration(in.WaitSeconds) * time.Second)
	for {
		dep, err := s.managed(ctx, in.Name)
		if err != nil {
			return nil, nil, err
		}
		st := describe(dep)
		if st.Ready || !time.Now().Before(deadline) {
			return result(st)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(s.poll):
		}
	}
}

func (s *server) restart(ctx context.Context, req *mcp.CallToolRequest, in restartInput) (*mcp.CallToolResult, any, error) {
	if _, err := s.managed(ctx, in.Name); err != nil {
		return nil, nil, err
	}
	agent := caller(req)
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{
			restartedAtAnnotation: time.Now().UTC().Format(time.RFC3339),
			RestartedByAnnotation: agent,
		},
	}}}})
	if err != nil {
		return nil, nil, err
	}
	return s.apply(ctx, RestartTool, in.Name, agent, patch)
}

func (s *server) scale(ctx context.Context, req *mcp.CallToolRequest, in scaleInput) (*mcp.CallToolResult, any, error) {
	if in.Replicas < minReplicas || in.Replicas > maxReplicas {
		return nil, nil, fmt.Errorf("replicas must be between %d and %d", minReplicas, maxReplicas)
	}
	if _, err := s.managed(ctx, in.Name); err != nil {
		return nil, nil, err
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"replicas": in.Replicas}})
	if err != nil {
		return nil, nil, err
	}
	return s.apply(ctx, ScaleTool, in.Name, caller(req), patch)
}

func (s *server) apply(ctx context.Context, tool, name, agent string, patch []byte) (*mcp.CallToolResult, any, error) {
	dep, err := s.client.AppsV1().Deployments(s.namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	log.Printf("ops_action tool=%s namespace=%q deployment=%q agent=%q applied=%t", tool, s.namespace, name, agent, err == nil)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", tool, name, err)
	}
	return result(describe(dep))
}

// managed fetches a Deployment only if it has opted in to agent management.
func (s *server) managed(ctx context.Context, name string) (*appsv1.Deployment, error) {
	if !deploymentName.MatchString(name) {
		return nil, errors.New("name must be a valid Deployment name")
	}
	dep, err := s.client.AppsV1().Deployments(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read deployment %s: %w", name, err)
	}
	if dep.Labels[ManagedLabel] != "true" {
		return nil, fmt.Errorf("deployment %s is not labelled %s=true", name, ManagedLabel)
	}
	return dep, nil
}

func describe(dep *appsv1.Deployment) Status {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	st := dep.Status
	return Status{
		Name: dep.Name, Namespace: dep.Namespace, DesiredReplicas: desired,
		UpdatedReplicas: st.UpdatedReplicas, ReadyReplicas: st.ReadyReplicas, AvailableReplicas: st.AvailableReplicas,
		// Old pods that are still ready must not make an unfinished rollout look healthy.
		Ready: desired > 0 && st.ObservedGeneration >= dep.Generation && st.Replicas == desired &&
			st.UpdatedReplicas == desired && st.ReadyReplicas == desired && st.AvailableReplicas == desired,
		LastRestartedAt: dep.Spec.Template.Annotations[restartedAtAnnotation],
		LastRestartedBy: dep.Spec.Template.Annotations[RestartedByAnnotation],
	}
}

func result(st Status) (*mcp.CallToolResult, any, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
}

// caller is the identity the gateway verified and wrote into X-Agent-ID.
// It is trustworthy only because NetworkPolicy admits the gateway alone.
func caller(req *mcp.CallToolRequest) string {
	agent := ""
	if req != nil && req.Extra != nil {
		agent = req.Extra.Header.Get("X-Agent-ID")
	}
	if agent == "" {
		return "unknown"
	}
	if len(agent) > 253 {
		agent = agent[:253]
	}
	return agent
}
