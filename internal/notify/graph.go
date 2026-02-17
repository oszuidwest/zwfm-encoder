package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	graphBaseURL     = "https://graph.microsoft.com/v1.0"
	graphScope       = "https://graph.microsoft.com/.default"
	tokenURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token" //nolint:gosec // URL template, not a credential

	// Retry settings.
	maxRetries       = 3
	initialRetryWait = 1 * time.Second
	maxRetryWait     = 30 * time.Second

	// HTTP client timeout.
	httpTimeout = 30 * time.Second
)

// guidPattern matches the standard GUID format.
var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateCredentials checks that required credential fields are present.
// If strict is true, validates GUID format for TenantID and ClientID.
func validateCredentials(cfg *types.GraphConfig, strict bool) error {
	if cfg.TenantID == "" {
		return fmt.Errorf("tenant ID is required")
	}
	if strict && !guidPattern.MatchString(cfg.TenantID) {
		return fmt.Errorf("tenant ID must be a valid GUID (e.g., 12345678-1234-1234-1234-123456789abc)")
	}
	if cfg.ClientID == "" {
		return fmt.Errorf("client ID is required")
	}
	if strict && !guidPattern.MatchString(cfg.ClientID) {
		return fmt.Errorf("client ID must be a valid GUID (e.g., 12345678-1234-1234-1234-123456789abc)")
	}
	if cfg.ClientSecret == "" {
		return fmt.Errorf("client secret is required")
	}
	return nil
}

// newCredentialsConfig creates an OAuth2 credentials configuration.
func newCredentialsConfig(cfg *types.GraphConfig) *clientcredentials.Config {
	return &clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		TokenURL:     fmt.Sprintf(tokenURLTemplate, cfg.TenantID),
		Scopes:       []string{graphScope},
	}
}

// GraphClient sends emails via Microsoft Graph API.
type GraphClient struct {
	fromAddress string
	httpClient  *http.Client
}

// NewGraphClient creates a new email client.
func NewGraphClient(cfg *types.GraphConfig) (*GraphClient, error) {
	if err := validateCredentials(cfg, false); err != nil {
		return nil, err
	}
	if cfg.FromAddress == "" {
		return nil, fmt.Errorf("from address (shared mailbox) is required")
	}

	conf := newCredentialsConfig(cfg)

	// Configure base HTTP client with timeout to prevent indefinite hangs
	baseClient := &http.Client{Timeout: httpTimeout}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, baseClient)
	httpClient := conf.Client(ctx)

	return &GraphClient{
		fromAddress: cfg.FromAddress,
		httpClient:  httpClient,
	}, nil
}

// graphMailRequest represents a send email request.
type graphMailRequest struct {
	Message graphMessage `json:"message"`
}

