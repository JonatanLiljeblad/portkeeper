package gateway

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
	"github.com/jonatan/portkeeper/internal/agent"
	"github.com/jonatan/portkeeper/internal/opsmcp"
	"github.com/jonatan/portkeeper/internal/runbookmcp"
)

// The scripted incident runs through the real router against both backends:
// the restart is applied, the scale-up is refused before reaching the backend.
func TestIncidentAgentThroughGateway(t *testing.T) {
	replicas := int32(1)
	cluster := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "sandbox", Labels: map[string]string{opsmcp.ManagedLabel: "true"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	})
	reg := &Registry{backends: map[types.NamespacedName]Backend{}}
	for name, backend := range map[string]struct {
		handler http.Handler
		tools   []string
	}{
		"runbooks": {runbookmcp.NewHandler(), []string{runbookmcp.ReadTool}},
		"ops":      {opsmcp.NewHandler(cluster, "sandbox"), []string{opsmcp.StatusTool, opsmcp.RestartTool}},
	} {
		server := httptest.NewServer(backend.handler)
		t.Cleanup(server.Close)
		reg.backends[types.NamespacedName{Namespace: "incident", Name: name}] = Backend{
			Namespace: "incident", Name: name, Ready: true, Address: strings.TrimPrefix(server.URL, "http://"),
			AllowedServiceAccounts: []mcpv1alpha1.ServiceAccountReference{{Namespace: "test", Name: "agent", AllowedTools: backend.tools}},
			PolicyObservedAt:       time.Now(),
		}
	}
	router := NewRouter(reg, NewAgentLimiter(1000, 1000), testAuthenticator(t))
	audit := &lockedBuffer{}
	router.AuditLog = slog.New(slog.NewJSONHandler(audit, nil))
	gw := httptest.NewServer(router)
	t.Cleanup(gw.Close)

	var transcript bytes.Buffer
	report, err := agent.Run(t.Context(), agent.Config{
		Servers: []agent.Server{
			{Name: "runbooks", Endpoint: gw.URL + "/incident/runbooks/mcp"},
			{Name: "ops", Endpoint: gw.URL + "/incident/ops/mcp"},
		},
		HTTPClient: &http.Client{Transport: agentTransport{base: http.DefaultTransport}},
		Planner:    agent.IncidentScript(0), Task: agent.IncidentTask, Output: &transcript,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, &transcript)
	}
	var outcomes []string
	for _, result := range report.Results {
		outcome := "ok"
		if result.Denied {
			outcome = "denied"
		} else if result.IsError {
			outcome = "error"
		}
		outcomes = append(outcomes, result.Name+"="+outcome)
	}
	want := "runbooks__read_runbook=ok ops__get_deployment_status=ok ops__restart_deployment=ok ops__get_deployment_status=ok ops__scale_deployment=denied"
	if strings.Join(outcomes, " ") != want {
		t.Fatalf("outcomes = %v\n%s", outcomes, &transcript)
	}
	if !strings.Contains(transcript.String(), "result DENIED by gateway policy") || !strings.Contains(report.Final, "Stopped at ops__scale_deployment") {
		t.Fatalf("denial not reported:\n%s", &transcript)
	}

	dep, err := cluster.AppsV1().Deployments("sandbox").Get(t.Context(), "checkout", metav1.GetOptions{})
	if err != nil || *dep.Spec.Replicas != 1 || dep.Spec.Template.Annotations[opsmcp.RestartedByAnnotation] != testPrincipal {
		t.Fatalf("sandbox state: replicas=%d annotations=%v err=%v", *dep.Spec.Replicas, dep.Spec.Template.Annotations, err)
	}
	var decisions []string
	for _, event := range audit.events(t) {
		if event["agent"] != testPrincipal {
			t.Fatalf("audit event without the verified identity: %v", event)
		}
		decisions = append(decisions, event["server"].(string)+"/"+event["tool"].(string)+"="+event["decision"].(string))
	}
	wantAudit := "runbooks/read_runbook=allow ops/get_deployment_status=allow ops/restart_deployment=allow ops/get_deployment_status=allow ops/scale_deployment=deny"
	if strings.Join(decisions, " ") != wantAudit {
		t.Fatalf("audit trail = %v", decisions)
	}
}
