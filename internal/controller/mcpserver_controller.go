package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
)

// MCPServerReconciler reconciles an MCPServer object into a running
// Deployment + Service. This is the "boring, provable core" described in
// PLAN.md — it deliberately does nothing clever yet.
type MCPServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mcp.portkeeper.dev,resources=mcpservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcp.portkeeper.dev,resources=mcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *MCPServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mcpServer mcpv1alpha1.MCPServer
	if err := r.Get(ctx, req.NamespacedName, &mcpServer); err != nil {
		if apierrors.IsNotFound(err) {
			// Object deleted — owned Deployment/Service are garbage
			// collected via owner references, nothing else to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !mcpServer.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.updateStatus(ctx, &mcpServer, "Pending", metav1.ConditionFalse, "Deleting", "MCPServer is being deleted")
	}

	dep, changed, err := r.reconcileDeployment(ctx, &mcpServer)
	if err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		return ctrl.Result{}, errors.Join(err, r.updateStatus(ctx, &mcpServer, "Failed", metav1.ConditionFalse, "ReconcileError", err.Error()))
	}

	if err := r.reconcileService(ctx, &mcpServer); err != nil {
		logger.Error(err, "failed to reconcile Service")
		return ctrl.Result{}, errors.Join(err, r.updateStatus(ctx, &mcpServer, "Failed", metav1.ConditionFalse, "ReconcileError", err.Error()))
	}

	phase, status, reason, message := deploymentReadiness(dep, changed)
	return ctrl.Result{}, r.updateStatus(ctx, &mcpServer, phase, status, reason, message)
}

func (r *MCPServerReconciler) updateStatus(ctx context.Context, m *mcpv1alpha1.MCPServer, phase string, status metav1.ConditionStatus, reason, message string) error {
	before := m.DeepCopy()
	m.Status.Phase = phase
	m.Status.ObservedGeneration = m.Generation
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: mcpv1alpha1.MCPServerReady, Status: status, ObservedGeneration: m.Generation,
		Reason: reason, Message: message,
	})
	if equality.Semantic.DeepEqual(before.Status, m.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, m, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	log.FromContext(ctx).Info("mcpserver_status", "namespace", m.Namespace, "server", m.Name,
		"generation", m.Generation, "phase", phase, "ready", status, "reason", reason)
	return nil
}

func deploymentReadiness(dep *appsv1.Deployment, changed bool) (string, metav1.ConditionStatus, string, string) {
	if changed || dep.Status.ObservedGeneration != dep.Generation {
		return "Pending", metav1.ConditionFalse, "Progressing", "Waiting for the Deployment controller to observe the desired workload"
	}
	for _, condition := range dep.Status.Conditions {
		if (condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse && condition.Reason == "ProgressDeadlineExceeded") ||
			(condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue) {
			reason := condition.Reason
			if reason == "" {
				reason = string(condition.Type)
			}
			return "Failed", metav1.ConditionFalse, reason, condition.Message
		}
	}
	desired := *dep.Spec.Replicas
	if desired > 0 && dep.Status.Replicas == desired && dep.Status.UpdatedReplicas == desired &&
		dep.Status.ReadyReplicas >= desired && dep.Status.AvailableReplicas >= desired {
		return "Ready", metav1.ConditionTrue, "DeploymentAvailable", "The desired Deployment replicas are updated, ready, and available"
	}
	return "Pending", metav1.ConditionFalse, "DeploymentUnavailable", "Waiting for the desired Deployment replicas to be updated, ready, and available"
}

func (r *MCPServerReconciler) reconcileDeployment(ctx context.Context, m *mcpv1alpha1.MCPServer) (*appsv1.Deployment, bool, error) {
	labels := map[string]string{"mcp.portkeeper.dev/server": m.Name}
	replicas := int32(1)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace},
	}

	changed := false
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		if err := checkOwnership(m, dep); err != nil {
			return err
		}
		before := dep.Spec.DeepCopy()
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		if dep.Spec.Template.Labels == nil {
			dep.Spec.Template.Labels = make(map[string]string)
		}
		dep.Spec.Template.Labels["mcp.portkeeper.dev/server"] = m.Name
		containers := &dep.Spec.Template.Spec.Containers
		index := -1
		for i := range *containers {
			if (*containers)[i].Name == "mcp-server" {
				index = i
				break
			}
		}
		if index == -1 {
			*containers = append(*containers, corev1.Container{Name: "mcp-server"})
			index = len(*containers) - 1
		}
		container := &(*containers)[index]
		container.Image = m.Spec.Image
		container.Ports = []corev1.ContainerPort{{ContainerPort: m.Spec.Port, Protocol: corev1.ProtocolTCP}}
		if container.ReadinessProbe == nil {
			container.ReadinessProbe = &corev1.Probe{}
		}
		container.ReadinessProbe.ProbeHandler = corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(m.Spec.Port)},
		}
		changed = !equality.Semantic.DeepEqual(before, &dep.Spec)
		return controllerutil.SetControllerReference(m, dep, r.Scheme)
	})
	return dep, changed, err
}

func (r *MCPServerReconciler) reconcileService(ctx context.Context, m *mcpv1alpha1.MCPServer) error {
	labels := map[string]string{"mcp.portkeeper.dev/server": m.Name}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName(m.Name), Namespace: m.Namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := checkOwnership(m, svc); err != nil {
			return err
		}
		svc.Spec.Selector = labels
		port := corev1.ServicePort{}
		if len(svc.Spec.Ports) != 0 {
			port = svc.Spec.Ports[0]
		}
		port.Port = m.Spec.Port
		port.TargetPort = intstr.FromInt32(m.Spec.Port)
		port.Protocol = corev1.ProtocolTCP
		svc.Spec.Ports = []corev1.ServicePort{port}
		return controllerutil.SetControllerReference(m, svc, r.Scheme)
	})
	return err
}

func checkOwnership(m *mcpv1alpha1.MCPServer, object client.Object) error {
	if object.GetResourceVersion() == "" {
		return nil
	}
	owner := metav1.GetControllerOf(object)
	if owner == nil || owner.UID != m.UID || owner.Name != m.Name ||
		owner.Kind != "MCPServer" || owner.APIVersion != mcpv1alpha1.GroupVersion.String() {
		return fmt.Errorf("refusing to mutate %T %s/%s: not controlled by MCPServer UID %s", object, object.GetNamespace(), object.GetName(), m.UID)
	}
	if !object.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("cannot reconcile %T %s/%s: resource is being deleted", object, object.GetNamespace(), object.GetName())
	}
	return nil
}

func serviceName(mcpServerName string) string {
	return fmt.Sprintf("%s-svc", mcpServerName)
}

// SetupWithManager wires this reconciler into the controller manager,
// watching MCPServer objects and the Deployments/Services it owns.
func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcpv1alpha1.MCPServer{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
