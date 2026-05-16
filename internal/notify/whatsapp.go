package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const (
	defaultWhatsAppTemplateLanguage = "en_US"
	maxWhatsAppTextRunes            = 4096
	maxWhatsAppErrorBodyBytes       = 4096
	maxWhatsAppErrorSnippetRunes    = 256
	whatsAppSendTimeout             = 10 * time.Second

	whatsAppMessagingProduct = "whatsapp"
	whatsAppRecipientType    = "individual"
	whatsAppMessageText      = "text"
	whatsAppMessageTemplate  = "template"
	whatsAppComponentBody    = "body"
)

var (
	whatsappClient = &http.Client{Timeout: whatsAppSendTimeout}
	// whatsappAPIBaseURL is mutable so tests can point requests at httptest.Server.
	whatsappAPIBaseURL = "https://graph.facebook.com/v24.0"
	ErrWhatsAppConfig  = errors.New("invalid WhatsApp configuration")

	// whatsAppRuntimePriority preserves the previous validator's first-error
	// behavior: more fundamental misconfiguration is reported before downstream
	// issues like recipients-required.
	whatsAppRuntimePriority = []types.WhatsAppValidationCode{
		types.WhatsAppConfigRequired,
		types.WhatsAppPhoneNumberIDRequired,
		types.WhatsAppPhoneNumberIDDigits,
		types.WhatsAppAccessTokenRequired,
		types.WhatsAppTemplateNameFormat,
		types.WhatsAppTemplateLanguageRequiresName,
		types.WhatsAppTemplateLanguageWhitespace,
		types.WhatsAppRecipientsRequired,
		types.WhatsAppRecipientInvalid,
	}
)

// WhatsAppConfig is the configuration for WhatsApp Cloud API notifications.
type WhatsAppConfig = types.WhatsAppConfig

type whatsappTextRequest struct {
	MessagingProduct string       `json:"messaging_product"`
	RecipientType    string       `json:"recipient_type"`
	To               string       `json:"to"`
	Type             string       `json:"type"`
	Text             whatsappText `json:"text"`
}

type whatsappTemplateRequest struct {
	MessagingProduct string           `json:"messaging_product"`
	RecipientType    string           `json:"recipient_type"`
	To               string           `json:"to"`
	Type             string           `json:"type"`
	Template         whatsappTemplate `json:"template"`
}

type whatsappText struct {
	PreviewURL bool   `json:"preview_url"`
	Body       string `json:"body"`
}

type whatsappTemplate struct {
	Name       string                      `json:"name"`
	Language   whatsappTemplateLanguage    `json:"language"`
	Components []whatsappTemplateComponent `json:"components,omitempty"`
}

type whatsappTemplateLanguage struct {
	Code string `json:"code"`
}

type whatsappTemplateComponent struct {
	Type       string                      `json:"type"`
	Parameters []whatsappTemplateParameter `json:"parameters"`
}

type whatsappTemplateParameter struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type whatsappErrorResponse struct {
	Error struct {
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      int    `json:"code"`
		ErrorData struct {
			Details string `json:"details"`
		} `json:"error_data"`
		TraceID string `json:"fbtrace_id"`
	} `json:"error"`
}

// WhatsAppChannel implements AlertChannel for WhatsApp Business Cloud API delivery.
type WhatsAppChannel struct{}

// Name returns the channel identifier used in logs.
func (c *WhatsAppChannel) Name() string { return "whatsapp" }

// IsConfiguredForSilence reports whether the channel participates in silence flows.
func (c *WhatsAppChannel) IsConfiguredForSilence(cfg *config.Snapshot) bool { return cfg.HasWhatsApp() }

// IsConfiguredForUpload reports whether the channel participates in upload-abandonment flows.
func (c *WhatsAppChannel) IsConfiguredForUpload(cfg *config.Snapshot) bool { return cfg.HasWhatsApp() }

// SubscribesSilenceStart reports whether silence-start events should be sent.
func (c *WhatsAppChannel) SubscribesSilenceStart(cfg *config.Snapshot) bool {
	return cfg.HasWhatsApp() && cfg.WhatsAppEvents.SilenceStart
}

