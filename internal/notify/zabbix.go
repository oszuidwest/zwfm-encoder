package notify

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// Zabbix protocol constants.
const (
	zabbixTimeout    = 5 * time.Second
	zabbixHeaderSize = 13        // "ZBXD\x01" (5) + uint64 length (8)
	maxReplySize     = 64 * 1024 // 64KB max reply to prevent memory exhaustion
)

// zabbixMagic is the protocol header prefix.
var zabbixMagic = [5]byte{'Z', 'B', 'X', 'D', 0x01}

// Zabbix protocol types.
type zabbixRequest struct {
	Request string       `json:"request"`
	Data    []zabbixItem `json:"data"`
}

type zabbixItem struct {
	Host  string `json:"host"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

type zabbixResponse struct {
	Response string `json:"response"`
	Info     string `json:"info"`
}

// sendZabbixPayload sends a payload to the Zabbix server.
func sendZabbixPayload(ctx context.Context, server string, port int, payload zabbixRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	addr := net.JoinHostPort(server, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: zabbixTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return util.WrapError("connect to zabbix", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(zabbixTimeout)); err != nil {
		return util.WrapError("set deadline", err)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return util.WrapError("marshal zabbix payload", err)
	}

	// Build header: "ZBXD\x01" + 8-byte little endian length
	header := make([]byte, zabbixHeaderSize)
	copy(header[0:5], zabbixMagic[:])
	binary.LittleEndian.PutUint64(header[5:], uint64(len(data)))

	if _, err := conn.Write(header); err != nil {
		return util.WrapError("write zabbix header", err)
	}
	if _, err := conn.Write(data); err != nil {
		return util.WrapError("write zabbix payload", err)
	}

	// Read reply header
	replyHeader := make([]byte, zabbixHeaderSize)
	if _, err := io.ReadFull(conn, replyHeader); err != nil {
		return util.WrapError("read zabbix reply header", err)
	}
	if !bytes.Equal(replyHeader[0:5], zabbixMagic[:]) {
		return fmt.Errorf("invalid zabbix reply header")
	}

	replyLen := binary.LittleEndian.Uint64(replyHeader[5:zabbixHeaderSize])
	if replyLen == 0 {
		return fmt.Errorf("empty zabbix reply")
	}
	if replyLen > maxReplySize {
		return fmt.Errorf("zabbix reply too large: %d bytes (max %d)", replyLen, maxReplySize)
	}

	// Read reply body
	reply := make([]byte, replyLen)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return util.WrapError("read zabbix reply body", err)
	}

	var resp zabbixResponse
	if err := json.Unmarshal(reply, &resp); err != nil {
		return util.WrapError("parse zabbix reply", err)
	}

	// Check for explicit failure response
	if resp.Response == "failed" {
		return fmt.Errorf("zabbix rejected data: %s", resp.Info)
	}

	// Check for no items processed (host/key not found in Zabbix)
	if strings.Contains(resp.Info, "processed: 0;") && strings.Contains(resp.Info, "failed: 0;") {
		return fmt.Errorf("zabbix processed no items (check host/key config)")
	}

	return nil
}

// sendZabbixEvent sends an event to Zabbix with the given value string.
func sendZabbixEvent(ctx context.Context, server string, port int, host, key, value string) error {
	if server == "" || host == "" || key == "" {
		return nil
	}
	req := zabbixRequest{
		Request: "sender data",
		Data:    []zabbixItem{{Host: host, Key: key, Value: value}},
	}
	return sendZabbixPayload(ctx, server, port, req)
}

// sendUploadAbandonedZabbix sends an upload abandonment event to Zabbix.
func sendUploadAbandonedZabbix(
	ctx context.Context, server string, port int, host, key string, p UploadAbandonedData,
) error {
	return sendZabbixEvent(ctx, server, port, host, key, fmt.Sprintf(
		"event=UPLOAD_ABANDONED recorder=%q file=%q retries=%d error=%q",
		p.RecorderName, p.Filename, p.RetryCount, p.LastError,
	))
}

// sendZabbixSilence sends a silence alert to Zabbix.
func sendZabbixSilence(ctx context.Context, server string, port int, host, key string, e silenceEventData) error {
	return sendZabbixEvent(ctx, server, port, host, key,
		fmt.Sprintf("event=SILENCE level_l=%.1f level_r=%.1f threshold=%.1f",
			e.LevelL, e.LevelR, e.Threshold))
}

// sendZabbixRecovery sends a recovery message to Zabbix.
func sendZabbixRecovery(ctx context.Context, server string, port int, host, key string, e silenceEventData) error {
	return sendZabbixEvent(ctx, server, port, host, key,
		fmt.Sprintf("event=RECOVERY duration_ms=%d level_l=%.1f level_r=%.1f threshold=%.1f",
			e.DurationMs, e.LevelL, e.LevelR, e.Threshold))
}

// sendZabbixChannelImbalanceStart sends a channel imbalance alert to Zabbix.
func sendZabbixChannelImbalanceStart(
	ctx context.Context, server string, port int, host, key string, e ChannelImbalanceData,
) error {
	return sendZabbixEvent(ctx, server, port, host, key, formatZabbixChannelImbalanceStartValue(e))
}

// sendZabbixChannelImbalanceEnd sends a channel balance recovery message to Zabbix.
func sendZabbixChannelImbalanceEnd(
	ctx context.Context, server string, port int, host, key string, e ChannelImbalanceData,
) error {
	return sendZabbixEvent(ctx, server, port, host, key, formatZabbixChannelImbalanceEndValue(e))
}

func formatZabbixChannelImbalanceStartValue(e ChannelImbalanceData) string {
	return fmt.Sprintf(
		"event=CHANNEL_IMBALANCE level_l=%.1f level_r=%.1f balance_db=%.1f imbalance_db=%.1f threshold=%.1f",
		e.LevelL,
		e.LevelR,
		e.BalanceDB,
		e.ImbalanceDB,
		e.ThresholdDB,
	)
}

func formatZabbixChannelImbalanceEndValue(e ChannelImbalanceData) string {
	return fmt.Sprintf(
		"event=CHANNEL_BALANCED duration_ms=%d level_l=%.1f level_r=%.1f balance_db=%.1f imbalance_db=%.1f threshold=%.1f",
		e.DurationMs,
		e.LevelL,
		e.LevelR,
		e.BalanceDB,
		e.ImbalanceDB,
		e.ThresholdDB,
	)
}

// SendZabbixTest sends a test message to verify Zabbix config.
func SendZabbixTest(server string, port int, host, key string) error {
	if issues := types.ValidateZabbixTarget(server, port, host, key); len(issues) > 0 {
		return fmt.Errorf("zabbix not fully configured (server, host, key, and a valid port 1-65535 are required)")
	}
	return sendZabbixEvent(context.Background(), server, port, host, key, "event=TEST source=zwfm-encoder")
}

// ZabbixChannel implements AlertChannel for Zabbix sender delivery.
type ZabbixChannel struct{}

// Name returns the channel identifier used in logs.
func (c *ZabbixChannel) Name() string { return "zabbix" }

// IsConfiguredForSilence reports whether the channel participates in silence flows.
func (c *ZabbixChannel) IsConfiguredForSilence(cfg *config.Snapshot) bool {
	return cfg.HasZabbixSilence()
}

// IsConfiguredForImbalance reports whether the channel participates in channel imbalance flows.
func (c *ZabbixChannel) IsConfiguredForImbalance(cfg *config.Snapshot) bool {
	return cfg.HasZabbixImbalance()
}

// IsConfiguredForUpload reports whether the channel participates in upload-abandonment flows.
func (c *ZabbixChannel) IsConfiguredForUpload(cfg *config.Snapshot) bool {
	return cfg.HasZabbixUpload()
}

// SubscribesSilenceStart reports whether silence-start events should be sent.
func (c *ZabbixChannel) SubscribesSilenceStart(cfg *config.Snapshot) bool {
	return cfg.HasZabbixSilence() && cfg.ZabbixEvents.SilenceStart
}

// SubscribesSilenceEnd reports whether silence-end events should be sent.
func (c *ZabbixChannel) SubscribesSilenceEnd(cfg *config.Snapshot) bool {
	return cfg.HasZabbixSilence() && cfg.ZabbixEvents.SilenceEnd
}

// SubscribesChannelImbalanceStart reports whether imbalance-start events should be sent.
func (c *ZabbixChannel) SubscribesChannelImbalanceStart(cfg *config.Snapshot) bool {
	return cfg.HasZabbixImbalance() && cfg.ZabbixEvents.ChannelImbalanceStart
}

// SubscribesChannelImbalanceEnd reports whether imbalance-end events should be sent.
func (c *ZabbixChannel) SubscribesChannelImbalanceEnd(cfg *config.Snapshot) bool {
	return cfg.HasZabbixImbalance() && cfg.ZabbixEvents.ChannelImbalanceEnd
}

// SubscribesAudioDump always reports false because Zabbix cannot carry file attachments.
func (c *ZabbixChannel) SubscribesAudioDump(_ *config.Snapshot) bool { return false }

func (c *ZabbixChannel) SendSilenceStart(ctx context.Context, cfg *config.Snapshot, levelL, levelR float64) error {
	return sendZabbixSilence(ctx, cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixSilenceKey, silenceEventData{
		LevelL:    levelL,
		LevelR:    levelR,
		Threshold: cfg.SilenceThreshold,
	})
}

func (c *ZabbixChannel) SendSilenceEnd(
	ctx context.Context, cfg *config.Snapshot, durationMS int64, levelL, levelR float64,
) error {
	return sendZabbixRecovery(ctx, cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixSilenceKey, silenceEventData{
		DurationMs: durationMS,
		LevelL:     levelL,
		LevelR:     levelR,
		Threshold:  cfg.SilenceThreshold,
	})
}

func (c *ZabbixChannel) SendChannelImbalanceStart(
	ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData,
) error {
	return sendZabbixChannelImbalanceStart(
		ctx,
		cfg.ZabbixServer,
		cfg.ZabbixPort,
		cfg.ZabbixHost,
		cfg.ZabbixImbalanceKey,
		data,
	)
}

func (c *ZabbixChannel) SendChannelImbalanceEnd(
	ctx context.Context, cfg *config.Snapshot, data ChannelImbalanceData,
) error {
	return sendZabbixChannelImbalanceEnd(
		ctx,
		cfg.ZabbixServer,
		cfg.ZabbixPort,
		cfg.ZabbixHost,
		cfg.ZabbixImbalanceKey,
		data,
	)
}

func (c *ZabbixChannel) SendAudioDump(
	_ context.Context, _ *config.Snapshot, _ int64, _, _ float64, _ *silencedump.EncodeResult,
) error {
	return fmt.Errorf("zabbix channel does not support audio dump delivery")
}

func (c *ZabbixChannel) SendUploadAbandoned(ctx context.Context, cfg *config.Snapshot, params UploadAbandonedData) error {
	return sendUploadAbandonedZabbix(ctx, cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixUploadKey, params)
}
