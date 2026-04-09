package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/bikramdhoju/rector/api/v1alpha1"
)

const (
	conditionAvailable   = "Available"
	conditionProgressing = "Progressing"
	requeueAfter         = 30 * time.Second
)

// ApplicationReconciler reconciles Application objects.
//
// +kubebuilder:rbac:groups=apps.rector.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.rector.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.rector.io,resources=applications/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
type ApplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	app := &appsv1alpha1.Application{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Capture patch base before mutations so the deferred status patch is a clean diff.
	patch := client.MergeFrom(app.DeepCopy())
	defer func() {
		if err := r.Status().Patch(ctx, app, patch); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to patch Application status")
		}
	}()

	deploy, err := r.reconcileDeployment(ctx, app)
	if err != nil {
		r.Recorder.Eventf(app, corev1.EventTypeWarning, "DeploymentFailed", "failed to reconcile Deployment: %v", err)
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, app); err != nil {
		r.Recorder.Eventf(app, corev1.EventTypeWarning, "ServiceFailed", "failed to reconcile Service: %v", err)
		return ctrl.Result{}, err
	}

	app.Status.AvailableReplicas = deploy.Status.AvailableReplicas
	r.updateConditions(app)

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *ApplicationReconciler) reconcileDeployment(ctx context.Context, app *appsv1alpha1.Application) (*appsv1.Deployment, error) {
	logger := log.FromContext(ctx)
	labels := labelsForApp(app.Name)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec = appsv1.DeploymentSpec{
			Replicas: app.Spec.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Tolerations: app.Spec.Tolerations,
					Containers: []corev1.Container{
						{
							Name:  app.Name,
							Image: app.Spec.Image,
							Ports: []corev1.ContainerPort{
								{ContainerPort: app.Spec.Port, Protocol: corev1.ProtocolTCP},
							},
							Env:       app.Spec.Env,
							Resources: app.Spec.Resources,
						},
					},
				},
			},
		}
		return ctrl.SetControllerReference(app, deploy, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("reconciling Deployment: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("Deployment reconciled", "operation", op)
		r.Recorder.Eventf(app, corev1.EventTypeNormal, "DeploymentReconciled", "Deployment %s %s", app.Name, op)
	}
	return deploy, nil
}

func (r *ApplicationReconciler) reconcileService(ctx context.Context, app *appsv1alpha1.Application) error {
	logger := log.FromContext(ctx)
	labels := labelsForApp(app.Name)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Annotations = app.Spec.ServiceAnnotations
		// Preserve ClusterIP — it is immutable after Service creation.
		existingClusterIP := svc.Spec.ClusterIP
		svc.Spec = corev1.ServiceSpec{
			Type:      app.Spec.ServiceType,
			Selector:  labels,
			ClusterIP: existingClusterIP,
			Ports: []corev1.ServicePort{
				{
					Port:       app.Spec.Port,
					TargetPort: intstr.FromInt32(app.Spec.Port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		}
		return ctrl.SetControllerReference(app, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconciling Service: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("Service reconciled", "operation", op)
		r.Recorder.Eventf(app, corev1.EventTypeNormal, "ServiceReconciled", "Service %s %s", app.Name, op)
	}
	return nil
}

func (r *ApplicationReconciler) updateConditions(app *appsv1alpha1.Application) {
	desired := int32(1)
	if app.Spec.Replicas != nil {
		desired = *app.Spec.Replicas
	}

	if app.Status.AvailableReplicas >= desired {
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               conditionAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             "MinimumReplicasAvailable",
			Message:            fmt.Sprintf("%d/%d replicas available", app.Status.AvailableReplicas, desired),
			ObservedGeneration: app.Generation,
		})
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               conditionProgressing,
			Status:             metav1.ConditionFalse,
			Reason:             "NewReplicaSetAvailable",
			Message:            "Deployment has successfully rolled out",
			ObservedGeneration: app.Generation,
		})
	} else {
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               conditionAvailable,
			Status:             metav1.ConditionFalse,
			Reason:             "MinimumReplicasUnavailable",
			Message:            fmt.Sprintf("%d/%d replicas available", app.Status.AvailableReplicas, desired),
			ObservedGeneration: app.Generation,
		})
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               conditionProgressing,
			Status:             metav1.ConditionTrue,
			Reason:             "ReplicaSetUpdated",
			Message:            "Deployment is progressing",
			ObservedGeneration: app.Generation,
		})
	}
}

// SetupWithManager registers the controller and sets up watches on owned resources.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Application{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func labelsForApp(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       name,
		"app.kubernetes.io/managed-by": "rector-controller",
	}
}
