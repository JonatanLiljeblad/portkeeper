package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type countingClient struct {
	client.Client
	writes, statusWrites int
	createError          error
	statusError          error
}

func (c *countingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.writes++
	if c.createError != nil {
		return c.createError
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *countingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.writes++
	return c.Client.Update(ctx, obj, opts...)
}

func (c *countingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.writes++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *countingClient) Status() client.SubResourceWriter {
	return &countingStatus{SubResourceWriter: c.Client.Status(), client: c}
}

type countingStatus struct {
	client.SubResourceWriter
	client *countingClient
}

func (s *countingStatus) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	s.client.statusWrites++
	if s.client.statusError != nil {
		return s.client.statusError
	}
	return s.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (s *countingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.client.statusWrites++
	if s.client.statusError != nil {
		return s.client.statusError
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

func newServer() *mcpv1alpha1.MCPServer {
	return &mcpv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "server-uid", Generation: 1},
		Spec:       mcpv1alpha1.MCPServerSpec{Image: "server:v1", Port: 8080},
	}
}

func setup(t *testing.T, objects ...client.Object) (*MCPServerReconciler, *countingClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{mcpv1alpha1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := &countingClient{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&mcpv1alpha1.MCPServer{}, &appsv1.Deployment{}).WithObjects(objects...).Build()}
	return &MCPServerReconciler{Client: c, Scheme: scheme}, c
}

func reconcile(t *testing.T, r *MCPServerReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newServer())}); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, c client.Client, object client.Object, name string) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, object); err != nil {
		t.Fatal(err)
	}
}

func assertStatus(t *testing.T, c client.Client, phase string, ready bool) *mcpv1alpha1.MCPServer {
	t.Helper()
	m := &mcpv1alpha1.MCPServer{}
	get(t, c, m, "test")
	condition := meta.FindStatusCondition(m.Status.Conditions, mcpv1alpha1.MCPServerReady)
	if m.Status.Phase != phase || m.IsReady() != ready || m.Status.ObservedGeneration != m.Generation ||
		condition == nil || condition.ObservedGeneration != m.Generation {
		t.Fatalf("unexpected status: %#v (ready=%v), want phase=%s ready=%v", m.Status, m.IsReady(), phase, ready)
	}
	return m
}

func setDeploymentStatus(t *testing.T, c client.Client, status appsv1.DeploymentStatus) {
	t.Helper()
	dep := &appsv1.Deployment{}
	get(t, c, dep, "test")
	dep.Status = status
	if err := c.Status().Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

func availableStatus(generation int64) appsv1.DeploymentStatus {
	return appsv1.DeploymentStatus{ObservedGeneration: generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
}

func TestReadinessLifecycle(t *testing.T) {
	r, c := setup(t, newServer())
	reconcile(t, r)
	assertStatus(t, c, "Pending", false)

	dep := &appsv1.Deployment{}
	get(t, c, dep, "test")
	// The fake client does not increment Deployment generations.
	dep.Generation = 2
	if err := c.Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		status appsv1.DeploymentStatus
		phase  string
	}{
		{"stale observed generation", availableStatus(1), "Pending"},
		{"available", availableStatus(2), "Ready"},
		{"unavailable", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1}, "Pending"},
		{"old replicas ready", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}, "Pending"},
		{"surging old replica ready", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}, "Pending"},
		{"not available yet", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}, "Pending"},
		{"deadline exceeded", appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "rollout timed out",
		}}}, "Failed"},
		{"replica failure", appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: "quota exceeded",
		}}}, "Failed"},
		{"recovery", availableStatus(2), "Ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDeploymentStatus(t, c, tt.status)
			reconcile(t, r)
			assertStatus(t, c, tt.phase, tt.phase == "Ready")
		})
	}
}

