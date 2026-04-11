// Package cloudflared manages the cloudflared Deployment and ConfigMap
// lifecycle, ensuring they exist with the correct base configuration.
// Ingress rule mutations are handled separately by the configmap package.
package cloudflared

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	configMountPath = "/etc/cloudflared/config"
	credsMountPath  = "/etc/cloudflared/creds"
	credsKey        = "credentials.json"

	labelKey   = "app.kubernetes.io/name"
	labelValue = "cloudflared"
)

// Manager creates and reconciles the cloudflared Secret, ConfigMap, and Deployment.
type Manager struct {
	client            client.Client
	namespace         string
	configMapName     string
	deploymentName    string
	image             string
	replicas          int32
	tunnelID          string
	credentialsSecret string
	credentialsJSON   string
}

// New returns a Manager that will reconcile cloudflared infra in namespace.
func New(c client.Client, namespace, configMapName, deploymentName, image string, replicas int32, tunnelID, credentialsSecret, credentialsJSON string) *Manager {
	return &Manager{
		client:            c,
		namespace:         namespace,
		configMapName:     configMapName,
		deploymentName:    deploymentName,
		image:             image,
		replicas:          replicas,
		tunnelID:          tunnelID,
		credentialsSecret: credentialsSecret,
		credentialsJSON:   credentialsJSON,
	}
}

// EnsureInfra idempotently reconciles the cloudflared Secret, ConfigMap, and
// Deployment. The Secret is created first so the Deployment volume mount is
// satisfiable on first creation.
func (m *Manager) EnsureInfra(ctx context.Context) error {
	if err := m.ensureCredentialsSecret(ctx); err != nil {
		return err
	}
	if err := m.ensureConfigMap(ctx); err != nil {
		return err
	}
	return m.ensureDeployment(ctx)
}

// ensureCredentialsSecret creates or updates the cloudflared credentials Secret
// with the tunnel credentials JSON under the key "credentials.json".
func (m *Manager) ensureCredentialsSecret(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("secret", m.namespace+"/"+m.credentialsSecret)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.credentialsSecret,
			Namespace: m.namespace,
		},
	}
	result, err := controllerutil.CreateOrUpdate(ctx, m.client, secret, func() error {
		secret.Labels = map[string]string{labelKey: labelValue}
		// StringData lets Kubernetes handle base64 encoding; map is overwritten
		// on each reconcile so the Secret stays in sync with the config value.
		secret.StringData = map[string]string{credsKey: m.credentialsJSON}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reconciling cloudflared credentials secret: %w", err)
	}
	if result != controllerutil.OperationResultNone {
		logger.Info("cloudflared credentials secret reconciled", "result", result)
	} else {
		logger.V(1).Info("cloudflared credentials secret unchanged")
	}
	return nil
}

// ensureConfigMap creates the cloudflared ConfigMap with a minimal base
// configuration if it does not already exist. An existing ConfigMap is left
// untouched so that ingress rules added by the configmap.Manager are preserved.
func (m *Manager) ensureConfigMap(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("configmap", m.namespace+"/"+m.configMapName)

	cm := &corev1.ConfigMap{}
	err := m.client.Get(ctx, types.NamespacedName{Name: m.configMapName, Namespace: m.namespace}, cm)
	if err == nil {
		logger.V(1).Info("configmap already exists, skipping create")
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting cloudflared configmap: %w", err)
	}

	initial := fmt.Sprintf("tunnel: %s\ncredentials-file: %s/%s\ningress:\n- service: http_status:404\n",
		m.tunnelID, credsMountPath, credsKey)

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.configMapName,
			Namespace: m.namespace,
			Labels:    map[string]string{labelKey: labelValue},
		},
		Data: map[string]string{"config.yaml": initial},
	}
	if err := m.client.Create(ctx, cm); err != nil {
		return fmt.Errorf("creating cloudflared configmap: %w", err)
	}
	logger.Info("cloudflared configmap created")
	return nil
}

// ensureDeployment creates or updates the cloudflared Deployment. On each call
// it reconciles the replica count and container image; other fields set by
// external actors (e.g. HPA annotations) are left intact.
func (m *Manager) ensureDeployment(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("deployment", m.namespace+"/"+m.deploymentName)
	labels := map[string]string{labelKey: labelValue}
	replicas := m.replicas

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.deploymentName,
			Namespace: m.namespace,
		},
	}
	result, err := controllerutil.CreateOrUpdate(ctx, m.client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = &replicas
		if deploy.CreationTimestamp.IsZero() {
			// First creation: set immutable selector and full pod template spec.
			deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
			deploy.Spec.Template = corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       m.podSpec(),
			}
			return nil
		}
		// Existing deployment: only update the fields we own to avoid wiping
		// Kubernetes-defaulted fields (terminationMessagePath, imagePullPolicy,
		// dnsPolicy, etc.) which would cause a spurious pod template diff and
		// rolling restart on every reconcile.
		deploy.Spec.Template.Labels = labels
		if len(deploy.Spec.Template.Spec.Containers) > 0 {
			deploy.Spec.Template.Spec.Containers[0].Image = m.image
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reconciling cloudflared deployment: %w", err)
	}
	if result != controllerutil.OperationResultNone {
		logger.Info("cloudflared deployment reconciled", "result", result)
	} else {
		logger.V(1).Info("cloudflared deployment unchanged")
	}
	return nil
}

// RestartDeployment triggers a rolling restart of the cloudflared Deployment by
// updating the "kubectl.kubernetes.io/restartedAt" pod template annotation —
// the same mechanism used by `kubectl rollout restart`.
func (m *Manager) RestartDeployment(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("deployment", m.namespace+"/"+m.deploymentName)

	deploy := &appsv1.Deployment{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: m.deploymentName, Namespace: m.namespace}, deploy); err != nil {
		return fmt.Errorf("getting cloudflared deployment for restart: %w", err)
	}

	patch := client.MergeFrom(deploy.DeepCopy())
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().UTC().Format(time.RFC3339)

	if err := m.client.Patch(ctx, deploy, patch); err != nil {
		return fmt.Errorf("patching cloudflared deployment for restart: %w", err)
	}
	logger.Info("cloudflared deployment restart triggered")
	return nil
}

// podSpec returns the PodSpec for the cloudflared container.
func (m *Manager) podSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "cloudflared",
				Image: m.image,
				Args: []string{
					"tunnel",
					"--config", configMountPath + "/config.yaml",
					"--no-autoupdate",
					"run",
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "config",
						MountPath: configMountPath,
						ReadOnly:  true,
					},
					{
						Name:      "creds",
						MountPath: credsMountPath,
						ReadOnly:  true,
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: m.configMapName},
					},
				},
			},
			{
				Name: "creds",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: m.credentialsSecret,
					},
				},
			},
		},
	}
}
