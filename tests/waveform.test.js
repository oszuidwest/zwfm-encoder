import { expect, test } from 'bun:test';

import '../web/waveform.js';

const { create, merge, span } = globalThis.liveWaveform;

// msg builds a server message whose points end at sequence number end.
const msg = (end, points) => ({
    end,
    points: btoa(String.fromCharCode(...points.flat().map((b) => b & 0xff))),
});

test('older points are ignored and skipped points are gaps', () => {
    const wave = create();
    merge(wave, msg(6008, Array(6000).fill([-1, 1, -1, 1, 0]))); // snapshot: 8 to 6007
    merge(wave, msg(6010, [[-3, 3, -1, 1, 0]])); // skips 6008
    merge(wave, msg(6007, [[-9, 9, -9, 9, 0]])); // stale

    expect(wave.end).toBe(6010);
    expect(span(wave, 0, 6006, 6008)).toEqual({ lo: -1, hi: 1, clip: false });
    expect(span(wave, 0, 6008, 6009)).toBeNull();
    expect(span(wave, 0, 6009, 6010)).toEqual({ lo: -3, hi: 3, clip: false });
});

test('gap points draw nothing and clipping is per channel', () => {
    const wave = create();
    merge(wave, msg(2, [[0, 0, 0, 0, 4], [-128, 127, -5, 5, 2]]));

    expect(span(wave, 0, 0, 1)).toBeNull();
    expect(span(wave, 0, -1, 2)).toEqual({ lo: -128, hi: 127, clip: false });
    expect(span(wave, 1, 0, 2)).toEqual({ lo: -5, hi: 5, clip: true });
});
