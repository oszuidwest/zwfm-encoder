package notify

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const (
	// expiryWarningDays is the number of days before expiration to show a warning.
	expiryWarningDays = 30
	// expiryCacheTTL is how long to cache the expiry info before re-checking.
	expiryCacheTTL = 1 * time.Hour
	// expiryTimeout bounds one secret expiry lookup.
	expiryTimeout = 30 * time.Second
)

// SecretExpiryChecker checks client secret expiration.
type SecretExpiryChecker struct {
	mu         sync.Mutex
	cfg        *types.GraphConfig
	cached     types.SecretExpiryInfo
	lastCheck  time.Time
	refreshing bool
}

// NewSecretExpiryChecker creates a new expiry checker for the given config.
func NewSecretExpiryChecker(cfg *types.GraphConfig) *SecretExpiryChecker {
	return &SecretExpiryChecker{cfg: cfg}
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
		info, err = fetchExpiryInfo(cfg)
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

// fetchExpiryInfo looks up when the configured client secret expires.
func fetchExpiryInfo(cfg *types.GraphConfig) (types.SecretExpiryInfo, error) {
	client, err := newMailClient(cfg)
	if err != nil {
		return types.SecretExpiryInfo{}, err
	}

	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		expiryTimeout,
		errors.New("graph API request timeout"),
	)
	defer cancel()

	earliest, err := client.SecretExpiry(ctx)
	if err != nil {
		return types.SecretExpiryInfo{}, err
	}

	daysLeft := max(int(time.Until(earliest).Hours()/24), 0)

	return types.SecretExpiryInfo{
		ExpiresAt:   earliest.Format(time.RFC3339),
		ExpiresSoon: daysLeft <= expiryWarningDays,
		DaysLeft:    daysLeft,
	}, nil
}
