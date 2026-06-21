package eventlog

import (
	"maps"
	"slices"
	"testing"
)

type expectedClassification struct {
	category Category
	severity Severity
	reason   Reason
}

func TestEventTypeClassification(t *testing.T) {
	t.Parallel()

	expected := map[EventType]expectedClassification{
		StreamStarted:         {category: CategoryStream, severity: SeverityInfo, reason: ReasonLifecycle},
		StreamStable:          {category: CategoryStream, severity: SeveritySuccess, reason: ReasonRecovery},
		StreamError:           {category: CategoryStream, severity: SeverityError, reason: ReasonProblem},
		StreamRetry:           {category: CategoryStream, severity: SeverityWarning, reason: ReasonProblem},
		StreamStopped:         {category: CategoryStream, severity: SeverityInfo, reason: ReasonLifecycle},
		SilenceStart:          {category: CategoryAudio, severity: SeverityWarning, reason: ReasonProblem},
		SilenceEnd:            {category: CategoryAudio, severity: SeveritySuccess, reason: ReasonRecovery},
		AudioDumpReady:        {category: CategoryAudio, severity: SeverityInfo, reason: ReasonLifecycle},
		ChannelImbalanceStart: {category: CategoryAudio, severity: SeverityWarning, reason: ReasonProblem},
		ChannelImbalanceEnd:   {category: CategoryAudio, severity: SeveritySuccess, reason: ReasonRecovery},
		RecorderStarted:       {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonLifecycle},
		RecorderStopped:       {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonLifecycle},
		RecorderError:         {category: CategoryRecorder, severity: SeverityError, reason: ReasonProblem},
		RecorderFile:          {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonRoutine},
		UploadQueued:          {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonRoutine},
		UploadCompleted:       {category: CategoryRecorder, severity: SeveritySuccess, reason: ReasonRoutine},
		UploadFailed:          {category: CategoryRecorder, severity: SeverityError, reason: ReasonProblem},
		UploadRetry:           {category: CategoryRecorder, severity: SeverityWarning, reason: ReasonProblem},
		UploadAbandoned:       {category: CategoryRecorder, severity: SeverityError, reason: ReasonProblem},
		CleanupCompleted:      {category: CategoryRecorder, severity: SeveritySuccess, reason: ReasonRoutine},
	}

	gotTypes := slices.Sorted(maps.Keys(eventClassifications))
	wantTypes := slices.Sorted(maps.Keys(expected))
	if !slices.Equal(gotTypes, wantTypes) {
		t.Fatalf("classified event types = %v, want %v", gotTypes, wantTypes)
	}

	for eventType, want := range expected {
		t.Run(string(eventType), func(t *testing.T) {
			t.Parallel()
			if got := eventType.Category(); got != want.category {
				t.Fatalf("Category() = %q, want %q", got, want.category)
			}
			if got := eventType.Severity(); got != want.severity {
				t.Fatalf("Severity() = %q, want %q", got, want.severity)
			}
			if got := eventType.Reason(); got != want.reason {
				t.Fatalf("Reason() = %q, want %q", got, want.reason)
			}
			if got := eventType.Reason() == ReasonRoutine; got != (want.reason == ReasonRoutine) {
				t.Fatalf("Reason() == ReasonRoutine = %v, want %v", got, want.reason == ReasonRoutine)
			}
		})
	}
}

func TestUnknownEventTypeClassification(t *testing.T) {
	t.Parallel()

	eventType := EventType("does_not_exist")
	if got := eventType.Category(); got != CategoryUnknown {
		t.Fatalf("Category() = %q, want %q", got, CategoryUnknown)
	}
	if got := eventType.Severity(); got != SeverityUnknown {
		t.Fatalf("Severity() = %q, want %q", got, SeverityUnknown)
	}
	if got := eventType.Reason(); got != ReasonUnknown {
		t.Fatalf("Reason() = %q, want %q", got, ReasonUnknown)
	}
	if eventType.Reason() == ReasonRoutine {
		t.Fatal("Reason() == ReasonRoutine, want false")
	}
}
