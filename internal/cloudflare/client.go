// Package cloudflare wraps the cloudflare-go SDK with the operations
// needed by the cloudflare-controller: idempotent DNS record management
// and Cloudflare Access Application lifecycle.
package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PolicySpec describes a single Access Policy to attach to an Access Application.
// The Raw field is one entry from the cloudflare-controller.io/access-policies annotation,
// e.g. "service-token", "email:user@example.com", "email-domain:example.com".
type PolicySpec struct {
	Raw string
}

// ParsePolicies splits the comma-separated access-policies annotation value into
// PolicySpec entries. Blank entries are silently dropped.
func ParsePolicies(annotation string) []PolicySpec {
	annotation = strings.TrimSpace(annotation)
	if annotation == "" {
		return nil
	}
	parts := strings.Split(annotation, ",")
	out := make([]PolicySpec, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, PolicySpec{Raw: p})
		}
	}
	return out
}

// name returns the Cloudflare policy name for this spec.
// The spec raw value is used directly so the controller matches existing
// policies that share the same name (e.g. a pre-existing "service-token" policy).
func (p PolicySpec) name() string { return p.Raw }

// decision returns the Cloudflare Access decision for this policy type.
// Service-token policies use non_identity; everyone uses bypass; all others use allow.
func (p PolicySpec) decision() string {
	switch p.Raw {
	case "service-token":
		return "non_identity"
	case "everyone":
		return "bypass"
	default:
		return "allow"
	}
}

// include returns the include rules slice for the policy, or nil if the spec
// type is unrecognised.
func (p PolicySpec) include() []interface{} {
	switch {
	case p.Raw == "everyone":
		return []interface{}{
			map[string]interface{}{"everyone": map[string]interface{}{}},
		}
	case p.Raw == "service-token":
		return []interface{}{
			map[string]interface{}{"any_valid_service_token": map[string]interface{}{}},
		}
	case strings.HasPrefix(p.Raw, "email:"):
		return []interface{}{
			map[string]interface{}{"email": map[string]interface{}{"email": strings.TrimPrefix(p.Raw, "email:")}},
		}
	case strings.HasPrefix(p.Raw, "email-domain:"):
		return []interface{}{
			map[string]interface{}{"email_domain": map[string]interface{}{"domain": strings.TrimPrefix(p.Raw, "email-domain:")}},
		}
	default:
		return nil
	}
}

// Client is the interface used by the ServiceReconciler.
// Defined as an interface to allow mocking in tests.
type Client interface {
	EnsureDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) (recordID string, err error)
	DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error
	// FindDNSRecordByHostname returns the record ID of the CNAME for hostname, or "" if not found.
	// Used as a fallback during cleanup when the stored record ID annotation is missing.
	FindDNSRecordByHostname(ctx context.Context, zoneID, hostname string) (recordID string, err error)
	EnsureAccessApp(ctx context.Context, accountID, hostname string) (appID string, err error)
	DeleteAccessApp(ctx context.Context, accountID, appID string) error
	// FindAccessAppByHostname returns the app ID of the Access Application for hostname, or "" if not found.
	// Used as a fallback during cleanup when the stored app ID annotation is missing.
	FindAccessAppByHostname(ctx context.Context, accountID, hostname string) (appID string, err error)
	// SyncAccessPolicies ensures all desired policies exist on the Access Application,
	// matching by name. Existing policies (including pre-created ones) are never deleted.
	SyncAccessPolicies(ctx context.Context, accountID, appID string, specs []PolicySpec) error
}

type cfClient struct {
	api *cf.API
}

// NewClient constructs a Cloudflare client authenticated with the given API token.
func NewClient(apiToken string) (Client, error) {
	api, err := cf.NewWithAPIToken(apiToken)
	if err != nil {
		return nil, fmt.Errorf("creating cloudflare client: %w", err)
	}
	return &cfClient{api: api}, nil
}

