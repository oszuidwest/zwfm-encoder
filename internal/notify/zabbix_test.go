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
	"strconv"
	"strings"
	"testing"
	"time"

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

			capture := receiveZabbixCapture(t, captures)
			if capture.err != nil {
				t.Fatalf("fake Zabbix trapper: %v", capture.err)
			}
			if !bytes.Equal(capture.header[:5], wantZabbixMagic[:]) {
				t.Fatalf("header prefix = %q, want %q", capture.header[:5], wantZabbixMagic)
			}
			if got := binary.LittleEndian.Uint64(capture.header[5:]); got != uint64(len(capture.body)) {
				t.Fatalf("header body length = %d, want %d", got, len(capture.body))
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

func TestSendZabbixPayloadRejectsServerErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		reply     []byte
		wantError string
	}{
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

			server, port, captures := startFakeZabbixTrapper(t, tt.reply)
			err := sendZabbixPayload(context.Background(), server, port, zabbixRequest{
				Request: "sender data",
				Data: []zabbixItem{{
					Host:  "encoder-host",
					Key:   "encoder.silence",
					Value: "event=SILENCE",
				}},
			})
			if err == nil {
				t.Fatal("sendZabbixPayload() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("sendZabbixPayload() error = %q, want it to contain %q", err, tt.wantError)
			}
			if capture := receiveZabbixCapture(t, captures); capture.err != nil {
				t.Fatalf("fake Zabbix trapper: %v", capture.err)
			}
		})
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

// assertSubscribesOnlyToEnabledEvents enables one event at a time and checks
// that exactly the matching Subscribes* method reports true, and only when the
// channel itself is configured.
func assertSubscribesOnlyToEnabledEvents(
	t *testing.T,
	channel AlertChannel,
	supportsAudioDump bool,
	snapshot func(events types.EventSubscriptions, configured bool) *config.Snapshot,
) {
	t.Helper()

	subscribers := map[string]func(*config.Snapshot) bool{
		"silence start":           channel.SubscribesSilenceStart,
		"silence end":             channel.SubscribesSilenceEnd,
		"channel imbalance start": channel.SubscribesChannelImbalanceStart,
		"channel imbalance end":   channel.SubscribesChannelImbalanceEnd,
		"audio dump":              channel.SubscribesAudioDump,
	}
	events := map[string]types.EventSubscriptions{
		"silence start":           {SilenceStart: true},
		"silence end":             {SilenceEnd: true},
		"channel imbalance start": {ChannelImbalanceStart: true},
		"channel imbalance end":   {ChannelImbalanceEnd: true},
		"audio dump":              {AudioDump: true},
	}
	for enabled, subscriptions := range events {
		for name, subscribes := range subscribers {
			want := name == enabled && (name != "audio dump" || supportsAudioDump)
			if got := subscribes(snapshot(subscriptions, true)); got != want {
				t.Errorf("with only %q enabled, %s subscription = %t, want %t", enabled, name, got, want)
			}
		}
		if subscribers[enabled](snapshot(subscriptions, false)) {
			t.Errorf("%s subscription = true for an unconfigured channel", enabled)
		}
	}
}

func TestZabbixChannelUnconfiguredSendsNothing(t *testing.T) {
	t.Parallel()

	channel := &ZabbixChannel{}
	cfg := &config.Snapshot{}
	tests := []struct {
		name string
		send func() error
	}{
		{name: "silence start", send: func() error {
			return channel.SendSilenceStart(context.Background(), cfg, -48, -52)
		}},
		{name: "silence end", send: func() error {
			return channel.SendSilenceEnd(context.Background(), cfg, 1000, -12, -14)
		}},
		{name: "channel imbalance start", send: func() error {
			return channel.SendChannelImbalanceStart(context.Background(), cfg, ChannelImbalanceData{})
		}},
		{name: "channel imbalance end", send: func() error {
			return channel.SendChannelImbalanceEnd(context.Background(), cfg, ChannelImbalanceData{})
		}},
		{name: "upload abandoned", send: func() error {
			return channel.SendUploadAbandoned(context.Background(), cfg, UploadAbandonedData{})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.send(); err != nil {
				t.Fatalf("unconfigured send returned error: %v", err)
			}
		})
	}
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

	host, portString, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split fake Zabbix address: %v", err)
	}
	parsedPort, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatalf("parse fake Zabbix port: %v", err)
	}

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

	return host, parsedPort, captureCh
}

func receiveZabbixCapture(t *testing.T, captures <-chan zabbixCapture) zabbixCapture {
	t.Helper()

	select {
	case capture := <-captures:
		return capture
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fake Zabbix trapper")
		return zabbixCapture{}
	}
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
