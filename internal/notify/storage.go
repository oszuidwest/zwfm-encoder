package notify

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func recordingStorageMessage(paths []types.RecordingStorageStatus) (event, message string) {
	event = string(eventlog.RecordingStorageRestored)
	if slices.ContainsFunc(paths, func(p types.RecordingStorageStatus) bool { return p.Unhealthy() }) {
		event = string(eventlog.RecordingStorageLow)
	}
	lines := make([]string, 0, len(paths))
	for _, p := range paths {
		line := fmt.Sprintf("%s: %d bytes available; warning below %d bytes", p.Path, p.AvailableBytes, p.ThresholdBytes)
		if p.Low { // A low path can be above the threshold but not yet recovered.
			line += fmt.Sprintf("; recovers at %d bytes", p.RecoveryBytes)
		}
		if p.Error != "" {
			line += "; check failed: " + p.Error
		}
		lines = append(lines, line)
	}
	return event, strings.Join(lines, "\n")
}

// SendRecordingStorage posts the complete path state, including zero free bytes.
func (c *WebhookChannel) SendRecordingStorage(
	ctx context.Context, cfg *config.Snapshot, paths []types.RecordingStorageStatus,
) error {
	event, message := recordingStorageMessage(paths)
	return sendWebhook(ctx, cfg.WebhookURL, &WebhookPayload{
		Event: event, Message: message, Timestamp: timestampUTC(), StoragePaths: paths,
	})
}

func recordingStorageEmail(station string, paths []types.RecordingStorageStatus) (subject, body string) {
	event, message := recordingStorageMessage(paths)
	subject = "[OK] Recording Storage Restored - " + station
	if event == string(eventlog.RecordingStorageLow) {
		subject = "[ALERT] Recording Storage Low or Unavailable - " + station
	}
	return subject, message + "\n\nNo recordings have been automatically evicted. Check disk space and S3 uploads."
}

// SendRecordingStorage emails a warning or aggregate recovery using the existing mail configuration.
func (c *EmailChannel) SendRecordingStorage(
	ctx context.Context, cfg *config.Snapshot, paths []types.RecordingStorageStatus,
) error {
	return c.sendEvent(cfg, func(graphCfg *GraphConfig, client *GraphClient) error {
		subject, body := recordingStorageEmail(cfg.StationName, paths)
		return sendEmailWithClient(ctx, graphCfg, client, subject, body)
	})
}

// SendRecordingStorage uses a separate item derived from the upload key so disk
// recoveries cannot clear upload-abandonment alerts. Only aggregate state is sent:
// recovery of one path must not hide another path that is still low.
func (c *ZabbixChannel) SendRecordingStorage(
	ctx context.Context, cfg *config.Snapshot, paths []types.RecordingStorageStatus,
) error {
	if !c.IsConfiguredForUpload(cfg) {
		return nil
	}
	event, _ := recordingStorageMessage(paths)
	return sendZabbixEvent(ctx, cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost,
		storageItemKey(cfg.ZabbixUploadKey), "event="+strings.ToUpper(event))
}

// storageItemKey derives the storage trapper key from the upload key, before
// any parameters: upload.alert[studio] becomes upload.alert.storage[studio].
func storageItemKey(uploadKey string) string {
	if i := strings.IndexByte(uploadKey, '['); i >= 0 {
		return uploadKey[:i] + ".storage" + uploadKey[i:]
	}
	return uploadKey + ".storage"
}
