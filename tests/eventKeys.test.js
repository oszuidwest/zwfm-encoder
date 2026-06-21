import { expect, test } from 'bun:test';

import '../web/eventKeys.js';

const keys = globalThis.eventKeyBuilders;

test('event key builders match backend canonical keys', () => {
    expect(keys.streamSourceKey('s1')).toBe('stream:s1');
    expect(keys.audioSilenceSourceKey()).toBe('audio:silence');
    expect(keys.audioImbalanceSourceKey()).toBe('audio:imbalance');
    expect(keys.recorderStatusSourceKey('hourly')).toBe('recorder:hourly:');
    expect(keys.recorderUploadSourceKey('hourly')).toBe('recorder-upload:hourly');
});