type graphMessage struct {
	Subject      string            `json:"subject"`
	Body         graphBody         `json:"body"`
	ToRecipients []graphRecipient  `json:"toRecipients"`
	Attachments  []graphAttachment `json:"attachments,omitempty"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphRecipient struct {
	EmailAddress graphEmailAddress `json:"emailAddress"`
}

type graphEmailAddress struct {
	Address string `json:"address"`
}

// graphAttachment represents an email attachment.
type graphAttachment struct {
	OdataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	ContentBytes string `json:"contentBytes"` // Base64-encoded
}

// EmailAttachment represents an email attachment.
type EmailAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// SendMail sends an email to the specified recipients.
func (c *GraphClient) SendMail(recipients []string, subject, body string) error {
	return c.SendMailWithAttachment(recipients, subject, body, nil)
}

// doWithRetry sends the email request with automatic retries.
func (c *GraphClient) doWithRetry(jsonData []byte) error {
	apiURL := fmt.Sprintf("%s/users/%s/sendMail", graphBaseURL, url.PathEscape(c.fromAddress))
	backoff := util.NewBackoff(initialRetryWait, maxRetryWait)

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff.Next())
		}

		req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(jsonData))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("send request: %w", err)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusAccepted, http.StatusOK, http.StatusNoContent:
			return nil
		case http.StatusTooManyRequests:
			// Parse Retry-After header if present (integer seconds only)
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
					time.Sleep(time.Duration(seconds) * time.Second)
				}
			}
			lastErr = fmt.Errorf("graph API rate limited (429): %s", string(respBody))
			continue
		case http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// Transient server errors - retry
			lastErr = fmt.Errorf("graph API returned %d: %s", resp.StatusCode, string(respBody))
			continue
		default:
			return fmt.Errorf("graph API error %d: %s", resp.StatusCode, string(respBody))
		}
	}

	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

// SendMailWithAttachment sends an email with an optional attachment.
func (c *GraphClient) SendMailWithAttachment(recipients []string, subject, body string, attachment *EmailAttachment) error {
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients specified")
	}

	toRecipients := make([]graphRecipient, 0, len(recipients))
	for _, addr := range recipients {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			toRecipients = append(toRecipients, graphRecipient{
				EmailAddress: graphEmailAddress{Address: addr},
			})
		}
	}

	if len(toRecipients) == 0 {
		return fmt.Errorf("no valid recipients after filtering")
	}

	message := graphMessage{
		Subject: subject,
		Body: graphBody{
			ContentType: "Text",
			Content:     body,
		},
		ToRecipients: toRecipients,
	}

	// Add attachment if provided and valid
	if attachment != nil && len(attachment.Data) > 0 {
		message.Attachments = []graphAttachment{
			{
				OdataType:    "#microsoft.graph.fileAttachment",
				Name:         attachment.Filename,
				ContentType:  attachment.ContentType,
				ContentBytes: base64.StdEncoding.EncodeToString(attachment.Data),
			},
		}
	}

	payload := graphMailRequest{Message: message}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	return c.doWithRetry(jsonData)
}

// ValidateAuth verifies that the email credentials are valid.
func (c *GraphClient) ValidateAuth() error {
	// The httpClient already has a token source configured.
	// Making any request will trigger token acquisition.
	// We use a lightweight request to /me endpoint which will fail with 403
	// for app-only auth, but the token acquisition itself validates credentials.
	apiURL := fmt.Sprintf("%s/users/%s", graphBaseURL, url.PathEscape(c.fromAddress))
	req, err := http.NewRequest(http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("create validation request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Token acquisition failed
		if strings.Contains(err.Error(), "oauth2") || strings.Contains(err.Error(), "token") {
			return fmt.Errorf("authentication failed: %w", err)
		}
		return fmt.Errorf("validation request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 200 = user exists and accessible
	// 403 = token valid but no User.Read permission (acceptable for Mail.Send only)
	// 404 = user/mailbox not found
	switch resp.StatusCode {
	case http.StatusOK, http.StatusForbidden:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("mailbox %s not found", c.fromAddress)
	case http.StatusUnauthorized:
		return fmt.Errorf("authentication failed: invalid credentials")
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("validation failed with status %d: %s", resp.StatusCode, string(body))
	}
}

// ValidateConfig validates that cfg has all required fields.
func ValidateConfig(cfg *types.GraphConfig) error {
	if err := validateCredentials(cfg, true); err != nil {
		return err
	}
	if cfg.FromAddress == "" {
		return fmt.Errorf("from address (shared mailbox) is required")
	}
	if cfg.Recipients == "" {
		return fmt.Errorf("recipients are required")
	}
	return nil
}

// IsConfigured reports whether the Graph configuration has the minimum required fields.
func IsConfigured(cfg *types.GraphConfig) bool {
	return cfg.TenantID != "" && cfg.ClientID != "" && cfg.ClientSecret != "" &&
		cfg.FromAddress != "" && cfg.Recipients != ""
}

// ParseRecipients splits a comma-separated recipients string into a slice.
func ParseRecipients(recipients string) []string {
	var result []string
	for r := range strings.SplitSeq(recipients, ",") {
		if r = strings.TrimSpace(r); r != "" {
			result = append(result, r)
		}
	}
	return result
}

// TokenSourceContext returns an OAuth2 token source bound to the given context.
// The context controls token acquisition timeouts.
func TokenSourceContext(ctx context.Context, cfg *types.GraphConfig) (oauth2.TokenSource, error) {
	if err := validateCredentials(cfg, false); err != nil {
		return nil, err
	}
	return newCredentialsConfig(cfg).TokenSource(ctx), nil
}
