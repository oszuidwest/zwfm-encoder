//go:build linux || darwin

package recording

import "testing"

func TestStatfsSpaceExcludesReservedBlocks(t *testing.T) {
	t.Parallel()
	const bs = 4096
	for _, tt := range []struct {
		name                     string
		blocks, bfree, bavail    uint64
		wantAvailable, wantTotal uint64
	}{
		{"5% reserved", 1000, 300, 250, 250 * bs, 950 * bs},
		// Deleting every file must be able to reach the reported total.
		{"empty volume", 1000, 1000, 950, 950 * bs, 950 * bs},
		{"no reservation", 1000, 300, 300, 300 * bs, 1000 * bs},
		{"reserved space in use", 1000, 10, ^uint64(0) - 39, 0, 990 * bs},
	} {
		t.Run(tt.name, func(t *testing.T) {
			available, total, err := statfsSpace(tt.blocks, tt.bfree, tt.bavail, bs)
			if err != nil || available != tt.wantAvailable || total != tt.wantTotal {
				t.Fatalf("available=%d total=%d err=%v, want %d and %d", available, total, err, tt.wantAvailable, tt.wantTotal)
			}
		})
	}
}
