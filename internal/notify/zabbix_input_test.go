package notify

import (
	"strings"
	"testing"
)

func TestFormatZabbixInputValues(t *testing.T) {
	t.Parallel()
	// Names chosen to collide with the template's substring triggers.
	data := InputData{Input: "dante://SILENCE-RECOVERY/01/02", Error: "TEST device gone", DurationMs: 42000}
	lost := formatZabbixInputLostValue(data)
	if lost != "event=INPUT_LOST" {
		t.Fatalf("lost value = %q, want %q", lost, "event=INPUT_LOST")
	}
	restored := formatZabbixInputRestoredValue(data)
	if want := "event=INPUT_RESTORED duration_ms=42000"; restored != want {
		t.Fatalf("restored value = %q, want %q", restored, want)
	}
	// The template's silence, recovery and test triggers match on these substrings.
	for _, value := range []string{lost, restored} {
		for _, marker := range []string{"SILENCE", "RECOVERY", "TEST"} {
			if strings.Contains(value, marker) {
				t.Fatalf("value %q contains %q and would fire a silence-item trigger", value, marker)
			}
		}
	}
}
