package notify

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

type zabbixCapture struct {
	header []byte
	body   []byte
	err    error
}

var wantZabbixMagic = [5]byte{'Z', 'B', 'X', 'D', 0x01}

func TestZabbixChannelSendPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value string
		send  func(*ZabbixChannel, *config.Snapshot) error
	}{
		{
			name: "recording storage low", key: "encoder.upload.storage", value: "event=RECORDING_STORAGE_LOW",
			send: func(ch *ZabbixChannel, cfg *config.Snapshot) error {
				return ch.SendRecordingStorage(context.Background(), cfg, []types.RecordingStorageStatus{{Path: "event=RECORDING_STORAGE_RESTORED", Low: true}, {Path: "/healthy"}})
			},
		},
		{
			name: "recording storage restored", key: "encoder.upload.storage", value: "event=RECORDING_STORAGE_RESTORED",
			send: func(ch *ZabbixChannel, cfg *config.Snapshot) error {
				return ch.SendRecordingStorage(context.Background(), cfg, []types.RecordingStorageStatus{{Path: "/healthy"}})
			},
		},
		{
			name: "recording storage unavailable", key: "encoder.upload.storage", value: "event=RECORDING_STORAGE_LOW",
			send: func(ch *ZabbixChannel, cfg *config.Snapshot) error {
				return ch.SendRecordingStorage(context.Background(), cfg, []types.RecordingStorageStatus{{Path: "/offline", Error: "disk missing"}})
			},
		},
		{
			name:  "silence start",
			key:   "encoder.silence",
			value: "event=SILENCE level_l=-48.5 level_r=-52.3 threshold=-40.0",
			send: func(channel *ZabbixChannel, cfg *config.Snapshot) error {
				return channel.SendSilenceStart(context.Background(), cfg, -48.5, -52.3)
			},
		},
		{
			name:  "silence end",
			key:   "encoder.silence",
			value: "event=RECOVERY duration_ms=65000 level_l=-12.3 level_r=-14.1 threshold=-40.0",
			send: func(channel *ZabbixChannel, cfg *config.Snapshot) error {
				return channel.SendSilenceEnd(context.Background(), cfg, 65000, -12.3, -14.1)
			},
		},
		{
			name:  "channel imbalance start",
			key:   "encoder.imbalance",
			value: "event=CHANNEL_IMBALANCE level_l=-6.2 level_r=-56.0 balance_db=49.8 imbalance_db=49.8 threshold=12.0",
			send: func(channel *ZabbixChannel, cfg *config.Snapshot) error {
				return channel.SendChannelImbalanceStart(context.Background(), cfg, ChannelImbalanceData{
					LevelL:      -6.2,
					LevelR:      -56,
					BalanceDB:   49.8,
					ImbalanceDB: 49.8,
					ThresholdDB: 12,
				})
			},
		},
		{
			name:  "channel imbalance end",
			key:   "encoder.imbalance",
			value: "event=CHANNEL_BALANCED duration_ms=180000 level_l=-8.0 level_r=-9.5 balance_db=1.5 imbalance_db=1.5 threshold=12.0",
			send: func(channel *ZabbixChannel, cfg *config.Snapshot) error {
				return channel.SendChannelImbalanceEnd(context.Background(), cfg, ChannelImbalanceData{
					LevelL:      -8,
					LevelR:      -9.5,
					BalanceDB:   1.5,
					ImbalanceDB: 1.5,
					ThresholdDB: 12,
					DurationMs:  180000,
				})
			},
		},
		{
			name:  "upload abandoned",
			key:   "encoder.upload",
			value: `event=UPLOAD_ABANDONED recorder="Archive" file="archive.mp3" retries=24 error="request timed out"`,
			send: func(channel *ZabbixChannel, cfg *config.Snapshot) error {
				return channel.SendUploadAbandoned(context.Background(), cfg, UploadAbandonedData{
					RecorderName: "Archive",
					Filename:     "archive.mp3",
					RetryCount:   24,
					LastError:    "request timed out",
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, port, captures := startFakeZabbixTrapper(t, framedZabbixReply(
				[]byte(`{"response":"success","info":"processed: 1; failed: 0; total: 1;"}`),
			))
			cfg := &config.Snapshot{
				SilenceThreshold:   -40,
				ZabbixServer:       server,
				ZabbixPort:         port,
				ZabbixHost:         "encoder-host",
				ZabbixSilenceKey:   "encoder.silence",
				ZabbixImbalanceKey: "encoder.imbalance",
				ZabbixUploadKey:    "encoder.upload",
			}

			if err := tt.send(&ZabbixChannel{}, cfg); err != nil {
				t.Fatalf("send Zabbix event: %v", err)
			}

			capture := awaitCall(t, captures, "fake Zabbix trapper")
			if capture.err != nil {
				t.Fatalf("fake Zabbix trapper: %v", capture.err)
			}
			if !bytes.Equal(capture.header[:5], wantZabbixMagic[:]) {
				t.Fatalf("header prefix = %q, want %q", capture.header[:5], wantZabbixMagic)
			}

			var gotBody map[string]any
			if err := json.Unmarshal(capture.body, &gotBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			wantBody := map[string]any{
				"request": "sender data",
				"data": []any{map[string]any{
					"host":  "encoder-host",
					"key":   tt.key,
					"value": tt.value,
				}},
			}
			if !reflect.DeepEqual(gotBody, wantBody) {
				t.Fatalf("request body = %#v, want %#v", gotBody, wantBody)
			}
		})
	}
}

func TestSendZabbixPayloadChecksReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		reply     []byte
		wantError string
	}{
		{
			name:  "unparsable info is accepted",
			reply: framedZabbixReply([]byte(`{"response":"success","info":"queued"}`)),
		},
		{
			name: "unknown host or key",
			reply: framedZabbixReply(
				[]byte(`{"response":"success","info":"processed: 0; failed: 1; total: 1; seconds spent: 0.000055"}`),
			),
			wantError: "did not accept the item",
		},
		{
			name: "no processed items",
			reply: framedZabbixReply(
				[]byte(`{"response":"success","info":"processed: 0; failed: 0; total: 0; seconds spent: 0.000010"}`),
			),
			wantError: "did not accept the item",
		},
		{
			name:      "explicit failure response",
			reply:     framedZabbixReply([]byte(`{"response":"failed","info":"host not allowed"}`)),
			wantError: "zabbix rejected data",
		},
		{
			name:      "malformed JSON",
			reply:     framedZabbixReply([]byte(`{"response":`)),
			wantError: "parse zabbix reply",
		},
		{
			name:      "invalid header",
			reply:     append([]byte("NOPE!\x02\x00\x00\x00\x00\x00\x00\x00"), []byte(`{}`)...),
			wantError: "invalid zabbix reply header",
		},
		{
			name:      "oversized reply",
			reply:     oversizedZabbixReplyHeader(),
			wantError: "zabbix reply too large",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, port, _ := startFakeZabbixTrapper(t, tt.reply)
			err := sendZabbixEvent(context.Background(), server, port, "encoder-host", "encoder.silence", "event=SILENCE")
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("sendZabbixEvent() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("sendZabbixEvent() error = %v, want it to contain %q", err, tt.wantError)
			}
		})
	}
}

