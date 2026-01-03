package notify

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// zabbixTimeout is the connection and read/write timeout for Zabbix operations.
const zabbixTimeout = 5 * time.Second

// isConfigured checks required Zabbix params are present.
func isConfiguredZabbix(server string, port int, host, key string) bool {
	return server != "" && host != "" && key != ""
}

// sendZabbixSenderPayload sends a single trapper item via the zabbix_sender protocol.
func sendZabbixSenderPayload(server string, port int, payload interface{}) error {
	addr := net.JoinHostPort(server, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, zabbixTimeout)
	if err != nil {
		return util.WrapError("connect to zabbix", err)
	}
	defer func() { _ = conn.Close() }()

	// Set overall deadline for all operations
	if err := conn.SetDeadline(time.Now().Add(zabbixTimeout)); err != nil {
		return util.WrapError("set deadline", err)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return util.WrapError("marshal zabbix payload", err)
	}

	// Build header: "ZBXD\1" + 8-byte little endian length
	head := make([]byte, 13)
	copy(head[0:5], []byte{'Z', 'B', 'X', 'D', 1})
	binary.LittleEndian.PutUint64(head[5:], uint64(len(data)))

	// Write header + payload
	if _, err := conn.Write(head); err != nil {
		return util.WrapError("write zabbix header", err)
	}
	if _, err := conn.Write(data); err != nil {
		return util.WrapError("write zabbix payload", err)
	}

	// Read reply header (must read exactly 13 bytes)
	replyHdr := make([]byte, 13)
	if _, err := io.ReadFull(conn, replyHdr); err != nil {
		return util.WrapError("read zabbix reply header", err)
	}
	if string(replyHdr[0:5]) != "ZBXD\x01" {
		return fmt.Errorf("invalid zabbix reply header")
	}
	replyLen := binary.LittleEndian.Uint64(replyHdr[5:13])
	if replyLen == 0 {
		return fmt.Errorf("empty zabbix reply")
	}

	// Read reply body (must read exactly replyLen bytes)
	reply := make([]byte, replyLen)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return util.WrapError("read zabbix reply body", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(reply, &resp); err != nil {
		return util.WrapError("parse zabbix reply", err)
	}

	// Check if Zabbix processed the item
	if info, ok := resp["info"].(string); ok {
		if info == "processed: 0; failed: 0; total: 0; spent: 0.000000 seconds" {
			return fmt.Errorf("zabbix processed no items")
		}
	}

	return nil
}

// SendSilenceZabbix sends a silence alert to Zabbix using the provided server/host/key.
func SendSilenceZabbix(server string, port int, host, key string, levelL, levelR, threshold float64) error {
	if !isConfiguredZabbix(server, port, host, key) {
		return nil
	}
	value := fmt.Sprintf("event=SILENCE level_l=%.1f level_r=%.1f threshold=%.1f", levelL, levelR, threshold)
	payload := map[string]interface{}{
		"request": "sender data",
		"data": []map[string]interface{}{
			{
				"host":  host,
				"key":   key,
				"value": value,
			},
		},
	}
	return sendZabbixSenderPayload(server, port, payload)
}

// SendRecoveryZabbix sends a recovery message with duration and levels.
func SendRecoveryZabbix(server string, port int, host, key string, durationMs int64, levelL, levelR, threshold float64) error {
	if !isConfiguredZabbix(server, port, host, key) {
		return nil
	}
	value := fmt.Sprintf("event=RECOVERY duration_ms=%d level_l=%.1f level_r=%.1f threshold=%.1f", durationMs, levelL, levelR, threshold)
	payload := map[string]interface{}{
		"request": "sender data",
		"data": []map[string]interface{}{
			{
				"host":  host,
				"key":   key,
				"value": value,
			},
		},
	}
	return sendZabbixSenderPayload(server, port, payload)
}

// SendTestZabbix sends a test message to verify Zabbix config.
func SendTestZabbix(server string, port int, host, key string) error {
	if !isConfiguredZabbix(server, port, host, key) {
		return nil
	}
	value := "event=TEST source=zwfm-encoder"
	payload := map[string]interface{}{
		"request": "sender data",
		"data": []map[string]interface{}{
			{
				"host":  host,
				"key":   key,
				"value": value,
			},
		},
	}
	return sendZabbixSenderPayload(server, port, payload)
}
