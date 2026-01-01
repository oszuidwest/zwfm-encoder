package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"golang.org/x/mod/semver"
)

const (
	githubRepo           = "oszuidwest/zwfm-encoder"
	versionCheckInterval = 24 * time.Hour
	versionCheckDelay    = 30000 * time.Millisecond // Delay before first check to avoid blocking startup
	versionCheckTimeout  = 30000 * time.Millisecond // HTTP request timeout
	versionMaxRetries    = 3                        // Max retries per check cycle
	versionRetryDelay    = 1 * time.Minute          // Delay between retries
)

// VersionChecker is a background service that checks GitHub for new releases.
type VersionChecker struct {
	mu     sync.RWMutex
	latest string
	etag   string // For conditional requests (304 Not Modified)
}

// NewVersionChecker creates and starts a version checker.
func NewVersionChecker() *VersionChecker {
	vc := &VersionChecker{}
	go vc.run()
	return vc
}

// run executes the periodic version check loop.
func (vc *VersionChecker) run() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in version checker", "panic", r)
		}
	}()

	time.Sleep(versionCheckDelay)
	vc.checkWithRetry()

	ticker := time.NewTicker(versionCheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		vc.checkWithRetry()
	}
}

// checkWithRetry performs the version check with automatic retries.
func (vc *VersionChecker) checkWithRetry() {
	for attempt := range versionMaxRetries {
		if vc.check() {
			return
		}
		if attempt < versionMaxRetries-1 {
			time.Sleep(versionRetryDelay)
		}
	}
}

// githubRelease is the GitHub API response structure for a release.
type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// check fetches the latest release from GitHub and reports success.
func (vc *VersionChecker) check() bool {
	ctx, cancel := context.WithTimeout(context.Background(), versionCheckTimeout)
	defer cancel()

	url := "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false
	}

	// Set required GitHub API headers.
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "zwfm-encoder/"+Version)

	vc.mu.RLock()
	etag := vc.etag
	vc.mu.RUnlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_ = resp.Body.Close() //nolint:errcheck // Best-effort cleanup; error doesn't affect caller
	}()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		// No changes since last check - success
		return true
	case http.StatusNotFound:
		// No releases exist yet - not an error
		return true
	case http.StatusForbidden, http.StatusTooManyRequests:
		// Rate limited - retry later
		return false
	default:
		if resp.StatusCode >= 500 {
			// Server error - retry
			return false
		}
		// Other client errors - don't retry
		return true
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return false
	}

	if release.Draft || release.Prerelease {
		return true
	}

	if release.TagName == "" {
		return false
	}

	vc.mu.Lock()
	vc.latest = normalizeVersion(release.TagName)
	if newEtag := resp.Header.Get("ETag"); newEtag != "" {
		vc.etag = newEtag
	}
	vc.mu.Unlock()

	return true
}

// Info returns the current version info for the frontend.
func (vc *VersionChecker) Info() types.VersionInfo {
	vc.mu.RLock()
	defer vc.mu.RUnlock()

	current := normalizeVersion(Version)
	info := types.VersionInfo{
		Current:   current,
		Latest:    vc.latest,
		Commit:    Commit,
		BuildTime: util.FormatHumanTime(BuildTime),
	}

	// Determine if an update is available.
	if vc.latest != "" && current != "dev" && current != "unknown" {
		info.UpdateAvail = isNewerVersion(vc.latest, current)
	}

	return info
}

// normalizeVersion returns the version without 'v' prefix and whitespace.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// canonicalVersion returns the version in semver canonical form with v prefix.
func canonicalVersion(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// isNewerVersion returns true if latest is newer than current using semver comparison.
func isNewerVersion(latest, current string) bool {
	latestCanon := canonicalVersion(latest)
	currentCanon := canonicalVersion(current)

	// semver.Compare returns 1 if latestCanon > currentCanon
	return semver.Compare(latestCanon, currentCanon) > 0
}
