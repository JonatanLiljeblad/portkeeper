package opsmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type agentHeader struct{ base http.RoundTripper }

func (t agentHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("X-Agent-ID", "system:serviceaccount:demo:agent")
	return t.base.RoundTrip(out)
}

func deployment(name string, managed bool, ready int32) *appsv1.Deployment {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "sandbox", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: ready, AvailableReplicas: ready,
		},
	}
	if managed {
		dep.Labels = map[string]string{ManagedLabel: "true"}
	}
	return dep
}

func TestOpsTools(t *testing.T) {
	elsewhere := deployment("checkout", true, 1)
	elsewhere.Namespace = "other"
	client := fake.NewClientset(deployment("checkout", true, 0), deployment("payments", false, 1), elsewhere)
	httpServer := httptest.NewServer(NewHandler(client, "sandbox"))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{
			Endpoint:   httpServer.URL,
			HTTPClient: &http.Client{Transport: agentHeader{http.DefaultTransport}},
			MaxRetries: -1,
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	call := func(tool string, args map[string]any) (Status, string, bool) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		var st Status
		if !res.IsError {
			if err := json.Unmarshal([]byte(text), &st); err != nil {
				t.Fatalf("%s returned non-JSON %q", tool, text)
			}
		}
		return st, text, res.IsError
	}
	stored := func(namespace, name string) *appsv1.Deployment {
		t.Helper()
		dep, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return dep
	}

	if st, _, isErr := call(StatusTool, map[string]any{"name": "checkout"}); isErr || st.Ready || st.DesiredReplicas != 1 || st.ReadyReplicas != 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if st, _, isErr := call(RestartTool, map[string]any{"name": "checkout"}); isErr || st.LastRestartedBy != "system:serviceaccount:demo:agent" || st.LastRestartedAt == "" {
		t.Fatalf("restart did not record the verified caller: %+v", st)
	}
	if got := stored("sandbox", "checkout").Spec.Template.Annotations[RestartedByAnnotation]; got != "system:serviceaccount:demo:agent" {
		t.Fatalf("restart annotation = %q", got)
	}
	if st, _, isErr := call(ScaleTool, map[string]any{"name": "checkout", "replicas": 3}); isErr || st.DesiredReplicas != 3 || *stored("sandbox", "checkout").Spec.Replicas != 3 {
		t.Fatalf("scale was not applied: %+v", st)
	}

	for _, tt := range []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"unmanaged-restart", RestartTool, map[string]any{"name": "payments"}, "not labelled"},
		{"unmanaged-scale", ScaleTool, map[string]any{"name": "payments", "replicas": 2}, "not labelled"},
		{"unmanaged-status", StatusTool, map[string]any{"name": "payments"}, "not labelled"},
		{"missing", RestartTool, map[string]any{"name": "absent"}, "not found"},
		{"path-like-name", RestartTool, map[string]any{"name": "../other/checkout"}, "valid Deployment name"},
		{"too-many-replicas", ScaleTool, map[string]any{"name": "checkout", "replicas": 6}, "between 1 and 5"},
		{"zero-replicas", ScaleTool, map[string]any{"name": "checkout", "replicas": 0}, "between 1 and 5"},
		{"excessive-wait", StatusTool, map[string]any{"name": "checkout", "waitSeconds": 600}, "between 0 and 60"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, text, isErr := call(tt.tool, tt.args)
			if !isErr || !strings.Contains(text, tt.want) {
				t.Fatalf("result = %q (error=%t), want error containing %q", text, isErr, tt.want)
			}
		})
	}
	// Refusals leave state untouched, and nothing outside the namespace moved.
	for _, dep := range []*appsv1.Deployment{stored("sandbox", "payments"), stored("other", "checkout")} {
		if *dep.Spec.Replicas != 1 || len(dep.Spec.Template.Annotations) != 0 {
			t.Fatalf("%s/%s was modified: %+v", dep.Namespace, dep.Name, dep.Spec)
		}
	}
	if *stored("sandbox", "checkout").Spec.Replicas != 3 {
		t.Fatal("a refused scale changed the replica count")
	}
}

func TestStatusWaitsForReadiness(t *testing.T) {
	client := fake.NewClientset(deployment("checkout", true, 0))
	s := &server{client: client, namespace: "sandbox", poll: 10 * time.Millisecond}
	go func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := client.AppsV1().Deployments("sandbox").UpdateStatus(context.Background(), deployment("checkout", true, 1), metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	res, _, err := s.status(t.Context(), nil, statusInput{Name: "checkout", WaitSeconds: 5})
	if err != nil || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"ready":true`) || time.Since(start) > 3*time.Second {
		t.Fatalf("wait did not return on readiness: %v %+v", err, res)
	}
	// Without a wait, an unready Deployment answers immediately.
	idle := &server{client: fake.NewClientset(deployment("checkout", true, 0)), namespace: "sandbox", poll: time.Hour}
	res, _, err = idle.status(t.Context(), nil, statusInput{Name: "checkout"})
	if err != nil || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"ready":false`) {
		t.Fatalf("unexpected immediate status: %v %+v", err, res)
	}
}
