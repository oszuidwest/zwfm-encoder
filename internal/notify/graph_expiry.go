package notify

import (
	"context"
	"encoding/json"
	"errors"
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

// SecretExpiryChecker checks client secret expiration.
type SecretExpiryChecker struct {
	mu         sync.Mutex
	cfg        *types.GraphConfig
	cached     types.SecretExpiryInfo
	lastCheck  time.Time
	refreshing bool
	httpClient *http.Client
}

// NewSecretExpiryChecker creates a new expiry checker for the given config.
func NewSecretExpiryChecker(cfg *types.GraphConfig) *SecretExpiryChecker {
	return &SecretExpiryChecker{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// Info returns the cached secret expiry information. When the cache is stale it
// starts a single background refresh and serves the previous value, so callers
// (the per-client WebSocket status tick) never block on Graph API latency.
func (c *SecretExpiryChecker) Info() types.SecretExpiryInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	stale := c.lastCheck.IsZero() || time.Since(c.lastCheck) >= expiryCacheTTL
	if stale && !c.refreshing {
		c.refreshing = true
		go c.refresh()
	}
	return c.cached
}

// UpdateConfig updates the configuration.
func (c *SecretExpiryChecker) UpdateConfig(cfg *types.GraphConfig) {
	c.mu.Lock()
	c.cfg = cfg
	c.lastCheck = time.Time{} // Invalidate cache
	c.mu.Unlock()
}

// refresh fetches fresh expiry information in the background and stores it,
// unless the config was swapped while the fetch was in flight (then the next
// Info call triggers a fresh refresh against the new config).
func (c *SecretExpiryChecker) refresh() {
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	var info types.SecretExpiryInfo
	if cfg == nil || cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		info = types.SecretExpiryInfo{Error: "Graph API not configured"}
	} else {
		var err error
		info, err = c.fetchExpiryInfo(cfg)
		if err != nil {
			info = types.SecretExpiryInfo{Error: err.Error()}
		}
	}

	c.mu.Lock()
	if c.cfg == cfg {
		c.cached = info
		c.lastCheck = time.Now()
	}
	c.refreshing = false
	c.mu.Unlock()
}

// applicationResponse represents an application response.
type applicationResponse struct {
	PasswordCredentials []passwordCredential `json:"passwordCredentials"`
}

type passwordCredential struct {
	EndDateTime string `json:"endDateTime"`
}

// fetchExpiryInfo queries for credential expiry information.
func (c *SecretExpiryChecker) fetchExpiryInfo(cfg *types.GraphConfig) (types.SecretExpiryInfo, error) {
	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		httpTimeout,
		errors.New("graph API request timeout"),
	)
	defer cancel()

	ts, err := TokenSourceContext(ctx, cfg)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("create token source: %w", err)
	}

	token, err := ts.Token()
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("acquire token: %w", err)
	}

	apiURL := fmt.Sprintf("%s/applications(appId='%s')", graphBaseURL, url.PathEscape(cfg.ClientID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return types.SecretExpiryInfo{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	//nolint:gosec // G704: URL is constructed from constant Graph API base URL + config values
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

	// Find the earliest non-expired credential
	now := time.Now()
	var earliest time.Time
	for _, cred := range appResp.PasswordCredentials {
		if cred.EndDateTime == "" {
			continue
		}
		expiry, err := time.Parse(time.RFC3339, cred.EndDateTime)
		if err != nil {
			continue
		}
		if expiry.Before(now) {
			continue // Skip already-expired credentials
		}
		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}

	if earliest.IsZero() {
		return types.SecretExpiryInfo{Error: "no valid (non-expired) credentials found"}, nil
	}

	daysLeft := max(int(time.Until(earliest).Hours()/24), 0)

	return types.SecretExpiryInfo{
		ExpiresAt:   earliest.Format(time.RFC3339),
		ExpiresSoon: daysLeft <= expiryWarningDays,
		DaysLeft:    daysLeft,
	}, nil
}
