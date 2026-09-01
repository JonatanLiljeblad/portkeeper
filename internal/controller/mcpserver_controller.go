package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	if err := r.reconcileDeployment(ctx, &mcpServer); err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, &mcpServer); err != nil {
		logger.Error(err, "failed to reconcile Service")
		return ctrl.Result{}, err
	}

	mcpServer.Status.Phase = "Ready"
	mcpServer.Status.ObservedGeneration = mcpServer.Generation
	if err := r.Status().Update(ctx, &mcpServer); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *MCPServerReconciler) reconcileDeployment(ctx context.Context, m *mcpv1alpha1.MCPServer) error {
	labels := map[string]string{"mcp.portkeeper.dev/server": m.Name}
	replicas := int32(1)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "mcp-server",
							Image: m.Spec.Image,
							Ports: []corev1.ContainerPort{{ContainerPort: m.Spec.Port}},
						},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(m, dep, r.Scheme)
	})
	return err
}

func (r *MCPServerReconciler) reconcileService(ctx context.Context, m *mcpv1alpha1.MCPServer) error {
	labels := map[string]string{"mcp.portkeeper.dev/server": m.Name}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName(m.Name), Namespace: m.Namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{
			{Port: m.Spec.Port, TargetPort: intstr.FromInt32(m.Spec.Port)},
		}
		return controllerutil.SetControllerReference(m, svc, r.Scheme)
	})
	return err
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
