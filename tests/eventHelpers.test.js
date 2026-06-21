import { expect, test } from 'bun:test';

import '../web/eventHelpers.js';

const helpers = globalThis.eventUIHelpers;

test('duration helper formats compact display values', () => {
    expect(helpers.formatSmartDuration(950)).toBe('950ms');
    expect(helpers.formatSmartDuration(1500)).toBe('1.5s');
    expect(helpers.formatSmartDuration(12_400)).toBe('12s');
    expect(helpers.formatSmartDuration(90_000)).toBe('1m 30s');
});

test('event window helpers clamp at the API read limit', () => {
    expect(helpers.EVENT_MAX_READ_LIMIT).toBe(500);
    expect(helpers.nextEventWindowSize(50, 50)).toBe(100);
    expect(helpers.nextEventWindowSize(450, 50)).toBe(500);
    expect(helpers.nextEventWindowSize(499, 50)).toBe(500);
    expect(helpers.nextEventWindowSize(500, 50)).toBe(500);
});