// TestSendZabbixTestReportsRejectedItem covers the UI test button, which used
// to report success for an item Zabbix dropped.
func TestSendZabbixTestReportsRejectedItem(t *testing.T) {
	t.Parallel()

	server, port, captures := startFakeZabbixTrapper(t, framedZabbixReply(
		[]byte(`{"response":"success","info":"processed: 0; failed: 1; total: 1; seconds spent: 0.000055"}`),
	))
	err := SendZabbixTest(server, port, "encoder-host", "encoder.silence")
	if err == nil || !strings.Contains(err.Error(), "did not accept the item") {
		t.Fatalf("SendZabbixTest() error = %v, want a rejected item error", err)
	}

	capture := awaitCall(t, captures, "fake Zabbix trapper")
	var body struct {
		Data []zabbixItem `json:"data"`
	}
	if err := json.Unmarshal(capture.body, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	// The template's test trigger matches TEST.
	if len(body.Data) != 1 || body.Data[0].Value != "event=TEST source=zwfm-encoder" {
		t.Fatalf("test items = %+v, want one event=TEST value", body.Data)
	}
}

func TestZabbixChannelSubscribesOnlyToEnabledEvents(t *testing.T) {
	t.Parallel()

	// Zabbix cannot carry attachments, so audio dumps are never sent.
	assertSubscribesOnlyToEnabledEvents(t, &ZabbixChannel{}, false, func(events types.EventSubscriptions, configured bool) *config.Snapshot {
		cfg := &config.Snapshot{ZabbixEvents: events}
		if configured {
			cfg.ZabbixServer = "127.0.0.1"
			cfg.ZabbixHost = "encoder-host"
			cfg.ZabbixSilenceKey = "encoder.silence"
			cfg.ZabbixImbalanceKey = "encoder.imbalance"
		}
		return cfg
	})
}

func startFakeZabbixTrapper(
	t *testing.T,
	reply []byte,
) (server string, port int, captures <-chan zabbixCapture) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake Zabbix trapper: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	captureCh := make(chan zabbixCapture, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			captureCh <- zabbixCapture{err: fmt.Errorf("accept connection: %w", err)}
			return
		}
		defer func() { _ = conn.Close() }()

		header := make([]byte, zabbixHeaderSize)
		if _, err := io.ReadFull(conn, header); err != nil {
			captureCh <- zabbixCapture{err: fmt.Errorf("read request header: %w", err)}
			return
		}
		bodyLength := binary.LittleEndian.Uint64(header[5:])
		if bodyLength > maxReplySize {
			captureCh <- zabbixCapture{err: fmt.Errorf("request body too large: %d", bodyLength)}
			return
		}
		body := make([]byte, bodyLength)
		if _, err := io.ReadFull(conn, body); err != nil {
			captureCh <- zabbixCapture{err: fmt.Errorf("read request body: %w", err)}
			return
		}
		if _, err := io.Copy(conn, bytes.NewReader(reply)); err != nil {
			captureCh <- zabbixCapture{err: fmt.Errorf("write reply: %w", err)}
			return
		}
		captureCh <- zabbixCapture{header: header, body: body}
	}()

	return "127.0.0.1", listener.Addr().(*net.TCPAddr).Port, captureCh
}

