package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	cfc "github.com/bikramdhoju/cloudflare-controller/internal/cloudflare"
	cfd "github.com/bikramdhoju/cloudflare-controller/internal/cloudflared"
	"github.com/bikramdhoju/cloudflare-controller/internal/configmap"
)

const (
	AnnotationHostname      = "cloudflare-controller.io/hostname"
	AnnotationPort          = "cloudflare-controller.io/port"
	AnnotationAccessEnabled = "cloudflare-controller.io/access-enabled"
	AnnotationAccessPolicies = "cloudflare-controller.io/access-policies"
	AnnotationDNSRecordID   = "cloudflare-controller.io/dns-record-id"
	AnnotationAccessAppID   = "cloudflare-controller.io/access-app-id"
	Finalizer               = "cloudflare-controller.io/finalizer"

	requeueAfter = 5 * time.Minute
)

// ServiceReconciler watches Services for cloudflare-controller.io annotations and
// manages the corresponding Cloudflare DNS records, cloudflared ConfigMap ingress
// rules, and (optionally) Cloudflare Access Applications.
//
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
type ServiceReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	ConfigMgr            *configmap.Manager
	// CloudflaredMgr manages the cloudflared Deployment and ConfigMap lifecycle.
	// Nil when credentialsSecret is not configured (ingress-only mode).
	CloudflaredMgr       *cfd.Manager
	ConfigMapName        string
	ConfigMapNamespace   string
	// Cloudflare config — sourced from flags and CLOUDFLARE_API_TOKEN env var at startup.
	CFClient    cfc.Client
	AccountID   string
	ZoneID      string
	TunnelID    string
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.V(1).Info("reconcile started")

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("service not found, skipping")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	hostname := svc.Annotations[AnnotationHostname]
	logger.V(1).Info("service fetched",
		"hostname", hostname,
		"deletionTimestamp", svc.DeletionTimestamp,
		"finalizers", svc.Finalizers,
	)

	// Deletion or annotation-removal path: clean up Cloudflare resources.
	if !svc.DeletionTimestamp.IsZero() || hostname == "" {
		if controllerutil.ContainsFinalizer(svc, Finalizer) {
			logger.V(1).Info("running cleanup", "reason", map[bool]string{true: "deletion", false: "annotation removed"}[!svc.DeletionTimestamp.IsZero()])
			if err := r.cleanup(ctx, svc); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(svc, Finalizer)
			if err := r.Update(ctx, svc); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("finalizer removed", "service", req.NamespacedName)
		} else {
			logger.V(1).Info("no finalizer present, nothing to clean up")
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before any external mutations.
	if !controllerutil.ContainsFinalizer(svc, Finalizer) {
		logger.V(1).Info("adding finalizer")
		controllerutil.AddFinalizer(svc, Finalizer)
		if err := r.Update(ctx, svc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil // re-enqueued by the Update watch event
	}

	backendURL, err := r.backendURL(svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("resolved backend URL", "backendURL", backendURL)

	// Use MergeFrom to patch only changed annotations back to the Service.
	patch := client.MergeFrom(svc.DeepCopy())

	// 0. Ensure cloudflared ConfigMap and Deployment exist.
	if r.CloudflaredMgr != nil {
		logger.V(1).Info("ensuring cloudflared infra")
		if err := r.CloudflaredMgr.EnsureInfra(ctx); err != nil {
			r.Recorder.Eventf(svc, corev1.EventTypeWarning, "CloudflaredInfraFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	// 1. Ensure DNS CNAME record.
	logger.V(1).Info("reconciling DNS record", "hostname", hostname)
	if err := r.reconcileDNS(ctx, svc, hostname); err != nil {
		r.Recorder.Eventf(svc, corev1.EventTypeWarning, "DNSFailed", err.Error())
		return ctrl.Result{}, err
	}

	// 2. Update cloudflared ConfigMap ingress rule.
	logger.V(1).Info("upserting configmap ingress rule", "hostname", hostname, "backend", backendURL)
	cmChanged, err := r.ConfigMgr.UpsertIngress(ctx, r.ConfigMapName, r.ConfigMapNamespace, hostname, backendURL)
	if err != nil {
		r.Recorder.Eventf(svc, corev1.EventTypeWarning, "ConfigMapFailed", err.Error())
		return ctrl.Result{}, err
	}
	if cmChanged && r.CloudflaredMgr != nil {
		logger.Info("configmap updated, restarting cloudflared deployment")
		if err := r.CloudflaredMgr.RestartDeployment(ctx); err != nil {
			r.Recorder.Eventf(svc, corev1.EventTypeWarning, "CloudflaredRestartFailed", err.Error())
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(svc, corev1.EventTypeNormal, "CloudflaredRestarted", "cloudflared deployment restarted after config update")
	}

	// 3. Optionally create Cloudflare Access Application and sync policies.
	accessEnabled := svc.Annotations[AnnotationAccessEnabled] == "true"
	logger.V(1).Info("access application", "enabled", accessEnabled)
	if accessEnabled {
		policies := cfc.ParsePolicies(svc.Annotations[AnnotationAccessPolicies])
		if err := r.reconcileAccessApp(ctx, svc, hostname, policies); err != nil {
			r.Recorder.Eventf(svc, corev1.EventTypeWarning, "AccessAppFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	// Persist any new annotation values (DNS record ID, Access App ID).
	if err := r.Patch(ctx, svc, patch); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("patching service annotations: %w", err)
	}

	logger.V(1).Info("reconcile complete", "requeueAfter", requeueAfter)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileDNS ensures a CNAME DNS record exists for hostname and stores
// the record ID in the Service annotation.
func (r *ServiceReconciler) reconcileDNS(ctx context.Context, svc *corev1.Service, hostname string) error {
	logger := log.FromContext(ctx)

	id, err := r.CFClient.EnsureDNSRecord(ctx, r.ZoneID, hostname, r.TunnelID)
	if err != nil {
		return err
	}
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	if svc.Annotations[AnnotationDNSRecordID] != id {
		svc.Annotations[AnnotationDNSRecordID] = id
		logger.Info("DNS record ensured", "hostname", hostname, "recordID", id)
		r.Recorder.Eventf(svc, corev1.EventTypeNormal, "DNSRecordCreated", "DNS record ensured for %s (id=%s)", hostname, id)
	}
	return nil
}

// reconcileAccessApp ensures a Cloudflare Access Application exists for hostname,
// stores the app ID in the Service annotation, and syncs the desired policies.
func (r *ServiceReconciler) reconcileAccessApp(ctx context.Context, svc *corev1.Service, hostname string, policies []cfc.PolicySpec) error {
	logger := log.FromContext(ctx)

	// Ensure the Access Application exists and record its ID.
	if svc.Annotations[AnnotationAccessAppID] == "" {
		id, err := r.CFClient.EnsureAccessApp(ctx, r.AccountID, hostname)
		if err != nil {
			return err
		}
		svc.Annotations[AnnotationAccessAppID] = id
		logger.Info("Access Application ensured", "hostname", hostname, "appID", id)
		r.Recorder.Eventf(svc, corev1.EventTypeNormal, "AccessAppCreated", "Cloudflare Access Application created for %s (id=%s)", hostname, id)
	}

	// Always sync policies so annotation changes take effect on every reconcile.
	appID := svc.Annotations[AnnotationAccessAppID]
	logger.V(1).Info("syncing access policies", "appID", appID, "count", len(policies))
	return r.CFClient.SyncAccessPolicies(ctx, r.AccountID, appID, policies)
}

// cleanup removes all Cloudflare resources that were created for this Service.
func (r *ServiceReconciler) cleanup(ctx context.Context, svc *corev1.Service) error {
	logger := log.FromContext(ctx)
	hostname := svc.Annotations[AnnotationHostname]

	// Delete DNS record.
	if id := svc.Annotations[AnnotationDNSRecordID]; id != "" {
		if err := r.CFClient.DeleteDNSRecord(ctx, r.ZoneID, id); err != nil {
			return fmt.Errorf("deleting DNS record during cleanup: %w", err)
		}
		logger.Info("DNS record deleted", "hostname", hostname, "recordID", id)
	}

	// Remove ingress rule from ConfigMap.
	if hostname != "" {
		cmChanged, err := r.ConfigMgr.RemoveIngress(ctx, r.ConfigMapName, r.ConfigMapNamespace, hostname)
		if err != nil {
			return fmt.Errorf("removing configmap ingress during cleanup: %w", err)
		}
		logger.Info("ingress rule removed from cloudflared configmap", "hostname", hostname)
		if cmChanged && r.CloudflaredMgr != nil {
			logger.Info("configmap updated, restarting cloudflared deployment")
			if err := r.CloudflaredMgr.RestartDeployment(ctx); err != nil {
				return fmt.Errorf("restarting cloudflared deployment after configmap update: %w", err)
			}
		}
	}

	// Delete Access Application.
	if id := svc.Annotations[AnnotationAccessAppID]; id != "" {
		if err := r.CFClient.DeleteAccessApp(ctx, r.AccountID, id); err != nil {
			return fmt.Errorf("deleting access application during cleanup: %w", err)
		}
		logger.Info("Access Application deleted", "hostname", hostname, "appID", id)
	}

	return nil
}

// backendURL constructs the in-cluster backend URL for the Service.
// Uses the first port unless overridden by the AnnotationPort annotation.
func (r *ServiceReconciler) backendURL(svc *corev1.Service) (string, error) {
	if len(svc.Spec.Ports) == 0 {
		return "", fmt.Errorf("service %s/%s has no ports defined", svc.Namespace, svc.Name)
	}
	port := int64(svc.Spec.Ports[0].Port)
	if override, ok := svc.Annotations[AnnotationPort]; ok {
		p, err := strconv.ParseInt(override, 10, 32)
		if err != nil {
			return "", fmt.Errorf("invalid %s annotation value %q: %w", AnnotationPort, override, err)
		}
		port = p
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svc.Name, svc.Namespace, port), nil
}

// SetupWithManager registers the controller with a Service watch filtered to
// only Services that carry the hostname annotation or the cleanup finalizer.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	hasAnnotationOrFinalizer := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		ann := obj.GetAnnotations()
		if _, ok := ann[AnnotationHostname]; ok {
			return true
		}
		for _, f := range obj.GetFinalizers() {
			if f == Finalizer {
				return true
			}
		}
		return false
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(
			predicate.Or(hasAnnotationOrFinalizer, predicate.AnnotationChangedPredicate{}),
		)).
		Complete(r)
}
