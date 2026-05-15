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
)

var (
	whatsappClient     = &http.Client{Timeout: 10 * time.Second}
	whatsappAPIBaseURL = "https://graph.facebook.com/v24.0"
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
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code"`
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

func (c *WhatsAppChannel) SendSilenceStart(cfg *config.Snapshot, levelL, levelR float64) error {
	whatsAppCfg := BuildWhatsAppConfig(cfg)
	body := fmt.Sprintf(
		"[ALERT] Silence Detected - %s\n\n"+
			"The encoder detected silence at %s.\n\n"+
			"Audio level dropped below the %.0f dB threshold.\n"+
			"Current level: Left %.1f dB / Right %.1f dB\n\n"+
			"Silence is ongoing. Please check the audio source.",
		cfg.StationName, util.HumanTime(), cfg.SilenceThreshold, levelL, levelR,
	)
	return sendWhatsAppMessage(context.Background(), whatsAppCfg, body)
}

func (c *WhatsAppChannel) SendSilenceEnd(cfg *config.Snapshot, durationMS int64, levelL, levelR float64) error {
	whatsAppCfg := BuildWhatsAppConfig(cfg)
	body := fmt.Sprintf(
		"[OK] Audio Restored - %s\n\n"+
			"Audio was restored at %s.\n\n"+
			"The silence lasted %s.\n"+
			"Level: Left %.1f dB / Right %.1f dB (threshold: %.1f dB)",
		cfg.StationName, util.HumanTime(), util.FormatDuration(durationMS), levelL, levelR, cfg.SilenceThreshold,
	)
	return sendWhatsAppMessage(context.Background(), whatsAppCfg, body)
}

func (c *WhatsAppChannel) SendAudioDump(
	cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
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
	return sendWhatsAppMessage(context.Background(), whatsAppCfg, body)
}

func (c *WhatsAppChannel) SendUploadAbandoned(cfg *config.Snapshot, params UploadAbandonedData) error {
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
	return sendWhatsAppMessage(context.Background(), whatsAppCfg, body)
}

// SendWhatsAppTest sends a test WhatsApp notification.
func SendWhatsAppTest(cfg *WhatsAppConfig, stationName string) error {
	body := fmt.Sprintf(
		"[TEST] %s\n\n"+
			"Test WhatsApp notification from the audio encoder.\n\n"+
			"Time: %s",
		stationName,
		util.HumanTime(),
	)
	return sendWhatsAppMessage(context.Background(), cfg, body)
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

func validateWhatsAppConfig(cfg *WhatsAppConfig) error {
	if cfg == nil {
		return fmt.Errorf("configuration is required")
	}
	if strings.TrimSpace(cfg.PhoneNumberID) == "" {
		return fmt.Errorf("phone number ID is required")
	}
	if strings.TrimSpace(cfg.AccessToken) == "" {
		return fmt.Errorf("access token is required")
	}

	recipients := parseWhatsAppRecipients(cfg.Recipients)
	if len(recipients) == 0 {
		return fmt.Errorf("recipients are required")
	}
	for _, recipient := range recipients {
		if !validWhatsAppRecipient(recipient) {
			return fmt.Errorf("recipient %q is invalid", recipient)
		}
	}

	return nil
}

func sendWhatsAppMessage(ctx context.Context, cfg *WhatsAppConfig, body string) error {
	if err := validateWhatsAppConfig(cfg); err != nil {
		return err
	}

	recipients := parseWhatsAppRecipients(cfg.Recipients)
	var errs []error
	for _, recipient := range recipients {
		if err := sendWhatsAppMessageToRecipient(ctx, cfg, recipient, body); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", recipient, err))
		}
	}
	return errors.Join(errs...)
}

func sendWhatsAppMessageToRecipient(ctx context.Context, cfg *WhatsAppConfig, recipient, body string) error {
	if strings.TrimSpace(cfg.TemplateName) != "" {
		return sendWhatsAppTemplateToRecipient(ctx, cfg, recipient, body)
	}
	return sendWhatsAppFreeTextToRecipient(ctx, cfg, recipient, body)
}

func sendWhatsAppFreeTextToRecipient(ctx context.Context, cfg *WhatsAppConfig, recipient, body string) error {
	payload := whatsappTextRequest{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               recipient,
		Type:             "text",
		Text: whatsappText{
			PreviewURL: false,
			Body:       truncateWhatsAppText(body),
		},
	}

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

func sendWhatsAppTemplateToRecipient(ctx context.Context, cfg *WhatsAppConfig, recipient, body string) error {
	language := strings.TrimSpace(cfg.TemplateLanguage)
	if language == "" {
		language = defaultWhatsAppTemplateLanguage
	}

	payload := whatsappTemplateRequest{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               recipient,
		Type:             "template",
		Template: whatsappTemplate{
			Name: strings.TrimSpace(cfg.TemplateName),
			Language: whatsappTemplateLanguage{
				Code: language,
			},
			Components: []whatsappTemplateComponent{
				{
					Type: "body",
					Parameters: []whatsappTemplateParameter{
						{
							Type: "text",
							Text: truncateWhatsAppText(body),
						},
					},
				},
			},
		},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return util.WrapError("marshal WhatsApp template payload", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		whatsappMessagesURL(cfg.PhoneNumberID),
		bytes.NewReader(jsonData),
	)
	if err != nil {
		return util.WrapError("create WhatsApp template request", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.AccessToken))
	req.Header.Set("Content-Type", "application/json")

	resp, err := whatsappClient.Do(req)
	if err != nil {
		return util.WrapError("send WhatsApp template request", err)
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyText := strings.TrimSpace(string(body))

	var apiErr whatsappErrorResponse
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
		if apiErr.Error.Code != 0 {
			return fmt.Errorf("WhatsApp API returned status %d: %s (code %d)",
				resp.StatusCode, apiErr.Error.Message, apiErr.Error.Code)
		}
		return fmt.Errorf("WhatsApp API returned status %d: %s", resp.StatusCode, apiErr.Error.Message)
	}

	if bodyText == "" {
		return fmt.Errorf("WhatsApp API returned status %d", resp.StatusCode)
	}
	return fmt.Errorf("WhatsApp API returned status %d: %s", resp.StatusCode, bodyText)
}

func parseWhatsAppRecipients(recipients string) []string {
	var result []string
	for recipient := range strings.SplitSeq(recipients, ",") {
		if normalized := normalizeWhatsAppRecipient(recipient); normalized != "" {
			result = append(result, normalized)
		}
	}
	return result
}

func normalizeWhatsAppRecipient(recipient string) string {
	trimmed := strings.TrimSpace(recipient)
	trimmed = strings.TrimPrefix(trimmed, "+")
	replacer := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
	return replacer.Replace(trimmed)
}

func validWhatsAppRecipient(recipient string) bool {
	normalized := normalizeWhatsAppRecipient(recipient)
	if len(normalized) < 8 || len(normalized) > 15 {
		return false
	}
	for _, r := range normalized {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func truncateWhatsAppText(body string) string {
	runes := []rune(body)
	if len(runes) <= maxWhatsAppTextRunes {
		return body
	}
	return string(runes[:maxWhatsAppTextRunes-3]) + "..."
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
