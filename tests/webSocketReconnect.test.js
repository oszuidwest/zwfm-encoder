import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const source = readFileSync(new URL('../web/app.js', import.meta.url), 'utf8');

const away = async () => { throw new TypeError('Failed to fetch'); };
const status = code => async () => ({ status: code });

// Connects, and lets the test drop the socket and answer the session check that follows.
function dashboard() {
    let init;
    let factory;
    const page = { sockets: [], visited: [], timers: [], checks: [] };
    runInNewContext(source, {
        location: { protocol: 'https:', host: 'encoder.example' },
        window: {
            location: { assign: path => page.visited.push(path) },
            liveWaveform: { create: () => ({}), draw() {} },
        },
        document: {
            documentElement: {},
            addEventListener: (type, listener) => { if (type === 'alpine:init') init = listener; },
        },
        getComputedStyle: () => ({ getPropertyValue: () => '' }),
        Alpine: { data: (_name, create) => { factory = create; } },
        WebSocket: class { constructor() { page.sockets.push(this); } },
        AbortSignal: { timeout: ms => `${ms} ms` },
        setTimeout: (callback, ms) => { page.timers.push({ callback, ms }); },
        fetch: async (url, options) => {
            page.checks.push({ url, ...options });
            return page.respond();
        },
    });
    init();
    page.app = factory();
    page.app.$refs = { vu: { querySelectorAll: () => [] } };
    page.app.encoder.state = 'running';
    page.app.connectWebSocket();
    // Runs the next timer. Its promise rejects when the code under test lets an error escape.
    page.tick = async () => {
        const { callback, ms } = page.timers.shift();
        expect(ms).toBe(1000);
        await callback();
    };
    page.drop = (respond) => {
        page.respond = respond;
        page.sockets.at(-1).onclose();
        return page.tick();
    };
    return page;
}

test('a tab without a session goes to the login page once and stops reconnecting', async () => {
    const page = dashboard();
    await page.drop(status(401));
    expect(page.visited).toEqual(['/login']);
    expect(page.sockets).toHaveLength(1);
    expect(page.timers).toEqual([]);
});

test('a tab reconnects when the server is away, failing or only dropped the socket', async () => {
    for (const respond of [away, status(502), status(200)]) {
        const page = dashboard();
        await page.drop(respond);
        await page.drop(respond);
        expect(page.visited).toEqual([]);
        expect(page.sockets).toHaveLength(3);
        expect(page.app.encoder.state).toBe('connecting');
    }
});

test('the session check is one small request with a time limit', async () => {
    const page = dashboard();
    await page.drop(away);
    expect(page.checks).toEqual([{ url: '/api/streams', method: 'HEAD', signal: '5000 ms' }]);
});

test('a slow session check holds back the next one', async () => {
    const page = dashboard();
    const { promise, resolve } = Promise.withResolvers();
    const tick = page.drop(() => promise);
    await Bun.sleep(0);
    expect(page.timers).toEqual([]);
    expect(page.checks).toHaveLength(1);
    expect(page.sockets).toHaveLength(1);
    resolve({ status: 200 });
    await tick;
    expect(page.sockets).toHaveLength(2);
});

test('a reconnected tab reloads the configuration, unless a form holds edits', async () => {
    const saved = async () => ({ status: 200, ok: true, json: async () => ({ streams: [{ id: 'added' }] }) });
    for (const [view, streams] of [['dashboard', [{ id: 'added' }]], ['settings', []]]) {
        const page = dashboard();
        page.app.configLoaded = true; // The page loaded the configuration just before.
        page.respond = saved;
        page.sockets[0].onopen();
        await page.drop(saved);
        page.app.view = view;
        page.sockets[1].onopen();
        await Bun.sleep(0);
        expect(page.app.streams).toEqual(streams);
        expect(page.checks.filter(({ url }) => url === '/api/config')).toHaveLength(streams.length);
    }
});

test('the first socket loads the configuration when the startup load failed', async () => {
    const page = dashboard();
    page.respond = async () => ({ status: 200, ok: true, json: async () => ({ streams: [{ id: 'late' }] }) });
    page.sockets[0].onopen();
    await Bun.sleep(0);
    expect(page.app.streams).toEqual([{ id: 'late' }]);
});

test('an older configuration answer cannot replace a newer one', async () => {
    const page = dashboard();
    const answers = [];
    page.respond = () => new Promise((resolve) => answers.push(resolve));
    const config = (id) => ({ status: 200, ok: true, json: async () => ({ streams: [{ id }] }) });
    const older = page.app.loadConfig();
    const newer = page.app.loadConfig();
    answers[1](config('newer'));
    await newer;
    answers[0](config('older'));
    await older;
    expect(page.app.streams).toEqual([{ id: 'newer' }]);
});
