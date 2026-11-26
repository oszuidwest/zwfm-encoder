import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const source = readFileSync(new URL('../web/app.js', import.meta.url), 'utf8');

// appState runs app.js and returns the Alpine data of the dashboard, the
// sockets it opens, and the waveforms it creates and draws.
function appState(stored) {
    const storage = new Map(stored ? [['vuMode', stored]] : []);
    const sockets = [];
    const waves = [];
    const drawn = [];
    let init;
    const context = {
        URL,
        location: { protocol: 'https:', host: 'encoder.example' },
        WebSocket: class {
            constructor() {
                sockets.push(this);
            }
        },
        setTimeout: (fn) => fn(),
        window: {
            location: { href: 'https://encoder.example/', origin: 'https://encoder.example' },
            liveWaveform: {
                create: () => waves[waves.push({ id: waves.length }) - 1],
                merge() {},
                draw: (wave, _canvas, channel) => drawn.push([wave, channel]),
            },
        },
        document: {
            documentElement: {},
            addEventListener(type, listener) {
                if (type === 'alpine:init') init = listener;
            },
        },
        getComputedStyle: () => ({ getPropertyValue: () => '' }),
        localStorage: {
            getItem: (key) => storage.get(key) ?? null,
            setItem: (key, value) => storage.set(key, value),
        },
    };
    runInNewContext(source, context);
    let factory;
    context.Alpine = { data: (_name, fn) => { factory = fn; } };
    init();
    return { state: factory(), storage, sockets, waves, drawn };
}

test('the mode button cycles peak, RMS and the waveform, and remembers the mode', () => {
    const { state, storage } = appState();
    const seen = [state.vuModeLabels[state.vuMode]];
    for (let i = 0; i < 3; i++) {
        state.toggleVuMode();
        seen.push(state.vuModeLabels[state.vuMode]);
    }
    expect(seen).toEqual(['Peak', 'RMS', 'Wave', 'Peak']);
    expect(storage.get('vuMode')).toBe('peak');

    state.toggleVuMode();
    state.toggleVuMode();
    expect(appState(storage.get('vuMode')).state.vuMode).toBe('wave');
});

test('a closed socket clears the waveform, because the server numbers its points again', () => {
    const { state, sockets, waves, drawn } = appState();
    state.$refs = { vu: { querySelectorAll: () => [{}, {}] } };
    const levels = { data: JSON.stringify({ type: 'levels', levels: {}, end: 0, points: '' }) };

    state.connectWebSocket();
    sockets[0].onmessage(levels);
    sockets[0].onmessage(levels);
    expect(waves).toHaveLength(1);

    sockets[0].onclose();
    // Both tracks get the empty waveform, so the old trace is gone.
    expect(drawn.slice(-2)).toEqual([[waves[1], 0], [waves[1], 1]]);
    sockets[1].onmessage(levels);
    expect(waves).toHaveLength(2);
});
