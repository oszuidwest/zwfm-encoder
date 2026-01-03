package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const (
	// expiryWarningDays is the number of days before expiration to show a warning.
	expiryWarningDays = 30
	// expiryCacheTTL is how long to cache the expiry info before re-checking.
	expiryCacheTTL = 1 * time.Hour
)

// SecretExpiryChecker checks client secret expiration on-demand with caching.
type SecretExpiryChecker struct {
	mu         sync.RWMutex
	cfg        *types.GraphConfig
	cached     types.SecretExpiryInfo
	lastCheck  time.Time
	httpClient *http.Client
}

// NewSecretExpiryChecker creates a new expiry checker for the given config.
func NewSecretExpiryChecker(cfg *types.GraphConfig) *SecretExpiryChecker {
	return &SecretExpiryChecker{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// GetInfo returns the secret expiry information.
func (c *SecretExpiryChecker) GetInfo() types.SecretExpiryInfo {
	c.mu.RLock()
	if time.Since(c.lastCheck) < expiryCacheTTL && c.lastCheck.After(time.Time{}) {
		info := c.cached
		c.mu.RUnlock()
		return info
	}
	c.mu.RUnlock()

	// Cache is stale, fetch fresh data
	return c.refresh()
}

// UpdateConfig updates the configuration.
func (c *SecretExpiryChecker) UpdateConfig(cfg *types.GraphConfig) {
	c.mu.Lock()
	c.cfg = cfg
	c.lastCheck = time.Time{} // Invalidate cache
	c.mu.Unlock()
}

// refresh fetches fresh expiry info from Azure AD.
func (c *SecretExpiryChecker) refresh() types.SecretExpiryInfo {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	if cfg == nil || cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		info := types.SecretExpiryInfo{Error: "Graph API not configured"}
		c.mu.Lock()
		c.cached = info
		c.lastCheck = time.Now()
		c.mu.Unlock()
		return info
	}

	info, err := c.fetchExpiryInfo(cfg)
	if err != nil {
		info = types.SecretExpiryInfo{Error: err.Error()}
	}

	c.mu.Lock()
	c.cached = info
	c.lastCheck = time.Now()
	c.mu.Unlock()

	return info
}

// applicationResponse represents the Graph API response for an application.
type applicationResponse struct {
	PasswordCredentials []passwordCredential `json:"passwordCredentials"`
}

type passwordCredential struct {
	EndDateTime string `json:"endDateTime"`
}

// fetchExpiryInfo queries the Azure AD Graph API for credential expiry.
func (c *SecretExpiryChecker) fetchExpiryInfo(cfg *types.GraphConfig) (types.SecretExpiryInfo, error) {
	ts, err := TokenSource(cfg)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("create token source: %w", err)
	}

	token, err := ts.Token()
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("acquire token: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	apiURL := fmt.Sprintf("%s/applications(appId='%s')", graphBaseURL, url.PathEscape(cfg.ClientID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return types.SecretExpiryInfo{}, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	var appResp applicationResponse
	if err := json.Unmarshal(body, &appResp); err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("parse response: %w", err)
	}

	// Find the earliest expiring credential
	var earliest time.Time
	for _, cred := range appResp.PasswordCredentials {
		if cred.EndDateTime == "" {
			continue
		}
		expiry, err := time.Parse(time.RFC3339, cred.EndDateTime)
		if err != nil {
			continue
		}
		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}

	if earliest.IsZero() {
		return types.SecretExpiryInfo{Error: "no password credentials found"}, nil
	}

	daysLeft := int(time.Until(earliest).Hours() / 24)
	if daysLeft < 0 {
		daysLeft = 0
	}

	return types.SecretExpiryInfo{
		ExpiresAt:   earliest.Format(time.RFC3339),
		ExpiresSoon: daysLeft <= expiryWarningDays,
		DaysLeft:    daysLeft,
	}, nil
}
