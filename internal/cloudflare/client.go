// Package cloudflare wraps the cloudflare-go SDK with the operations
// needed by the rector controller: idempotent DNS record management
// and Cloudflare Access Application lifecycle.
package cloudflare

import (
	"context"
	"errors"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Client is the interface used by the ServiceReconciler.
// Defined as an interface to allow mocking in tests.
type Client interface {
	EnsureDNSRecord(ctx context.Context, zoneID, hostname, tunnelID string) (recordID string, err error)
	DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error
	EnsureAccessApp(ctx context.Context, accountID, hostname string) (appID string, err error)
	DeleteAccessApp(ctx context.Context, accountID, appID string) error
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
		Comment: "managed by rector",
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
