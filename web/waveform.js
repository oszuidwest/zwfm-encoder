(function initWaveform(root) {
    // Keep in sync with internal/audio/waveform.go.
    const POINTS = 1000;
    const POINT_SIZE = 5;
    const GAP = 4;
    const GAP_POINT = Int8Array.of(0, 0, 0, 0, GAP);

    const create = () => ({ end: 0, data: new Int8Array(POINTS * POINT_SIZE) });

    // Points before wave.end are already here; points a message skips become gaps.
    const merge = (wave, msg) => {
        const bytes = Int8Array.from(atob(msg.points), (c) => c.charCodeAt(0));
        const first = msg.end - bytes.length / POINT_SIZE;
        for (let seq = Math.max(wave.end, msg.end - POINTS); seq < msg.end; seq++) {
            const i = (seq - first) * POINT_SIZE;
            wave.data.set(i < 0 ? GAP_POINT : bytes.subarray(i, i + POINT_SIZE), (seq % POINTS) * POINT_SIZE);
        }
        wave.end = Math.max(wave.end, msg.end);
    };

    // Returns min, max and clip over channel (0 left, 1 right) of points
    // [from, to), or null when none of them holds audio.
    const span = (wave, channel, from, to) => {
        let lo = 127;
        let hi = -128;
        let clip = false;
        let found = false;
        for (let seq = Math.max(from, 0); seq < to; seq++) {
            const off = (seq % POINTS) * POINT_SIZE;
            if (wave.data[off + 4] & GAP) continue;
            found = true;
            lo = Math.min(lo, wave.data[off + channel * 2]);
            hi = Math.max(hi, wave.data[off + channel * 2 + 1]);
            clip ||= (wave.data[off + 4] & (1 << channel)) !== 0; // Flags 1 and 2: clip left, right.
        }
        return found ? { lo, hi, clip } : null;
    };

    // Draws one channel (0 left, 1 right) of the whole ring, newest at the
    // right. Each pixel column takes the min and max of its points, so short
    // peaks survive on a narrow screen.
    const draw = (wave, canvas, channel) => {
        const width = Math.round(canvas.clientWidth * devicePixelRatio);
        const height = Math.round(canvas.clientHeight * devicePixelRatio);
        if (canvas.width !== width) canvas.width = width;
        if (canvas.height !== height) canvas.height = height;
        if (width === 0 || height === 0) return;

        const ctx = canvas.getContext('2d');
        ctx.clearRect(0, 0, width, height);
        const style = getComputedStyle(canvas);
        const colors = [style.color, style.getPropertyValue('--danger')];
        const center = height / 2;
        const scale = center / 128;
        const start = wave.end - POINTS;
        for (let x = 0; x < width; x++) {
            const from = start + Math.floor((x * POINTS) / width);
            const to = Math.max(start + Math.floor(((x + 1) * POINTS) / width), from + 1);
            const s = span(wave, channel, from, to);
            if (!s) continue;
            ctx.fillStyle = colors[s.clip ? 1 : 0];
            // A point pinned at -128 would start on the bottom edge; keep it in.
            const top = Math.min(center - s.hi * scale, height - 1);
            ctx.fillRect(x, top, 1, Math.max((s.hi - s.lo) * scale, 1));
        }
    };

    root.liveWaveform = { create, merge, span, draw };
})(globalThis);