// EnsureDNSRecord creates a proxied CNAME record hostname → tunnelID.cfargotunnel.com
// if one does not already exist. Returns the record ID.
// If a matching record already exists, returns its ID without making any changes.
func (c *cfClient) EnsureDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) (string, error) {
	logger := log.FromContext(ctx).WithValues("hostname", hostname, "zoneID", zoneID)
	target := tunnelID + ".cfargotunnel.com"

	logger.V(1).Info("listing existing CNAME records")
	existing, _, err := c.api.ListDNSRecords(ctx, cf.ZoneIdentifier(zoneID), cf.ListDNSRecordsParams{
		Type: "CNAME",
		Name: hostname,
	})
	if err != nil {
		return "", fmt.Errorf("listing DNS records for %s: %w", hostname, err)
	}
	for _, r := range existing {
		if r.Content == target {
			logger.V(1).Info("DNS record already up-to-date", "recordID", r.ID)
			return r.ID, nil
		}
		// Record exists but points elsewhere — update it.
		logger.V(1).Info("DNS record exists with wrong target, updating", "recordID", r.ID, "currentTarget", r.Content, "desiredTarget", target)
		updated, err := c.api.UpdateDNSRecord(ctx, cf.ZoneIdentifier(zoneID), cf.UpdateDNSRecordParams{
			ID:      r.ID,
			Type:    "CNAME",
			Name:    hostname,
			Content: target,
			TTL:     1,
			Proxied: cf.BoolPtr(true),
		})
		if err != nil {
			return "", fmt.Errorf("updating DNS record for %s: %w", hostname, err)
		}
		logger.V(1).Info("DNS record updated", "recordID", updated.ID)
		return updated.ID, nil
	}

	logger.V(1).Info("no existing DNS record found, creating", "target", target)
	rec, err := c.api.CreateDNSRecord(ctx, cf.ZoneIdentifier(zoneID), cf.CreateDNSRecordParams{
		Type:    "CNAME",
		Name:    hostname,
		Content: target,
		TTL:     1,
		Proxied: cf.BoolPtr(true),
		Comment: "managed by cloudflare-controller",
	})
	if err != nil {
		return "", fmt.Errorf("creating DNS record for %s: %w", hostname, err)
	}
	logger.V(1).Info("DNS record created", "recordID", rec.ID)
	return rec.ID, nil
}

// DeleteDNSRecord removes a DNS record by ID. A 404 from Cloudflare is treated
// as success (already deleted).
func (c *cfClient) DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error {
	logger := log.FromContext(ctx).WithValues("recordID", recordID, "zoneID", zoneID)
	logger.V(1).Info("deleting DNS record")
	if err := c.api.DeleteDNSRecord(ctx, cf.ZoneIdentifier(zoneID), recordID); err != nil {
		// Already gone — treat as success.
		if isNotFound(err) {
			logger.V(1).Info("DNS record already gone, skipping")
			return nil
		}
		return fmt.Errorf("deleting DNS record %s: %w", recordID, err)
	}
	logger.V(1).Info("DNS record deleted")
	return nil
}

// EnsureAccessApp creates a Cloudflare Zero Trust Access Application for hostname
// if one does not already exist. Returns the application ID.
func (c *cfClient) EnsureAccessApp(ctx context.Context, accountID, hostname string) (string, error) {
	logger := log.FromContext(ctx).WithValues("hostname", hostname, "accountID", accountID)
	logger.V(1).Info("listing access applications")
	apps, _, err := c.api.ListAccessApplications(ctx, cf.AccountIdentifier(accountID), cf.ListAccessApplicationsParams{})
	if err != nil {
		return "", fmt.Errorf("listing access applications: %w", err)
	}
	for _, app := range apps {
		if app.Domain == hostname {
			logger.V(1).Info("access application already exists", "appID", app.ID)
			return app.ID, nil
		}
	}

	logger.V(1).Info("creating access application")
	app, err := c.api.CreateAccessApplication(ctx, cf.AccountIdentifier(accountID), cf.CreateAccessApplicationParams{
		Name:            hostname,
		Domain:          hostname,
		Type:            cf.SelfHosted,
		SessionDuration: "24h",
	})
	if err != nil {
		return "", fmt.Errorf("creating access application for %s: %w", hostname, err)
	}
	logger.V(1).Info("access application created", "appID", app.ID)
	return app.ID, nil
}

