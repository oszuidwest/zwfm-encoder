import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const source = readFileSync(new URL('../web/app.js', import.meta.url), 'utf8');

function requestHarness(token = 'session-bound-token', response = { ok: true, json: async () => ({ saved: true }) }) {
    const calls = [];
    const context = {
        URL,
        window: { location: { href: 'https://encoder.example/', origin: 'https://encoder.example' } },
        document: {
            documentElement: {},
            addEventListener() {},
            querySelector(selector) {
                expect(selector).toBe('meta[name="csrf-token"]');
                return token ? { content: token } : null;
            },
        },
        getComputedStyle: () => ({ getPropertyValue: () => '' }),
        fetch: async (url, options) => {
            calls.push({ url, options });
            return response;
        },
    };
    return { calls, request: runInNewContext(`${source}\nrequestJSON;`, context) };
}

test('every mutating request sends the session CSRF header, even without JSON', async () => {
    const { calls, request } = requestHarness();
    for (const method of ['POST', 'PUT', 'PATCH', 'DELETE']) {
        await request('/api/settings', { method, body: { name: 'station' } });
        await request('/logout', { method: method.toLowerCase() });
    }
    expect(calls).toHaveLength(8);
    for (const { options } of calls) {
        expect(options.headers['X-CSRF-Token']).toBe('session-bound-token');
        if (options.body) {
            expect(options.headers['Content-Type']).toBe('application/json');
            expect(JSON.parse(options.body)).toEqual({ name: 'station' });
        }
    }
});

test('safe reads do not need a CSRF token', async () => {
    const { calls, request } = requestHarness(null);
    for (const method of ['GET', 'HEAD', 'OPTIONS', 'get']) await request('/api/config', { method });
    expect(calls).toHaveLength(4);
    for (const { options } of calls) expect(options.headers).toBeUndefined();
});

test('missing token and foreign targets fail before fetching or disclosing a token', async () => {
    const missing = requestHarness(null);
    await expect(missing.request('/api/settings', { method: 'POST' })).rejects.toThrow('Reload this page');
    expect(missing.calls).toHaveLength(0);
    const foreign = requestHarness();
    for (const url of ['https://attacker.example/api', '//attacker.example/api']) {
        await expect(foreign.request(url, { method: 'POST' })).rejects.toThrow('same-origin');
    }
    expect(foreign.calls).toHaveLength(0);
});

test('HTTP errors expose their status so sign-out can treat an expired session as signed out', async () => {
    const { request } = requestHarness('token', { ok: false, status: 401, json: async () => ({ error: 'Session expired; reload this page' }) });
    const error = await request('/logout', { method: 'POST' }).catch(caught => caught);
    expect(error.message).toBe('Session expired; reload this page');
    expect(error.status).toBe(401);
});