func TestManagedFieldsAndIdempotence(t *testing.T) {
	r, c := setup(t, newServer())
	reconcile(t, r)
	dep := &appsv1.Deployment{}
	svc := &corev1.Service{}
	get(t, c, dep, "test")
	get(t, c, svc, "test-svc")
	container := &dep.Spec.Template.Spec.Containers[0]
	if container.Name != "mcp-server" || container.Image != "server:v1" || container.Ports[0].ContainerPort != 8080 ||
		container.ReadinessProbe.TCPSocket.Port.IntVal != 8080 {
		t.Fatalf("unexpected container: %#v", container)
	}
	dep.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	dep.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	dep.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	container.ImagePullPolicy = corev1.PullIfNotPresent
	container.TerminationMessagePath = "/dev/termination-log"
	container.ReadinessProbe.PeriodSeconds = 10
	container.ReadinessProbe.TimeoutSeconds = 1
	container.ReadinessProbe.SuccessThreshold = 1
	container.ReadinessProbe.FailureThreshold = 3
	dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar", Image: "sidecar:v1"})
	if err := c.Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	svc.Spec.ClusterIP = "10.0.0.10"
	svc.Spec.ClusterIPs = []string{"10.0.0.10"}
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.SessionAffinity = corev1.ServiceAffinityNone
	if err := c.Update(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	setDeploymentStatus(t, c, availableStatus(dep.Generation))
	reconcile(t, r)
	m := assertStatus(t, c, "Ready", true)
	before := m.DeepCopy()
	c.writes, c.statusWrites = 0, 0
	reconcile(t, r)
	reconcile(t, r)
	after := assertStatus(t, c, "Ready", true)
	if c.writes != 0 || c.statusWrites != 0 || !equality.Semantic.DeepEqual(before.Status, after.Status) {
		t.Fatalf("reconcile churn: resource writes=%d status writes=%d", c.writes, c.statusWrites)
	}
	get(t, c, dep, "test")
	expected := dep.Spec.DeepCopy()
	m.Spec.Image, m.Spec.Port, m.Generation = "server:v2", 9090, 2
	if err := c.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	stale := &mcpv1alpha1.MCPServer{}
	get(t, c, stale, "test")
	if stale.IsReady() {
		t.Fatal("old status must not be ready after desired generation changes")
	}
	reconcile(t, r)
	assertStatus(t, c, "Pending", false)
	get(t, c, dep, "test")
	get(t, c, svc, "test-svc")
	expected.Template.Spec.Containers[0].Image = "server:v2"
	expected.Template.Spec.Containers[0].Ports[0].ContainerPort = 9090
	expected.Template.Spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntVal = 9090
	if !equality.Semantic.DeepEqual(expected, &dep.Spec) {
		t.Fatalf("unexpected managed Deployment mutation:\ngot %#v\nwant %#v", dep.Spec, expected)
	}
	if svc.Spec.ClusterIP != "10.0.0.10" || len(svc.Spec.ClusterIPs) != 1 || svc.Spec.ClusterIPs[0] != "10.0.0.10" ||
		svc.Spec.Ports[0].Port != 9090 || svc.Spec.Ports[0].TargetPort.IntVal != 9090 {
		t.Fatalf("unexpected Service mutation: %#v", svc.Spec)
	}
	dep.Generation++
	if err := c.Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	assertStatus(t, c, "Pending", false)
	setDeploymentStatus(t, c, availableStatus(dep.Generation))
	reconcile(t, r)
	assertStatus(t, c, "Ready", true)
}

func TestPreservesTransitionTime(t *testing.T) {
	r, c := setup(t, newServer())
	reconcile(t, r)
	m := assertStatus(t, c, "Pending", false)
	transition := m.Status.Conditions[0].LastTransitionTime
	transition.Time = transition.AddDate(-1, 0, 0)
	m.Status.Conditions[0].LastTransitionTime = transition
	unrelated := metav1.Condition{Type: "Other", Status: metav1.ConditionTrue, Reason: "OtherController", LastTransitionTime: transition}
	m.Status.Conditions = append(m.Status.Conditions, unrelated)
	if err := c.Status().Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	m = assertStatus(t, c, "Pending", false)
	if !m.Status.Conditions[0].LastTransitionTime.Equal(&transition) {
		t.Fatal("unchanged False condition reset LastTransitionTime")
	}
	if !equality.Semantic.DeepEqual(meta.FindStatusCondition(m.Status.Conditions, "Other"), &unrelated) {
		t.Fatal("unrelated condition was changed")
	}
	c.statusWrites = 0
	reconcile(t, r)
	if c.statusWrites != 0 {
		t.Fatal("unchanged pending status was written")
	}
}

func TestInitializesMissingManagedContainer(t *testing.T) {
	r, c := setup(t, newServer())
	reconcile(t, r)
	dep := &appsv1.Deployment{}
	get(t, c, dep, "test")
	dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: "sidecar", Image: "sidecar:v1"}}
	if err := c.Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	get(t, c, dep, "test")
	if len(dep.Spec.Template.Spec.Containers) != 2 || dep.Spec.Template.Spec.Containers[0].Image != "sidecar:v1" ||
		dep.Spec.Template.Spec.Containers[1].Name != "mcp-server" || dep.Spec.Template.Spec.Containers[1].Image != "server:v1" ||
		dep.Spec.Template.Spec.Containers[1].ReadinessProbe.TCPSocket.Port.IntVal != 8080 {
		t.Fatalf("managed container was not initialized correctly: %#v", dep.Spec.Template.Spec.Containers)
	}
}

func TestDeletingChildClearsReadiness(t *testing.T) {
	r, c := setup(t, newServer())
	reconcile(t, r)
	setDeploymentStatus(t, c, availableStatus(0))
	reconcile(t, r)
	assertStatus(t, c, "Ready", true)
	svc := &corev1.Service{}
	get(t, c, svc, "test-svc")
	svc.Finalizers = []string{"test.example/finalizer"}
	if err := c.Update(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newServer())})
	if err == nil || !strings.Contains(err.Error(), "resource is being deleted") {
		t.Fatalf("expected deleting-child error, got %v", err)
	}
	assertStatus(t, c, "Failed", false)
}

