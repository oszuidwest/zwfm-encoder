package notify

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// isConfigured checks required Zabbix params are present.
func isConfiguredZabbix(server string, port int, host, key string) bool {
	return server != "" && host != "" && key != ""
}

// sendZabbixSenderPayload sends a single trapper item via the zabbix_sender protocol.
func sendZabbixSenderPayload(server string, port int, payload interface{}, timeoutMs int) error {
	addr := net.JoinHostPort(server, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, time.Duration(timeoutMs)*time.Millisecond)
	if err != nil {
		return util.WrapError("connect to zabbix", err)
	}
	defer func() { _ = conn.Close() }()
	// set overall deadline
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	if err := conn.SetDeadline(deadline); err != nil {
		return util.WrapError("set deadline", err)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return util.WrapError("marshal zabbix payload", err)
	}

	// Build header: "ZBXD\1" + 8-byte little endian length
	head := make([]byte, 5+8)
	copy(head[0:5], []byte{'Z', 'B', 'X', 'D', 1})
	binary.LittleEndian.PutUint64(head[5:], uint64(len(data)))

	// Write header + payload
	if _, err := conn.Write(head); err != nil {
		return util.WrapError("write zabbix header", err)
	}
	if _, err := conn.Write(data); err != nil {
		return util.WrapError("write zabbix payload", err)
	}

	// Read reply header
	replyHdr := make([]byte, 13)
	if _, err := conn.Read(replyHdr); err != nil {
		return util.WrapError("read zabbix reply header", err)
	}
	if string(replyHdr[0:5]) != "ZBXD\x01" {
		return fmt.Errorf("invalid zabbix reply header")
	}
	replyLen := binary.LittleEndian.Uint64(replyHdr[5:13])
	if replyLen == 0 {
		return fmt.Errorf("empty zabbix reply")
	}

	reply := make([]byte, replyLen)
	if _, err := conn.Read(reply); err != nil {
		return util.WrapError("read zabbix reply body", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(reply, &resp); err != nil {
		return util.WrapError("parse zabbix reply", err)
	}
	// Optionally inspect resp.info for success
	if info, ok := resp["info"].(string); ok {
		// Example: "processed: 1; failed: 0; total: 1; spent: 0.000000 seconds"
		if info == "processed: 0; failed: 0; total: 0; spent: 0.000000 seconds" {
			return fmt.Errorf("zabbix processed no items")
		}
	}

	return nil
}

// SendSilenceZabbix sends a silence alert to Zabbix using the provided server/host/key.
func SendSilenceZabbix(server string, port int, host, key string, levelL, levelR, threshold float64, timeoutMs int) error {
	if !isConfiguredZabbix(server, port, host, key) {
		return nil
	}
	value := fmt.Sprintf("SILENCE L:%.1f R:%.1f threshold:%.1f", levelL, levelR, threshold)
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
	return sendZabbixSenderPayload(server, port, payload, timeoutMs)
}

// SendRecoveryZabbix sends a recovery message with duration and levels.
func SendRecoveryZabbix(server string, port int, host, key string, durationMs int64, levelL, levelR, threshold float64, timeoutMs int) error {
	if !isConfiguredZabbix(server, port, host, key) {
		return nil
	}
	value := fmt.Sprintf("RECOVERY duration_ms:%d L:%.1f R:%.1f threshold:%.1f", durationMs, levelL, levelR, threshold)
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
	return sendZabbixSenderPayload(server, port, payload, timeoutMs)
}

// SendTestZabbix sends a test message to verify Zabbix config.
func SendTestZabbix(server string, port int, host, key string, timeoutMs int) error {
	if !isConfiguredZabbix(server, port, host, key) {
		return nil
	}
	value := "TEST from zwfm-encoder"
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
	return sendZabbixSenderPayload(server, port, payload, timeoutMs)
}