// SubscribesSilenceEnd reports whether silence-end events should be sent.
func (c *WhatsAppChannel) SubscribesSilenceEnd(cfg *config.Snapshot) bool {
	return cfg.HasWhatsApp() && cfg.WhatsAppEvents.SilenceEnd
}

// SubscribesAudioDump reports whether audio-dump events should be sent.
func (c *WhatsAppChannel) SubscribesAudioDump(cfg *config.Snapshot) bool {
	return cfg.HasWhatsApp() && cfg.WhatsAppEvents.AudioDump
}

// SendSilenceStart sends a WhatsApp alert when silence starts.
func (c *WhatsAppChannel) SendSilenceStart(ctx context.Context, cfg *config.Snapshot, levelL, levelR float64) error {
	whatsAppCfg := BuildWhatsAppConfig(cfg)
	body := fmt.Sprintf(
		"[ALERT] Silence Detected - %s\n\n"+
			"The encoder detected silence at %s.\n\n"+
			"Audio level dropped below the %.0f dB threshold.\n"+
			"Current level: Left %.1f dB / Right %.1f dB\n\n"+
			"Silence is ongoing. Please check the audio source.",
		cfg.StationName, util.HumanTime(), cfg.SilenceThreshold, levelL, levelR,
	)
	return sendWhatsAppMessage(ctx, whatsAppCfg, body)
}

// SendSilenceEnd sends a WhatsApp alert when audio recovers after silence.
func (c *WhatsAppChannel) SendSilenceEnd(
	ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
) error {
	whatsAppCfg := BuildWhatsAppConfig(cfg)
	body := fmt.Sprintf(
		"[OK] Audio Restored - %s\n\n"+
			"Audio was restored at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)",
		cfg.StationName, util.HumanTime(), util.FormatDuration(durationMS), levelL, levelR, cfg.SilenceThreshold,
	)
	return sendWhatsAppMessage(ctx, whatsAppCfg, body)
}

// SendAudioDump sends a WhatsApp alert with the status of the silence audio dump.
func (c *WhatsAppChannel) SendAudioDump(
	ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
	result *silencedump.EncodeResult,
) error {
	whatsAppCfg := BuildWhatsAppConfig(cfg)
	body := fmt.Sprintf(
		"[DUMP] Audio Recording - %s\n\n"+
			"Audio dump ready at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)\n\n"+
			"%s",
		cfg.StationName,
		util.HumanTime(),
		util.FormatDuration(durationMS),
		levelL,
		levelR,
		cfg.SilenceThreshold,
		whatsAppDumpDetail(result),
	)
	return sendWhatsAppMessage(ctx, whatsAppCfg, body)
}

// SendUploadAbandoned sends a WhatsApp alert for a recording upload that exhausted retries.
func (c *WhatsAppChannel) SendUploadAbandoned(
	ctx context.Context, cfg *config.Snapshot, params UploadAbandonedData,
) error {
	whatsAppCfg := BuildWhatsAppConfig(cfg)
	body := fmt.Sprintf(
		"[ALERT] Upload Abandoned - %s\n\n"+
			"A recording upload was abandoned at %s.\n\n"+
			"Recorder: %s\n"+
			"File: %s\n"+
			"S3 key: %s\n"+
			"Retries: %d\n"+
			"Last error: %s\n\n"+
			"The file could not be uploaded to S3 after exhausting all retries.",
		cfg.StationName,
		util.HumanTime(),
		params.RecorderName,
		params.Filename,
		params.S3Key,
		params.RetryCount,
		params.LastError,
	)
	return sendWhatsAppMessage(ctx, whatsAppCfg, body)
}

// SendWhatsAppTest sends a test WhatsApp notification.
func SendWhatsAppTest(ctx context.Context, cfg *WhatsAppConfig, stationName string) error {
	body := fmt.Sprintf(
		"[TEST] %s\n\n"+
			"Test WhatsApp notification from the audio encoder.\n\n"+
			"Time: %s",
		stationName,
		util.HumanTime(),
	)
	return sendWhatsAppMessage(ctx, cfg, body)
}

