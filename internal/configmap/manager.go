// Package configmap manages the cloudflared ConfigMap ingress rules.
// It reads, mutates, and writes the config.yaml key of the cloudflared
// ConfigMap, maintaining the invariant that the catch-all rule is always last.
package configmap

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	configKey    = "config.yaml"
	catchAllRule = "http_status:404"
)

// cloudflaredConfig mirrors the cloudflared config.yaml structure.
type cloudflaredConfig struct {
	Tunnel          string        `yaml:"tunnel"`
	CredentialsFile string        `yaml:"credentials-file"`
	Ingress         []ingressRule `yaml:"ingress"`
}

// ingressRule maps a hostname to a backend service URL.
// hostname is omitted for the catch-all rule.
type ingressRule struct {
	Hostname string `yaml:"hostname,omitempty"`
	Service  string `yaml:"service"`
}

// Manager performs read-modify-write operations on the cloudflared ConfigMap.
type Manager struct {
	client client.Client
}

// New returns a Manager backed by the given Kubernetes client.
func New(c client.Client) *Manager {
	return &Manager{client: c}
}

// UpsertIngress adds or updates the ingress rule for hostname in the cloudflared
// ConfigMap, routing traffic to backendURL. The catch-all rule is always preserved
// as the final entry.
func (m *Manager) UpsertIngress(ctx context.Context, name, namespace, hostname, backendURL string) error {
	log.FromContext(ctx).V(1).Info("upserting ingress rule", "configmap", namespace+"/"+name, "hostname", hostname, "backend", backendURL)
	return m.retryOnConflict(ctx, name, namespace, func(cfg *cloudflaredConfig) {
		cfg.Ingress = removeByHostname(cfg.Ingress, hostname)
		cfg.Ingress = insertBeforeCatchAll(cfg.Ingress, ingressRule{
			Hostname: hostname,
			Service:  backendURL,
		})
	})
}

// RemoveIngress deletes the ingress rule for hostname from the cloudflared ConfigMap.
func (m *Manager) RemoveIngress(ctx context.Context, name, namespace, hostname string) error {
	log.FromContext(ctx).V(1).Info("removing ingress rule", "configmap", namespace+"/"+name, "hostname", hostname)
	return m.retryOnConflict(ctx, name, namespace, func(cfg *cloudflaredConfig) {
		cfg.Ingress = removeByHostname(cfg.Ingress, hostname)
	})
}

// retryOnConflict fetches the ConfigMap, runs mutateFn on the parsed config,
// marshals it back, and updates the ConfigMap. Retries up to 3 times on
// optimistic-lock conflicts (HTTP 409).
func (m *Manager) retryOnConflict(ctx context.Context, name, namespace string, mutateFn func(*cloudflaredConfig)) error {
	key := types.NamespacedName{Name: name, Namespace: namespace}

	logger := log.FromContext(ctx).WithValues("configmap", namespace+"/"+name)

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			logger.V(1).Info("retrying after conflict", "attempt", attempt+1)
		}

		cm := &corev1.ConfigMap{}
		if err := m.client.Get(ctx, key, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("cloudflared configmap %s/%s not found", namespace, name)
			}
			return fmt.Errorf("getting cloudflared configmap: %w", err)
		}
		logger.V(1).Info("configmap fetched", "resourceVersion", cm.ResourceVersion)

		cfg, err := parseConfig(cm.Data[configKey])
		if err != nil {
			return err
		}
		logger.V(1).Info("parsed ingress rules", "count", len(cfg.Ingress))

		mutateFn(cfg)
		logger.V(1).Info("ingress rules after mutation", "count", len(cfg.Ingress))

		raw, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshaling cloudflared config: %w", err)
		}

		patch := client.MergeFrom(cm.DeepCopy())
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[configKey] = string(raw)

		if err := m.client.Patch(ctx, cm, patch); err != nil {
			if apierrors.IsConflict(err) {
				logger.V(1).Info("conflict on patch, will retry")
				continue
			}
			return fmt.Errorf("patching cloudflared configmap: %w", err)
		}
		logger.V(1).Info("configmap patched successfully")
		return nil
	}
	return fmt.Errorf("failed to update cloudflared configmap after 3 attempts due to conflicts")
}

// parseConfig unmarshals config.yaml content into a cloudflaredConfig.
// It ensures the catch-all rule exists as the final entry.
func parseConfig(raw string) (*cloudflaredConfig, error) {
	cfg := &cloudflaredConfig{}
	if raw != "" {
		if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
			return nil, fmt.Errorf("parsing cloudflared config.yaml: %w", err)
		}
	}
	// Ensure exactly one catch-all at the end.
	cfg.Ingress = removeByHostname(cfg.Ingress, "") // remove any stale bare catch-alls
	hasCatchAll := false
	for _, r := range cfg.Ingress {
		if r.Hostname == "" && r.Service == catchAllRule {
			hasCatchAll = true
			break
		}
	}
	if !hasCatchAll {
		cfg.Ingress = append(cfg.Ingress, ingressRule{Service: catchAllRule})
	}
	return cfg, nil
}

// removeByHostname removes all rules matching hostname from rules.
func removeByHostname(rules []ingressRule, hostname string) []ingressRule {
	out := make([]ingressRule, 0, len(rules))
	for _, r := range rules {
		if r.Hostname != hostname {
			out = append(out, r)
		}
	}
	return out
}

// insertBeforeCatchAll inserts rule immediately before the final catch-all entry.
func insertBeforeCatchAll(rules []ingressRule, rule ingressRule) []ingressRule {
	if len(rules) > 0 {
		last := rules[len(rules)-1]
		if last.Hostname == "" && last.Service == catchAllRule {
			result := make([]ingressRule, len(rules)+1)
			copy(result, rules[:len(rules)-1])
			result[len(rules)-1] = rule
			result[len(rules)] = last
			return result
		}
	}
	return append(rules, rule)
}