// DeleteAccessApp removes a Cloudflare Access Application by ID. A 404 is treated as success.
func (c *cfClient) DeleteAccessApp(ctx context.Context, accountID, appID string) error {
	logger := log.FromContext(ctx).WithValues("appID", appID, "accountID", accountID)
	logger.V(1).Info("deleting access application")
	if err := c.api.DeleteAccessApplication(ctx, cf.AccountIdentifier(accountID), appID); err != nil {
		if isNotFound(err) {
			logger.V(1).Info("access application already gone, skipping")
			return nil
		}
		return fmt.Errorf("deleting access application %s: %w", appID, err)
	}
	logger.V(1).Info("access application deleted")
	return nil
}

// FindDNSRecordByHostname returns the CNAME record ID for hostname, or "" if not found.
func (c *cfClient) FindDNSRecordByHostname(ctx context.Context, zoneID, hostname string) (string, error) {
	records, _, err := c.api.ListDNSRecords(ctx, cf.ZoneIdentifier(zoneID), cf.ListDNSRecordsParams{
		Type: "CNAME",
		Name: hostname,
	})
	if err != nil {
		return "", fmt.Errorf("listing DNS records for %s: %w", hostname, err)
	}
	if len(records) == 0 {
		return "", nil
	}
	return records[0].ID, nil
}

// FindAccessAppByHostname returns the Access Application ID for hostname, or "" if not found.
func (c *cfClient) FindAccessAppByHostname(ctx context.Context, accountID, hostname string) (string, error) {
	apps, _, err := c.api.ListAccessApplications(ctx, cf.AccountIdentifier(accountID), cf.ListAccessApplicationsParams{})
	if err != nil {
		return "", fmt.Errorf("listing access applications: %w", err)
	}
	for _, app := range apps {
		if app.Domain == hostname {
			return app.ID, nil
		}
	}
	return "", nil
}

// SyncAccessPolicies ensures all desired policies exist on the Access Application.
// Policies are matched by name (the spec raw value). Missing ones are created;
// existing ones are left as-is. No policies are ever deleted — removal must be
// done manually since the controller cannot distinguish owned from pre-existing policies.
func (c *cfClient) SyncAccessPolicies(ctx context.Context, accountID, appID string, specs []PolicySpec) error {
	logger := log.FromContext(ctx).WithValues("accountID", accountID, "appID", appID)

	existing, _, err := c.api.ListAccessPolicies(ctx, cf.AccountIdentifier(accountID), cf.ListAccessPoliciesParams{
		ApplicationID: appID,
	})
	if err != nil {
		return fmt.Errorf("listing access policies: %w", err)
	}

	existingByName := make(map[string]cf.AccessPolicy, len(existing))
	for _, p := range existing {
		existingByName[p.Name] = p
	}

	desired := make(map[string]PolicySpec, len(specs))
	for _, s := range specs {
		desired[s.name()] = s
	}

	// Create any missing desired policies.
	for name, spec := range desired {
		if _, ok := existingByName[name]; ok {
			logger.V(1).Info("access policy already exists", "policy", name)
			continue
		}
		include := spec.include()
		if include == nil {
			logger.Info("unrecognised access policy spec, skipping", "spec", spec.Raw)
			continue
		}
		if _, err := c.api.CreateAccessPolicy(ctx, cf.AccountIdentifier(accountID), cf.CreateAccessPolicyParams{
			ApplicationID: appID,
			Name:          name,
			Decision:      spec.decision(),
			Include:       include,
		}); err != nil {
			return fmt.Errorf("creating access policy %q: %w", name, err)
		}
		logger.Info("access policy created", "policy", name)
	}

	return nil
}

// isNotFound returns true when the Cloudflare API returns a 404-equivalent error.
// Cloudflare-go wraps API errors as *cf.RequestError; a nil or absent resource
// typically surfaces as error code 7003 (Invalid resource) or 1001 (not found).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var reqErr *cf.RequestError
	if errors.As(err, &reqErr) {
		return reqErr.InternalErrorCodeIs(7003) ||
			reqErr.InternalErrorCodeIs(1001) ||
			reqErr.InternalErrorCodeIs(81044)
	}
	return false
}

// IsUnknownApplication returns true when the Cloudflare API returns error 11021
// (access.api.error.unknown_application), meaning the Access Application ID is stale.
func IsUnknownApplication(err error) bool {
	if err == nil {
		return false
	}
	var reqErr cf.RequestError
	if errors.As(err, &reqErr) {
		for _, code := range reqErr.ErrorCodes() {
			if code == 11021 {
				return true
			}
		}
	}
	return false
}
