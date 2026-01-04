package notify

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

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
func sendZabbixPayload(server string, port int, payload zabbixRequest) error {
	addr := net.JoinHostPort(server, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, zabbixTimeout)
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
func sendZabbixEvent(server string, port int, host, key, value string) error {
	if server == "" || host == "" || key == "" {
		return nil
	}
	req := zabbixRequest{
		Request: "sender data",
		Data:    []zabbixItem{{Host: host, Key: key, Value: value}},
	}
	return sendZabbixPayload(server, port, req)
}

// SendSilenceZabbix sends a silence alert to Zabbix.
func SendSilenceZabbix(server string, port int, host, key string, levelL, levelR, threshold float64) error {
	return sendZabbixEvent(server, port, host, key,
		fmt.Sprintf("event=SILENCE level_l=%.1f level_r=%.1f threshold=%.1f", levelL, levelR, threshold))
}

// SendRecoveryZabbix sends a recovery message to Zabbix.
func SendRecoveryZabbix(server string, port int, host, key string, durationMs int64, levelL, levelR, threshold float64) error {
	return sendZabbixEvent(server, port, host, key,
		fmt.Sprintf("event=RECOVERY duration_ms=%d level_l=%.1f level_r=%.1f threshold=%.1f", durationMs, levelL, levelR, threshold))
}

// SendTestZabbix sends a test message to verify Zabbix config.
func SendTestZabbix(server string, port int, host, key string) error {
	return sendZabbixEvent(server, port, host, key, "event=TEST source=zwfm-encoder")
}
