package main

import (
	"context"
	"encoding/json"
	"errors"
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

// VersionChecker checks for new releases and reports update availability. It is safe for concurrent use.
type VersionChecker struct {
	mu     sync.RWMutex
	latest string
	etag   string        // For conditional requests (304 Not Modified)
	stopCh chan struct{} // Close to signal goroutine to stop
}

// NewVersionChecker returns a new VersionChecker that checks for updates.
func NewVersionChecker() *VersionChecker {
	vc := &VersionChecker{
		stopCh: make(chan struct{}),
	}
	go vc.run()
	return vc
}

// Stop stops the version checker.
func (vc *VersionChecker) Stop() {
	close(vc.stopCh)
}

// run executes the version check loop.
func (vc *VersionChecker) run() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in version checker", "panic", r)
		}
	}()

	// Initial delay before first check
	select {
	case <-time.After(versionCheckDelay):
		vc.checkWithRetry()
	case <-vc.stopCh:
		return
	}

	ticker := time.NewTicker(versionCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			vc.checkWithRetry()
		case <-vc.stopCh:
			return
		}
	}
}

// checkWithRetry performs the version check with retries on failure.
func (vc *VersionChecker) checkWithRetry() {
	for attempt := range versionMaxRetries {
		if vc.check() {
			return
		}
		if attempt < versionMaxRetries-1 {
			select {
			case <-time.After(versionRetryDelay):
				// Continue to next retry
			case <-vc.stopCh:
				return
			}
		}
	}
}

// githubRelease represents a release with version and status information.
type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// check retrieves the latest release information and reports whether the check succeeded.
func (vc *VersionChecker) check() bool {
	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		versionCheckTimeout,
		errors.New("github API request timeout"),
	)
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

// normalizeVersion returns a normalized version string.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// canonicalVersion returns the version in canonical semver format.
func canonicalVersion(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// isNewerVersion reports whether latest is newer than current.
func isNewerVersion(latest, current string) bool {
	latestCanon := canonicalVersion(latest)
	currentCanon := canonicalVersion(current)

	// semver.Compare returns 1 if latestCanon > currentCanon
	return semver.Compare(latestCanon, currentCanon) > 0
}
