import { expect, test } from 'bun:test';

import '../web/waveform.js';

const { create, merge, span, draw } = globalThis.liveWaveform;

// msg builds a server message whose points end at sequence number end.
const msg = (end, points) => ({
    end,
    points: btoa(String.fromCharCode(...points.flat().map((b) => b & 0xff))),
});

test('older points are ignored and skipped points are gaps', () => {
    const wave = create();
    merge(wave, msg(1008, Array(1000).fill([-1, 1, -1, 1, 0]))); // snapshot: 8 to 1007
    merge(wave, msg(1010, [[-3, 3, -1, 1, 0]])); // skips 1008
    merge(wave, msg(1007, [[-9, 9, -9, 9, 0]])); // stale

    expect(wave.end).toBe(1010);
    expect(span(wave, 0, 1006, 1008)).toEqual({ lo: -1, hi: 1, clip: false });
    expect(span(wave, 0, 1008, 1009)).toBeNull();
    expect(span(wave, 0, 1009, 1010)).toEqual({ lo: -3, hi: 3, clip: false });
});

test('gap points draw nothing and clipping is per channel', () => {
    const wave = create();
    merge(wave, msg(2, [[0, 0, 0, 0, 4], [-128, 127, -5, 5, 2]]));

    expect(span(wave, 0, 0, 1)).toBeNull();
    expect(span(wave, 0, -1, 2)).toEqual({ lo: -128, hi: 127, clip: false });
    expect(span(wave, 1, 0, 2)).toEqual({ lo: -5, hi: 5, clip: true });
});

test('draw puts the left channel on top, the newest point at the right and clipping in red', () => {
    const points = Array(1000).fill([0, 0, 0, 0, 4]); // Gaps draw nothing.
    points[0] = [0, 0, -64, 64, 0]; // Oldest: silent left, audio right.
    points[500] = [0, 0, -128, -128, 2]; // Right pinned at negative full scale.
    points[999] = [-64, 64, 0, 0, 1]; // Newest: clipped left, silent right.
    const wave = create();
    merge(wave, msg(1000, points));

    const rects = [];
    const ctx = {
        clearRect() {},
        fillRect(x, y, w, h) {
            rects.push([x, y, h, this.fillStyle]);
        },
    };
    globalThis.devicePixelRatio = 1;
    globalThis.getComputedStyle = () => ({ color: 'audio', getPropertyValue: () => 'danger' });
    try {
        draw(wave, { clientWidth: 1000, clientHeight: 8, getContext: () => ctx });
    } finally {
        delete globalThis.devicePixelRatio;
        delete globalThis.getComputedStyle;
    }

    // Lanes are centred at 2 (left) and 6 (right); full scale is 2 px, and a
    // pinned point stays on the last row of its lane.
    expect(rects).toEqual([
        [0, 2, 1, 'audio'],
        [0, 5, 2, 'audio'],
        [500, 2, 1, 'audio'],
        [500, 7, 1, 'danger'],
        [999, 1, 2, 'danger'],
        [999, 6, 1, 'audio'],
    ]);
});