func framedZabbixReply(body []byte) []byte {
	reply := make([]byte, zabbixHeaderSize+len(body))
	copy(reply, wantZabbixMagic[:])
	binary.LittleEndian.PutUint64(reply[5:], uint64(len(body)))
	copy(reply[zabbixHeaderSize:], body)
	return reply
}

func oversizedZabbixReplyHeader() []byte {
	header := make([]byte, zabbixHeaderSize)
	copy(header, wantZabbixMagic[:])
	binary.LittleEndian.PutUint64(header[5:], maxReplySize+1)
	return header
}

func TestFormatZabbixChannelImbalanceValues(t *testing.T) {
	t.Parallel()
	start := formatZabbixChannelImbalanceStartValue(ChannelImbalanceData{
		LevelL:      -6,
		LevelR:      -30,
		BalanceDB:   24,
		ImbalanceDB: 24,
		ThresholdDB: 12,
	})
	wantStart := "event=CHANNEL_IMBALANCE level_l=-6.0 level_r=-30.0 balance_db=24.0 imbalance_db=24.0 threshold=12.0"
	if start != wantStart {
		t.Fatalf("start value = %q, want %q", start, wantStart)
	}

	end := formatZabbixChannelImbalanceEndValue(ChannelImbalanceData{
		LevelL:      -8,
		LevelR:      -8,
		BalanceDB:   0,
		ImbalanceDB: 0,
		ThresholdDB: 12,
		DurationMs:  16000,
	})
	wantEnd := "event=CHANNEL_BALANCED duration_ms=16000 level_l=-8.0 level_r=-8.0 balance_db=0.0 imbalance_db=0.0 threshold=12.0"
	if end != wantEnd {
		t.Fatalf("end value = %q, want %q", end, wantEnd)
	}
}
