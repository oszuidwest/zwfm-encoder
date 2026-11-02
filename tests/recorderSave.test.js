import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const source = readFileSync(new URL('../web/app.js', import.meta.url), 'utf8');

// Saves an edit of a recorder shown as `state` and returns the state the dashboard shows right after.
async function stateAfterSave(state, enabled) {
    let init;
    let factory;
    runInNewContext(source, {
        URL,
        window: { location: { href: 'https://encoder.example/', origin: 'https://encoder.example' } },
        document: {
            documentElement: {},
            addEventListener: (type, listener) => { if (type === 'alpine:init') init = listener; },
            querySelector: () => ({ content: 'token' }),
        },
        getComputedStyle: () => ({ getPropertyValue: () => '' }),
        Alpine: { data: (_name, create) => { factory = create; } },
        fetch: async () => ({ ok: true, json: async () => ({ id: 'rec', enabled }) }),
    });
    init();
    const app = factory();
    Object.assign(app, { loadConfig() {}, showDashboard() {}, showToast() {}, showRequestError(err) { throw err; } });
    app.recorders = [{ id: 'rec', name: 'Studio', enabled: true, recording_mode: 'hourly', storage_mode: 'local', local_path: '/rec' }];
    app.recorderStatuses = { rec: { state } };
    app.showRecorderForm('rec');
    app.recorderForm.enabled = enabled;
    await app.submitRecorderForm();
    return app.recorderStatuses.rec.state;
}

test('saving an enabled recorder keeps its state until the next status push', async () => {
    expect(await stateAfterSave('running', true)).toBe('running');
});

test('disabling a recorder shows it as disabled right away', async () => {
    expect(await stateAfterSave('running', false)).toBe('disabled');
});
