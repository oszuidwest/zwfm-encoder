//go:build integration

package decodermonitor

import (
	"archive/zip"
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

// Offline replay uses the operator-supplied, sanitized evidence ZIP; no network,
// production config, audio device or mail sender participates in this test.
func TestIncidentEvidenceReplay(t *testing.T) {
	path := os.Getenv("MICROMPX_EVIDENCE_ZIP")
	if path == "" {
		t.Skip("set MICROMPX_EVIDENCE_ZIP to the reviewed 2026-10-19 evidence ZIP")
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	for _, site := range []string{"boz", "oudenbosch"} {
		t.Run(site, func(t *testing.T) {
			file, err := archive.Open(site + "/journal.log")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			var records []Record
			scanner := bufio.NewScanner(file)
			starvations := 0
			for scanner.Scan() {
				line := scanner.Text()
				stamp, message, ok := strings.Cut(line, " ")
				if !ok {
					t.Fatal("unexpected journal format")
				}
				at, err := time.Parse("2006-01-02T15:04:05.999999-0700", stamp)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(message, "Buffer empty") {
					starvations++
				}
				records = append(records, Record{TimeUS: at.UnixMicro(), Message: message, Cursor: stamp, Key: stamp, BootID: "replay"})
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.Site = site
			e, state := Engine{cfg}, State{Version: 1}
			zone := time.FixedZone("CEST", 2*3600)
			start := time.Date(2026, 10, 19, 0, 0, 0, 0, zone)
			end := start.Add(10*time.Hour + 45*time.Minute)
			index, incidents, warnings, criticals, recoveries := 0, map[uint64]bool{}, 0, 0, 0
			var criticalAt, recoveryAt time.Time
			for at := start; !at.After(end); at = at.Add(10 * time.Second) {
				p := healthyProbe(at, uint64(at.Sub(start)/time.Second)*192000)
				for index < len(records) && records[index].TimeUS <= at.UnixMicro() {
					p.Records = append(p.Records, records[index])
					index++
				}
				advance(t, &e, &state, p, at)
				for _, mail := range state.Outbox {
					if mail.IncidentID != 0 {
						incidents[mail.IncidentID] = true
					}
					if strings.Contains(mail.Subject, "[WARNING]") {
						warnings++
					}
					if strings.Contains(mail.Subject, "CRITICAL]") {
						criticals++
						if criticalAt.IsZero() {
							criticalAt = at
						}
					}
					if strings.Contains(mail.Subject, "[RECOVERY]") {
						recoveries++
						recoveryAt = at
					}
				}
				state.Outbox = nil // Collect notifications in-memory; never send them.
			}
			if site == "oudenbosch" {
				if starvations != 0 || len(incidents) != 0 || recoveries != 0 {
					t.Fatalf("clean decoder alarmed: records=%d incidents=%d recoveries=%d", starvations, len(incidents), recoveries)
				}
			} else {
				if starvations != 1433 || len(incidents) != 1 || warnings != 1 || criticals == 0 || recoveries != 1 {
					t.Fatalf("unexpected replay: records=%d incidents=%d warnings=%d criticals=%d recoveries=%d", starvations, len(incidents), warnings, criticals, recoveries)
				}
				if criticalAt.Hour() != 8 || criticalAt.Minute() != 15 {
					t.Fatalf("critical at %s", criticalAt)
				}
				if recoveryAt.Hour() != 10 || recoveryAt.Minute() != 9 {
					t.Fatalf("recovery at %s", recoveryAt)
				}
			}
			t.Logf("starvation records=%d incidents=%d warning=%d critical notices including reminders=%d recovery=%d first critical=%s recovery=%s", starvations, len(incidents), warnings, criticals, recoveries, criticalAt.Format(time.RFC3339), recoveryAt.Format(time.RFC3339))
		})
	}
}
