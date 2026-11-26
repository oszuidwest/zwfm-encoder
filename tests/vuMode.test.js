import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const source = readFileSync(new URL('../web/app.js', import.meta.url), 'utf8');

// appState runs app.js and returns the Alpine data of the dashboard.
function appState(stored) {
    const storage = new Map(stored ? [['vuMode', stored]] : []);
    let init;
    const context = {
        URL,
        window: { location: { href: 'https://encoder.example/', origin: 'https://encoder.example' } },
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
    return { state: factory(), storage };
}

test('the mode button cycles peak, RMS and the waveform, and remembers the mode', () => {
    const { state, storage } = appState();
    const seen = [state.vuModeLabels[state.vuMode]];
    for (let i = 0; i < 3; i++) {
        state.toggleVuMode();
        seen.push(state.vuModeLabels[state.vuMode]);
    }
    expect(seen).toEqual(['Peak', 'RMS', '10 s', 'Peak']);
    expect(storage.get('vuMode')).toBe('peak');

    state.toggleVuMode();
    state.toggleVuMode();
    expect(appState(storage.get('vuMode')).state.vuMode).toBe('wave');
});