// BuildWhatsAppConfig builds a WhatsAppConfig from a config snapshot.
func BuildWhatsAppConfig(cfg *config.Snapshot) *WhatsAppConfig {
	return &WhatsAppConfig{
		PhoneNumberID:    cfg.WhatsAppPhoneNumberID,
		AccessToken:      cfg.WhatsAppAccessToken,
		Recipients:       cfg.WhatsAppRecipients,
		TemplateName:     cfg.WhatsAppTemplateName,
		TemplateLanguage: cfg.WhatsAppTemplateLanguage,
		Events:           cfg.WhatsAppEvents,
	}
}

func validateWhatsAppConfig(cfg *WhatsAppConfig) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: configuration is required", ErrWhatsAppConfig)
	}
	if err := formatWhatsAppRuntimeError(cfg.Validate(types.WhatsAppRequireComplete)); err != nil {
		return nil, err
	}

	return util.ParseWhatsAppRecipients(cfg.Recipients), nil
}

func formatWhatsAppRuntimeError(issues []types.WhatsAppValidationIssue) error {
	for _, code := range whatsAppRuntimePriority {
		for _, issue := range issues {
			if issue.Code == code {
				return formatWhatsAppRuntimeIssue(issue)
			}
		}
	}
	if len(issues) > 0 {
		return formatWhatsAppRuntimeIssue(issues[0])
	}
	return nil
}

func formatWhatsAppRuntimeIssue(issue types.WhatsAppValidationIssue) error {
	switch issue.Code {
	case types.WhatsAppConfigRequired:
		return fmt.Errorf("%w: configuration is required", ErrWhatsAppConfig)
	case types.WhatsAppPhoneNumberIDRequired:
		return fmt.Errorf("%w: phone number ID is required", ErrWhatsAppConfig)
	case types.WhatsAppPhoneNumberIDDigits:
		return fmt.Errorf("%w: phone number ID must contain digits only", ErrWhatsAppConfig)
	case types.WhatsAppAccessTokenRequired:
		return fmt.Errorf("%w: access token is required", ErrWhatsAppConfig)
	case types.WhatsAppTemplateNameFormat:
		return fmt.Errorf("%w: template name is invalid", ErrWhatsAppConfig)
	case types.WhatsAppTemplateLanguageRequiresName:
		return fmt.Errorf("%w: template language requires template name", ErrWhatsAppConfig)
	case types.WhatsAppTemplateLanguageWhitespace:
		return fmt.Errorf("%w: template language cannot contain whitespace", ErrWhatsAppConfig)
	case types.WhatsAppRecipientsRequired:
		return fmt.Errorf("%w: recipients are required", ErrWhatsAppConfig)
	case types.WhatsAppRecipientInvalid:
		return fmt.Errorf("%w: recipient %q is invalid", ErrWhatsAppConfig, issue.Value)
	default:
		return fmt.Errorf("%w: invalid WhatsApp configuration", ErrWhatsAppConfig)
	}
}

func sendWhatsAppMessage(ctx context.Context, cfg *WhatsAppConfig, body string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	recipients, err := validateWhatsAppConfig(cfg)
	if err != nil {
		return err
	}

	useTemplate := strings.TrimSpace(cfg.TemplateName) != ""
	var (
		textBody     string
		templateBody string
	)
	if useTemplate {
		templateBody = sanitizeWhatsAppTemplateText(body)
	} else {
		textBody = truncateWhatsAppText(body)
	}

	var errs []error
	for _, recipient := range recipients {
		var sendErr error
		if useTemplate {
			sendErr = sendWhatsAppPayload(ctx, cfg, whatsappTemplatePayload(cfg, recipient, templateBody))
		} else {
			sendErr = sendWhatsAppPayload(ctx, cfg, whatsappTextPayload(recipient, textBody))
		}
		if sendErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", recipient, sendErr))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d WhatsApp recipients failed: %w", len(errs), len(recipients), errors.Join(errs...))
	}
	return nil
}

