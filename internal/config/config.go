// Package config loads and validates controller configuration from a YAML file.
// CLOUDFLARE_API_TOKEN is intentionally excluded — it must be supplied via
// environment variable (mounted from a Kubernetes Secret).
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds all controller configuration.
type Config struct {
	// Cloudflare API configuration.
	Cloudflare CloudflareConfig `yaml:"cloudflare"`

	// Cloudflared deployment configuration.
	Cloudflared CloudflaredConfig `yaml:"cloudflared"`

	// Controller manager settings. All have defaults; override as needed.
	MetricsBindAddress     string `yaml:"metricsBindAddress"`
	HealthProbeBindAddress string `yaml:"healthProbeBindAddress"`
	LeaderElect            bool   `yaml:"leaderElect"`

	// LogLevel sets the zap log verbosity. Accepted values: debug, info, warn, error.
	// Defaults to "info".
	LogLevel string `yaml:"logLevel"`
}

// CloudflareConfig holds Cloudflare account and tunnel identifiers.
type CloudflareConfig struct {
	// AccountID is the Cloudflare account ID.
	AccountID string `yaml:"accountID"`

	// ZoneID is the Cloudflare DNS zone ID.
	ZoneID string `yaml:"zoneID"`

	// TunnelID is the Argo Tunnel UUID.
	TunnelID string `yaml:"tunnelID"`
}

// CloudflaredConfig controls both the cloudflared infra (Deployment + ConfigMap)
// and the ingress rules within that ConfigMap.
type CloudflaredConfig struct {
	// Image is the cloudflared container image. Defaults to "cloudflare/cloudflared:latest".
	Image string `yaml:"image"`

	// Replicas is the desired number of cloudflared pods. Defaults to 2.
	Replicas int32 `yaml:"replicas"`

	// DeploymentName is the name of the cloudflared Deployment.
	// Defaults to "cloudflared".
	DeploymentName string `yaml:"deploymentName"`

	// CredentialsSecret is the name of the Kubernetes Secret (in the same
	// namespace as the ConfigMap) containing the tunnel credentials JSON
	// under the key "credentials.json". When set, the controller creates and
	// reconciles the cloudflared ConfigMap and Deployment. When empty, the
	// controller only manages ingress rules in a pre-existing ConfigMap.
	CredentialsSecret string `yaml:"credentialsSecret"`

	ConfigMap ConfigMapRef `yaml:"configMap"`
}

// ConfigMapRef identifies a Kubernetes ConfigMap by name and namespace.
type ConfigMapRef struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// defaults returns a Config with sensible default values.
func defaults() Config {
	return Config{
		MetricsBindAddress:     ":8080",
		HealthProbeBindAddress: ":8081",
		LogLevel:               "info",
		Cloudflared: CloudflaredConfig{
			Image:          "cloudflare/cloudflared:latest",
			Replicas:       2,
			DeploymentName: "cloudflared",
		},
	}
}

// Load reads and unmarshals the YAML file at path into a Config.
// Missing optional fields fall back to defaults.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	return &cfg, nil
}

// Validate returns an error if any required field is missing.
func (c *Config) Validate() error {
	var missing []string
	if c.Cloudflare.AccountID == "" {
		missing = append(missing, "cloudflare.accountID")
	}
	if c.Cloudflare.ZoneID == "" {
		missing = append(missing, "cloudflare.zoneID")
	}
	if c.Cloudflare.TunnelID == "" {
		missing = append(missing, "cloudflare.tunnelID")
	}
	if c.Cloudflared.ConfigMap.Name == "" {
		missing = append(missing, "cloudflared.configMap.name")
	}
	if c.Cloudflared.ConfigMap.Namespace == "" {
		missing = append(missing, "cloudflared.configMap.namespace")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config fields: %v", missing)
	}
	return nil
}
