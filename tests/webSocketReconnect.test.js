import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const source = readFileSync(new URL('../web/app.js', import.meta.url), 'utf8');

// Drops the socket while the API gives `respond`, and returns what the dashboard does next.
async function afterDisconnect(respond) {
    let init;
    let factory;
    const sockets = [];
    const visited = [];
    const timers = [];
    runInNewContext(source, {
        location: { protocol: 'https:', host: 'encoder.example' },
        window: { location: { assign: path => visited.push(path) } },
        document: {
            documentElement: {},
            addEventListener: (type, listener) => { if (type === 'alpine:init') init = listener; },
        },
        getComputedStyle: () => ({ getPropertyValue: () => '' }),
        Alpine: { data: (_name, create) => { factory = create; } },
        WebSocket: class { constructor() { sockets.push(this); } },
        setTimeout: callback => { timers.push(callback); },
        AbortSignal,
        fetch: (url, { signal }) => {
            expect(url).toBe('/api/streams');
            expect(signal).toBeInstanceOf(AbortSignal); // A hung check must not pile up.
            return respond();
        },
    });
    init();
    const app = factory();
    app.encoder.state = 'running';
    app.connectWebSocket();
    sockets[0].onclose();
    await Bun.sleep(0);
    for (const callback of timers) callback();
    return { visited, sockets: sockets.length, state: app.encoder.state };
}

test('a tab without a session goes to the login page', async () => {
    const { visited } = await afterDisconnect(async () => ({ status: 401 }));
    expect(visited).toEqual(['/login']);
});

test('a tab reconnects when the server is away, failing, slow or only dropped the socket', async () => {
    const responses = {
        away: async () => { throw new TypeError('Failed to fetch'); },
        failing: async () => ({ status: 502 }),
        slow: () => new Promise(() => {}),
        fine: async () => ({ status: 200 }),
    };
    for (const respond of Object.values(responses)) {
        expect(await afterDisconnect(respond)).toEqual({ visited: [], sockets: 2, state: 'connecting' });
    }
});
