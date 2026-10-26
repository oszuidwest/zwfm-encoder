package recording

import (
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func localOnDemandRecorder(t *testing.T, id string) *types.Recorder {
	t.Helper()
	return &types.Recorder{
		ID:            id,
		Name:          id,
		FilePrefix:    id,
		Enabled:       true,
		Codec:         types.CodecPCM,
		RecordingMode: types.RecordingOnDemand,
		StorageMode:   types.StorageLocal,
		LocalPath:     t.TempDir(),
	}
}
