package encoder

import (
	"strings"
	"testing"
	"testing/synctest"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestStopJoinsAnAlreadyApprovedConsumerStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newSourceLifecycleTestEncoder(t, "exit")
		t.Cleanup(e.alertOrchestrator.Close)
		e.state, e.sourceRunID = types.StateRunning, 1
		e.stopChan = make(chan struct{})
		e.sourceStdout = strings.NewReader("")
		if !e.beginConsumerStart(1) {
			t.Fatal("active source could not claim consumer startup")
		}
		stopped := make(chan error, 1)
		go func() { stopped <- e.Stop() }()
		synctest.Wait()
		select {
		case err := <-stopped:
			e.consumerStarts.Done()
			t.Fatalf("Stop returned before approved startup completed: %v", err)
		default:
		}
		if e.State() != types.StateStopping {
			t.Fatal("Stop did not publish stopping while awaiting startup")
		}
		// Resume the manager call that was approved before Stop changed state.
		if err := e.recordingManager.Start(); err != nil {
			e.consumerStarts.Done()
			t.Fatal(err)
		}
		e.consumerStarts.Done()
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
		assertManagersStopped(t, e, "approved startup escaped final teardown")
		for _, runID := range []uint64{0, 1} {
			if e.beginConsumerStart(runID) {
				e.consumerStarts.Done()
				t.Fatalf("consumer start for run %d was approved after Stop", runID)
			}
		}
	})
}