func whatsappTextPayload(recipient, body string) whatsappTextRequest {
	return whatsappTextRequest{
		MessagingProduct: whatsAppMessagingProduct,
		RecipientType:    whatsAppRecipientType,
		To:               recipient,
		Type:             whatsAppMessageText,
		Text: whatsappText{
			PreviewURL: false,
			Body:       body,
		},
	}
}

func whatsappTemplatePayload(cfg *WhatsAppConfig, recipient, body string) whatsappTemplateRequest {
	language := strings.TrimSpace(cfg.TemplateLanguage)
	if language == "" {
		language = defaultWhatsAppTemplateLanguage
	}

	return whatsappTemplateRequest{
		MessagingProduct: whatsAppMessagingProduct,
		RecipientType:    whatsAppRecipientType,
		To:               recipient,
		Type:             whatsAppMessageTemplate,
		Template: whatsappTemplate{
			Name: strings.TrimSpace(cfg.TemplateName),
			Language: whatsappTemplateLanguage{
				Code: language,
			},
			Components: []whatsappTemplateComponent{
				{
					Type: whatsAppComponentBody,
					Parameters: []whatsappTemplateParameter{
						{
							Type: whatsAppMessageText,
							Text: body,
						},
					},
				},
			},
		},
	}
}

func sendWhatsAppPayload(ctx context.Context, cfg *WhatsAppConfig, payload any) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return util.WrapError("marshal WhatsApp payload", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		whatsappMessagesURL(cfg.PhoneNumberID),
		bytes.NewReader(jsonData),
	)
	if err != nil {
		return util.WrapError("create WhatsApp request", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.AccessToken))
	req.Header.Set("Content-Type", "application/json")

	resp, err := whatsappClient.Do(req)
	if err != nil {
		return util.WrapError("send WhatsApp request", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return whatsappStatusError(resp)
	}

	return nil
}

func whatsappMessagesURL(phoneNumberID string) string {
	base := strings.TrimRight(whatsappAPIBaseURL, "/")
	return base + "/" + url.PathEscape(strings.TrimSpace(phoneNumberID)) + "/messages"
}

func whatsappStatusError(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWhatsAppErrorBodyBytes))
	if err != nil {
		return fmt.Errorf("WhatsApp API returned status %d: read error body: %w", resp.StatusCode, err)
	}
	bodyText := strings.TrimSpace(string(body))
	if bodyText == "" {
		return fmt.Errorf("WhatsApp API returned status %d", resp.StatusCode)
	}

	var apiErr whatsappErrorResponse
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
		return whatsappAPIError(resp.StatusCode, apiErr)
	}

	return fmt.Errorf(
		"WhatsApp API returned status %d: error body snippet: %s",
		resp.StatusCode,
		truncateRunes(bodyText, maxWhatsAppErrorSnippetRunes),
	)
}

func whatsappAPIError(statusCode int, apiErr whatsappErrorResponse) error {
	parts := []string{fmt.Sprintf("WhatsApp API returned status %d: %s", statusCode, apiErr.Error.Message)}
	if apiErr.Error.ErrorData.Details != "" {
		parts = append(parts, "details: "+apiErr.Error.ErrorData.Details)
	}
	if apiErr.Error.Code != 0 {
		parts = append(parts, fmt.Sprintf("code: %d", apiErr.Error.Code))
	}
	if apiErr.Error.TraceID != "" {
		parts = append(parts, "trace: "+apiErr.Error.TraceID)
	}
	return errors.New(strings.Join(parts, "; "))
}

func truncateWhatsAppText(body string) string {
	return truncateRunes(body, maxWhatsAppTextRunes)
}

func sanitizeWhatsAppTemplateText(body string) string {
	return truncateWhatsAppText(strings.Join(strings.Fields(body), " "))
}

func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-3]) + "..."
}

func whatsAppDumpDetail(result *silencedump.EncodeResult) string {
	if result == nil {
		return "Audio recording: not available."
	}
	if result.Error != nil {
		return "Audio recording: failed to capture (" + result.Error.Error() + ")."
	}
	if result.Filename == "" {
		return "Audio recording: captured, but no filename is available."
	}
	return fmt.Sprintf("Audio recording: %s (%d bytes).", result.Filename, result.FileSize)
}
