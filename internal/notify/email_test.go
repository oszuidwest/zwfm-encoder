package notify

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oszuidwest/zw-graphmail"
	"github.com/oszuidwest/zwfm-encoder/internal/silencedump"
)

type fakeMailer struct{ sent []*graphmail.Message }

func (f *fakeMailer) Send(_ context.Context, m *graphmail.Message) error {
	f.sent = append(f.sent, m)
	return nil
}

func TestSendEmailWithClientBuildsMessage(t *testing.T) {
	t.Parallel()
	client := &fakeMailer{}
	cfg := &GraphConfig{Recipients: "a@example.com, ,b@example.com"}
	if err := sendEmailWithClient(context.Background(), cfg, client, "subject", "body"); err != nil {
		t.Fatalf("sendEmailWithClient() error = %v", err)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(client.sent))
	}
	m := client.sent[0]
	if strings.Join(m.To, ",") != "a@example.com,b@example.com" || m.Subject != "subject" || m.Body != "body" || m.HTML {
		t.Fatalf("message = %+v", m)
	}
}

func TestSendDumpReadyEmailAttachesRecording(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{name: "recording attached", data: []byte("mp3"), want: 1},
		{name: "empty file skipped", data: nil, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "dump.mp3")
			if err := os.WriteFile(path, tt.data, 0o600); err != nil {
				t.Fatal(err)
			}
			client := &fakeMailer{}
			cfg := &GraphConfig{Recipients: "a@example.com, b@example.com"}
			data := &AudioDumpData{Result: &silencedump.EncodeResult{FilePath: path, Filename: "dump.mp3"}}
			if err := sendDumpReadyEmailWithClient(context.Background(), cfg, client, "Station", data); err != nil {
				t.Fatalf("sendDumpReadyEmailWithClient() error = %v", err)
			}
			if len(client.sent) != 1 {
				t.Fatalf("sent %d messages, want 1", len(client.sent))
			}
			m := client.sent[0]
			if strings.Join(m.To, ",") != "a@example.com,b@example.com" || m.Subject != "[DUMP] Audio Recording - Station" ||
				!strings.HasPrefix(m.Body, "Audio dump ready at") || m.HTML {
				t.Fatalf("message = %+v", m)
			}
			if len(m.Attachments) != tt.want {
				t.Fatalf("attachments = %d, want %d", len(m.Attachments), tt.want)
			}
			if tt.want == 1 {
				a := m.Attachments[0]
				if a.Name != "dump.mp3" || a.ContentType != "audio/mpeg" || !bytes.Equal(a.Data, tt.data) {
					t.Fatalf("attachment = %+v", a)
				}
			}
		})
	}
}

func TestBuildChannelImbalanceEmails(t *testing.T) {
	t.Parallel()
	startSubject, startBody := buildChannelImbalanceStartEmail(
		"ZuidWest FM",
		"27 Jun 2026 12:00:00 CEST",
		ChannelImbalanceData{
			LevelL:      -6,
			LevelR:      -30,
			BalanceDB:   24,
			ImbalanceDB: 24,
			ThresholdDB: 12,
		},
	)
	if startSubject != "[ALERT] Channel Imbalance Detected - ZuidWest FM" {
		t.Fatalf("start subject = %q", startSubject)
	}
	for _, want := range []string{
		"27 Jun 2026 12:00:00 CEST",
		"Level: Left -6.0 dB / Right -30.0 dB",
		"Balance: 24.0 dB (left louder)",
		"Imbalance: 24.0 dB",
		"Threshold: 12.0 dB",
	} {
		if !strings.Contains(startBody, want) {
			t.Fatalf("start body missing %q:\n%s", want, startBody)
		}
	}

	endSubject, endBody := buildChannelImbalanceEndEmail(
		"ZuidWest FM",
		"27 Jun 2026 12:01:00 CEST",
		ChannelImbalanceData{
			LevelL:      -8,
			LevelR:      -8,
			BalanceDB:   0,
			ImbalanceDB: 0,
			ThresholdDB: 12,
			DurationMs:  16000,
		},
	)
	if endSubject != "[OK] Channels Balanced - ZuidWest FM" {
		t.Fatalf("end subject = %q", endSubject)
	}
	for _, want := range []string{
		"27 Jun 2026 12:01:00 CEST",
		"The imbalance lasted 16s.",
		"Final level: Left -8.0 dB / Right -8.0 dB",
		"Final balance: 0.0 dB (balanced)",
		"Final imbalance: 0.0 dB",
		"Threshold: 12.0 dB",
	} {
		if !strings.Contains(endBody, want) {
			t.Fatalf("end body missing %q:\n%s", want, endBody)
		}
	}
}

func TestBuildChannelImbalanceDumpEmail(t *testing.T) {
	t.Parallel()
	balanceDB, imbalanceDB := 1.0, 1.0
	subject, body := buildDumpReadyEmail(
		"ZuidWest FM",
		"11 Aug 2026 20:19:00 CEST",
		&AudioDumpData{
			Trigger:     silencedump.TriggerChannelImbalance,
			LevelL:      -12,
			LevelR:      -13,
			BalanceDB:   &balanceDB,
			ImbalanceDB: &imbalanceDB,
			ThresholdDB: 12,
			DurationMs:  30000,
		},
	)
	if subject != "[DUMP] Audio Recording - ZuidWest FM" {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{
		"The channel imbalance lasted 30s.",
		"Final level: Left -12.0 dB / Right -13.0 dB",
		"Final balance: 1.0 dB (left louder)",
		"Final imbalance: 1.0 dB",
		"Threshold: 12.0 dB",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dump body missing %q:\n%s", want, body)
		}
	}
}

func TestChannelImbalanceDirection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{name: "left louder", in: 1, want: "left louder"},
		{name: "right louder", in: -1, want: "right louder"},
		{name: "balanced", in: 0, want: "balanced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := channelImbalanceDirection(tt.in); got != tt.want {
				t.Fatalf("channelImbalanceDirection(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildInputEmails(t *testing.T) {
	t.Parallel()
	subject, body := buildInputLostEmail("ZuidWest", "16 Oct 2026 03:00", InputData{Input: "hw:1", Error: "device disconnected"})
	if subject != "[ALERT] Audio Input Lost - ZuidWest" {
		t.Fatalf("lost subject = %q", subject)
	}
	for _, want := range []string{"16 Oct 2026 03:00", "Input: hw:1", "Error: device disconnected"} {
		if !strings.Contains(body, want) {
			t.Fatalf("lost body missing %q:\n%s", want, body)
		}
	}
	subject, body = buildInputRestoredEmail("ZuidWest", "16 Oct 2026 03:07", InputData{Input: "hw:1", DurationMs: 390000})
	if subject != "[OK] Audio Input Restored - ZuidWest" {
		t.Fatalf("restored subject = %q", subject)
	}
	if !strings.Contains(body, "Input: hw:1") || !strings.Contains(body, "unavailable for") {
		t.Fatalf("restored body = %q", body)
	}
}