func TestRecreatesOwnedResources(t *testing.T) {
	r, c := setup(t, newServer())
	reconcile(t, r)
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
		name := "test"
		if _, ok := obj.(*corev1.Service); ok {
			name = "test-svc"
		}
		get(t, c, obj, name)
		if err := c.Delete(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		reconcile(t, r)
		get(t, c, obj, name)
		owner := metav1.GetControllerOf(obj)
		if owner == nil || owner.UID != newServer().UID {
			t.Fatalf("recreated resource lacks correct controller: %#v", obj)
		}
	}
}

func TestRefusesAdoption(t *testing.T) {
	for _, kind := range []string{"Deployment", "Service"} {
		for _, ownership := range []string{"unowned", "other UID", "non-controller", "wrong kind"} {
			t.Run(kind+"/"+ownership, func(t *testing.T) {
				m := newServer()
				m.Status = mcpv1alpha1.MCPServerStatus{Phase: "Ready", ObservedGeneration: 1, Conditions: []metav1.Condition{{
					Type: mcpv1alpha1.MCPServerReady, Status: metav1.ConditionTrue, ObservedGeneration: 1,
				}}}
				var object client.Object = &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"}}
				if kind == "Service" {
					object = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"}}
				}
				if ownership != "unowned" {
					controller := ownership != "non-controller"
					owner := metav1.OwnerReference{APIVersion: mcpv1alpha1.GroupVersion.String(), Kind: "MCPServer", Name: m.Name, UID: m.UID, Controller: &controller}
					if ownership == "other UID" {
						owner.UID = "old-server-uid"
					}
					if ownership == "wrong kind" {
						owner.Kind = "Other"
					}
					object.SetOwnerReferences([]metav1.OwnerReference{owner})
				}
				r, c := setup(t, m, object)
				get(t, c, object, object.GetName())
				before := object.DeepCopyObject()
				_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
				if err == nil || !strings.Contains(err.Error(), "not controlled by MCPServer UID") {
					t.Fatalf("expected ownership error, got %v", err)
				}
				get(t, c, object, object.GetName())
				if !equality.Semantic.DeepEqual(before, object) {
					t.Fatal("conflicting resource was mutated")
				}
				m = assertStatus(t, c, "Failed", false)
				if m.Status.Conditions[0].Reason != "ReconcileError" {
					t.Fatalf("unexpected condition: %#v", m.Status.Conditions)
				}
			})
		}
	}
}

func TestDeletingAndNotFound(t *testing.T) {
	t.Run("notfound", func(t *testing.T) {
		r, c := setup(t)
		reconcile(t, r)
		if c.writes != 0 || c.statusWrites != 0 {
			t.Fatal("notfound reconciliation wrote resources")
		}
	})
	t.Run("deleting", func(t *testing.T) {
		m := newServer()
		now := metav1.Now()
		m.DeletionTimestamp, m.Finalizers = &now, []string{"test.example/finalizer"}
		m.Status = mcpv1alpha1.MCPServerStatus{Phase: "Ready", ObservedGeneration: 1, Conditions: []metav1.Condition{{
			Type: mcpv1alpha1.MCPServerReady, Status: metav1.ConditionTrue, ObservedGeneration: 1,
		}}}
		r, c := setup(t, m)
		reconcile(t, r)
		assertStatus(t, c, "Pending", false)
		if c.writes != 0 {
			t.Fatal("deleting reconciliation wrote children")
		}
	})
}

func TestReconcileErrorsClearReadiness(t *testing.T) {
	for _, failStatus := range []bool{false, true} {
		t.Run(map[bool]string{false: "status succeeds", true: "status fails"}[failStatus], func(t *testing.T) {
			r, c := setup(t, newServer())
			reconcile(t, r)
			setDeploymentStatus(t, c, availableStatus(0))
			reconcile(t, r)
			assertStatus(t, c, "Ready", true)
			dep := &appsv1.Deployment{}
			get(t, c, dep, "test")
			if err := c.Delete(context.Background(), dep); err != nil {
				t.Fatal(err)
			}
			createErr, statusErr := errors.New("create unavailable"), errors.New("status unavailable")
			c.createError = createErr
			if failStatus {
				c.statusError = statusErr
			}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newServer())})
			if !errors.Is(err, createErr) || (failStatus && !errors.Is(err, statusErr)) {
				t.Fatalf("reconcile did not preserve errors: %v", err)
			}
			if !failStatus {
				m := assertStatus(t, c, "Failed", false)
				if m.Status.Conditions[0].Reason != "ReconcileError" || !strings.Contains(m.Status.Conditions[0].Message, createErr.Error()) {
					t.Fatalf("unexpected error condition: %#v", m.Status.Conditions)
				}
			}
		})
	}
}
